package retrieval_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"linkweft/internal/retrieval"
)

type searchFunc func(context.Context, retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error)

func (f searchFunc) Search(ctx context.Context, request retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
	return f(ctx, request)
}

func mockProvider(search searchFunc) retrieval.SearchProvider {
	return retrieval.SearchProvider{Searcher: search, Timeout: 5 * time.Second, MaxResults: 10}
}

func TestSearchExplicitSubset(t *testing.T) {
	var calls []string
	registry := make(map[string]retrieval.SearchProvider)
	all := []string{"tavily", "exa", "keenable", "firecrawl", "anysearch", "tinyfish"}
	for _, name := range all {
		name := name
		registry[name] = mockProvider(func(context.Context, retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
			calls = append(calls, name)
			return nil, &retrieval.Error{Code: "upstream_error", Message: "provider failed"}
		})
	}
	for _, tc := range []struct {
		name     string
		selected []string
		explicit bool
	}{
		{"all configured candidates", all, false},
		{"explicit subset", []string{"tinyfish", "exa"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls = nil
			response, exit := retrieval.Search(context.Background(), retrieval.SearchRequest{Limit: 10},
				retrieval.SearchOptions{Timeout: time.Second}, tc.selected, tc.explicit, registry, nil)
			if exit != 1 || response.Outcome != "error" || response.Error == nil || response.Error.Code != "search_failed" || len(response.Results) != 0 {
				t.Fatalf("exit = %d, response = %+v", exit, response)
			}
			if !slices.Equal(calls, tc.selected) || len(response.Executions) != len(tc.selected) {
				t.Fatalf("calls = %v, executions = %+v, want %v", calls, response.Executions, tc.selected)
			}
			for i, execution := range response.Executions {
				if execution.Provider != tc.selected[i] || execution.Status != "error" || execution.DurationMS == nil ||
					execution.Reason != nil || execution.Error == nil || execution.Error.Code != "upstream_error" {
					t.Errorf("unexpected execution: %+v", execution)
				}
			}
		})
	}
}

func TestSearchSkipsUnsupportedCapability(t *testing.T) {
	const resultURL = "https://example.org/result"
	var calls []string
	registry := map[string]retrieval.SearchProvider{
		"exa": mockProvider(func(context.Context, retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
			calls = append(calls, "exa")
			return []retrieval.Candidate{{URL: resultURL, Rank: 1}}, nil
		}),
		"tavily": mockProvider(func(context.Context, retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
			calls = append(calls, "tavily")
			return nil, nil
		}),
	}
	request := retrieval.SearchRequest{Limit: 10}
	t.Run("AC33 automatic skips unsupported and stops after success", func(t *testing.T) {
		response, exit := retrieval.Search(context.Background(), request, retrieval.SearchOptions{Timeout: time.Second}, []string{"firecrawl", "exa", "tavily"}, false, registry, nil)
		if exit != 0 || response.Outcome != "success" || response.Error != nil || len(response.Results) != 1 || response.Results[0].URL != resultURL {
			t.Fatalf("exit = %d, response = %+v", exit, response)
		}
		if !slices.Equal(calls, []string{"exa"}) || len(response.Executions) != 3 {
			t.Fatalf("calls = %v, executions = %+v", calls, response.Executions)
		}
		assertUncalled(t, response.Executions[0], "firecrawl", "skipped", "unsupported_capability")
		if execution := response.Executions[1]; execution.Provider != "exa" || execution.Status != "success" ||
			execution.DurationMS == nil || execution.Reason != nil || execution.Error != nil {
			t.Errorf("unexpected successful execution: %+v", execution)
		}
		assertUncalled(t, response.Executions[2], "tavily", "not_started", "fallback_satisfied")
	})
	t.Run("AC06 explicit unsupported prevents any request", func(t *testing.T) {
		calls = nil
		response, exit := retrieval.Search(context.Background(), request, retrieval.SearchOptions{Timeout: time.Second}, []string{"exa", "firecrawl"}, true, registry, nil)
		if exit != 2 || response.Outcome != "error" || response.Error == nil || response.Error.Code != "unsupported_capability" ||
			len(response.Results) != 0 || len(response.Executions) != 0 || len(calls) != 0 {
			t.Fatalf("exit = %d, response = %+v, calls = %v", exit, response, calls)
		}
	})
}

func TestSearchSharesDeadlineAcrossProviders(t *testing.T) {
	var firstDeadline time.Time
	var calls []string
	registry := map[string]retrieval.SearchProvider{
		"tavily": mockProvider(func(ctx context.Context, _ retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
			calls = append(calls, "tavily")
			firstDeadline, _ = ctx.Deadline()
			select {
			case <-time.After(350 * time.Millisecond):
			case <-ctx.Done():
			}
			return nil, nil
		}),
		"exa": mockProvider(func(ctx context.Context, _ retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
			calls = append(calls, "exa")
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.Equal(firstDeadline) {
				t.Errorf("second deadline = %v, want shared deadline %v", deadline, firstDeadline)
				return nil, nil
			}
			<-ctx.Done()
			return nil, nil
		}),
		"keenable": mockProvider(func(context.Context, retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
			calls = append(calls, "keenable")
			return nil, nil
		}),
	}
	request := retrieval.SearchRequest{Limit: 10}
	response, exit := retrieval.Search(context.Background(), request, retrieval.SearchOptions{Timeout: 600 * time.Millisecond}, []string{"tavily", "exa", "keenable"}, false, registry, nil)
	if exit != 1 || response.Outcome != "error" || response.Error == nil || response.Error.Code != "search_failed" || len(response.Results) != 0 {
		t.Fatalf("exit = %d, response = %+v", exit, response)
	}
	if !slices.Equal(calls, []string{"tavily", "exa"}) || len(response.Executions) != 3 {
		t.Fatalf("calls = %v, executions = %+v", calls, response.Executions)
	}
	if execution := response.Executions[0]; execution.Provider != "tavily" || execution.Status != "empty" ||
		execution.DurationMS == nil || execution.Reason != nil || execution.Error != nil {
		t.Errorf("unexpected first execution: %+v", execution)
	}
	if execution := response.Executions[1]; execution.Provider != "exa" || execution.Status != "timeout" ||
		execution.DurationMS == nil || execution.Reason == nil || *execution.Reason != "command_timeout" ||
		execution.Error == nil || execution.Error.Code != "command_timeout" {
		t.Errorf("unexpected second execution: %+v", execution)
	}
	assertUncalled(t, response.Executions[2], "keenable", "not_started", "command_timeout")
}

func assertUncalled(t *testing.T, execution retrieval.Execution, provider, status, reason string) {
	t.Helper()
	if execution.Provider != provider || execution.Status != status || execution.Reason == nil || *execution.Reason != reason ||
		execution.DurationMS != nil || execution.Error != nil {
		t.Errorf("unexpected uncalled execution: %+v", execution)
	}
}
