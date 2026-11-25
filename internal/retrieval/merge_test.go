package retrieval_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"linkweft/internal/retrieval"
)

type mergeReply struct {
	response retrieval.Response
	exit     int
}

func startMerge(ctx context.Context, options retrieval.SearchOptions, names []string, registry map[string]retrieval.SearchProvider) <-chan mergeReply {
	reply := make(chan mergeReply, 1)
	go func() {
		response, exit := retrieval.Search(ctx, retrieval.SearchRequest{Limit: 10}, options, names, false, registry, nil)
		reply <- mergeReply{response, exit}
	}()
	return reply
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test event")
		var zero T
		return zero
	}
}

func staticProvider(candidates ...retrieval.Candidate) retrieval.SearchProvider {
	return mockProvider(func(context.Context, retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
		return candidates, nil
	})
}

func TestMergeURLKey(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"HTTP://Example.ORG:80/a%2Fb?b=2&a=1#Pass", "http://example.org/a%2Fb?b=2&a=1#Pass", true},
		{"HTTPS://Example.ORG:443/a", "https://example.org/a", true},
		{"HTTP://[2001:DB8::1]:80/a", "http://[2001:db8::1]/a", true},
		{"HTTP://[FE80::1%25ETH0]:80/a", "http://[fe80::1%25eth0]/a", false},
		{"https://example.org:80/a", "https://example.org/a", false},
		{"http://example.org/a", "https://example.org/a", false},
		{"https://example.org/a", "https://example.org/A", false},
		{"https://example.org/a%2fb", "https://example.org/a%2Fb", false},
		{"https://example.org/a/../b", "https://example.org/b", false},
		{"https://example.org/a?x=1&y=2", "https://example.org/a?y=2&x=1", false},
		{"https://example.org/a?x=1", "https://example.org/a?x=2", false},
		{"https://example.org/a?utm_source=x", "https://example.org/a", false},
		{"https://example.org/a#A", "https://example.org/a#a", false},
		{"https://example.org/a#", "https://example.org/a", false},
		{"https://example.org/a?", "https://example.org/a", false},
		{"https://example.org", "https://example.org/", false},
		{"https://User@example.org/a", "https://user@example.org/a", false},
	} {
		t.Run(tc.a+" and "+tc.b, func(t *testing.T) {
			registry := map[string]retrieval.SearchProvider{
				"a": staticProvider(retrieval.Candidate{URL: tc.a, Rank: 1}),
				"b": staticProvider(retrieval.Candidate{URL: tc.b, Rank: 1}),
			}
			reply := await(t, startMerge(context.Background(), retrieval.SearchOptions{Timeout: time.Second, Strategy: "merge", MaxConcurrency: 2}, []string{"a", "b"}, registry))
			want := []retrieval.Result{{URL: tc.a, Sources: []retrieval.Source{{Provider: "a", URL: tc.a, Rank: 1}}}}
			b := retrieval.Source{Provider: "b", URL: tc.b, Rank: 1}
			if tc.same {
				want[0].Sources = append(want[0].Sources, b)
			} else {
				want = append(want, retrieval.Result{URL: tc.b, Sources: []retrieval.Source{b}})
			}
			if reply.exit != 0 || reply.response.Outcome != "success" || !reflect.DeepEqual(reply.response.Results, want) {
				t.Fatalf("exit = %d, response = %+v, want results %+v", reply.exit, reply.response, want)
			}
		})
	}
}

func TestMergeCountsBestOriginalRankOnce(t *testing.T) {
	const x, y = "https://example.org/x", "https://example.org/y"
	registry := map[string]retrieval.SearchProvider{
		"a": staticProvider(
			retrieval.Candidate{URL: x, Rank: 1}, retrieval.Candidate{URL: "HTTPS://EXAMPLE.ORG:443/x", Rank: 2},
			retrieval.Candidate{URL: y, Rank: 2},
		),
		"b": staticProvider(
			retrieval.Candidate{URL: y, Rank: 2}, retrieval.Candidate{URL: x, Rank: 3},
		),
	}
	reply := await(t, startMerge(context.Background(), retrieval.SearchOptions{Timeout: time.Second, Strategy: "merge", MaxConcurrency: 2}, []string{"a", "b"}, registry))
	// 1/61 + 1/63 beats 2/62, which in turn beats using X's rank 2 in place of rank 1.
	want := []retrieval.Result{
		{URL: x, Sources: []retrieval.Source{{Provider: "a", URL: x, Rank: 1}, {Provider: "b", URL: x, Rank: 3}}},
		{URL: y, Sources: []retrieval.Source{{Provider: "a", URL: y, Rank: 2}, {Provider: "b", URL: y, Rank: 2}}},
	}
	if !reflect.DeepEqual(reply.response.Results, want) {
		t.Fatalf("results = %+v, want %+v", reply.response.Results, want)
	}
	// Give Y the same contributions as X. Lexical order then separates the results;
	// an extra contribution from A's second copy of X would incorrectly put X first.
	const earlierY = "https://example.org/a"
	registry["a"] = staticProvider(retrieval.Candidate{URL: x, Rank: 1}, retrieval.Candidate{URL: x, Rank: 2}, retrieval.Candidate{URL: earlierY, Rank: 3})
	registry["b"] = staticProvider(retrieval.Candidate{URL: earlierY, Rank: 1}, retrieval.Candidate{URL: x, Rank: 3})
	reply = await(t, startMerge(context.Background(), retrieval.SearchOptions{Timeout: time.Second, Strategy: "merge", MaxConcurrency: 2}, []string{"a", "b"}, registry))
	if results := reply.response.Results; len(results) != 2 || results[0].URL != earlierY || !reflect.DeepEqual(results[1], want[0]) {
		t.Fatalf("expected Y before X at the same score, got %+v", results)
	}
}

func TestMergeComparesRRFScoresExactly(t *testing.T) {
	const earlier, later = "https://example.org/a", "https://example.org/z"
	for _, tc := range []struct {
		name string
		a, b []retrieval.Candidate
	}{
		{
			name: "exact tie goes to best original rank",
			// 1/62 = 1/93 + 1/186.
			a: []retrieval.Candidate{{URL: later, Rank: 2}, {URL: earlier, Rank: 33}},
			b: []retrieval.Candidate{{URL: earlier, Rank: 126}},
		},
		{
			name: "distinct scores below float64 precision",
			a:    []retrieval.Candidate{{URL: earlier, Rank: 1}, {URL: later, Rank: 1}},
			b:    []retrieval.Candidate{{URL: later, Rank: 1 << 30}, {URL: earlier, Rank: (1 << 30) + 1}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := map[string]retrieval.SearchProvider{"a": staticProvider(tc.a...), "b": staticProvider(tc.b...)}
			reply := await(t, startMerge(context.Background(), retrieval.SearchOptions{Timeout: time.Second, Strategy: "merge", MaxConcurrency: 2}, []string{"a", "b"}, registry))
			if results := reply.response.Results; len(results) != 2 || results[0].URL != later || results[1].URL != earlier {
				t.Fatalf("expected %s before %s, got %+v", later, earlier, results)
			}
		})
	}
}

func TestMergeTiesAndFieldsIndependentOfCompletionOrder(t *testing.T) {
	const x, y = "https://example.org/x", "https://example.org/y"
	const ax, ay = "HTTPS://EXAMPLE.org:443/x", "HTTPS://EXAMPLE.org/y"
	title, snippet, other := "title from B", "snippet from A's second matching candidate", "lower priority field"
	content := retrieval.Content{Text: "body from B", Format: "text", Freshness: "unknown"}
	bodies := map[string][]retrieval.Candidate{
		"a": {{URL: ay, Rank: 1}, {URL: ax, Rank: 2}, {URL: x, Rank: 2, Snippet: &snippet}, {URL: x, Rank: 3, Snippet: &other}},
		"b": {{URL: x, Rank: 1, Title: &title, Snippet: &other, Content: &content}, {URL: y, Rank: 2}},
	}
	for _, first := range []string{"a", "b"} {
		t.Run(first+" completes first", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan string, 3)
			gates := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
			registry := make(map[string]retrieval.SearchProvider)
			for _, name := range []string{"a", "b"} {
				name := name
				registry[name] = mockProvider(func(ctx context.Context, _ retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
					started <- name
					select {
					case <-gates[name]:
					case <-ctx.Done():
					}
					return bodies[name], nil
				})
			}
			registry["c"] = mockProvider(func(context.Context, retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
				started <- "c"
				return nil, nil
			})
			pending := startMerge(ctx, retrieval.SearchOptions{Timeout: 4 * time.Second, Strategy: "merge", MaxConcurrency: 2}, []string{"a", "b", "c"}, registry)
			if a, b := await(t, started), await(t, started); a == b || a == "c" || b == "c" {
				t.Fatalf("first slots = %q, %q", a, b)
			}
			close(gates[first])
			if name := await(t, started); name != "c" {
				t.Fatalf("next slot = %q, want c", name)
			}
			for name, gate := range gates {
				if name != first {
					close(gate)
				}
			}
			reply := await(t, pending)
			wantContent := content
			wantContent.Provider = "b"
			want := []retrieval.Result{
				{URL: ax, Title: &title, Snippet: &snippet, Content: &wantContent, Sources: []retrieval.Source{{Provider: "a", URL: ax, Rank: 2}, {Provider: "b", URL: x, Rank: 1}}},
				{URL: ay, Sources: []retrieval.Source{{Provider: "a", URL: ay, Rank: 1}, {Provider: "b", URL: y, Rank: 2}}},
			}
			if reply.exit != 0 || reply.response.Outcome != "success" || !reflect.DeepEqual(reply.response.Results, want) {
				t.Fatalf("unexpected merge reply: %+v", reply)
			}
			if content.Provider != "" {
				t.Fatal("merge modified provider-owned content")
			}
		})
	}
}

func TestMergeAppliesLocalAndUpstreamLimits(t *testing.T) {
	const a, b, c = "https://example.org/a", "https://example.org/b", "https://example.org/c"
	requested := make(chan int, 2)
	limited := mockProvider(func(_ context.Context, request retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
		requested <- request.Limit
		return []retrieval.Candidate{{URL: a, Rank: 1}, {URL: b, Rank: 2}, {URL: c, Rank: 3}}, nil
	})
	limited.MaxResults = 1
	unlimited := mockProvider(func(_ context.Context, request retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
		requested <- request.Limit
		return []retrieval.Candidate{{URL: "/invalid", Rank: 1}, {URL: b, Rank: 2}, {URL: c, Rank: 3}, {URL: a, Rank: 4}}, nil
	})
	unlimited.MaxResults = 0
	registry := map[string]retrieval.SearchProvider{"a": limited, "b": unlimited}
	response, exit := retrieval.Search(context.Background(), retrieval.SearchRequest{Limit: 2}, retrieval.SearchOptions{Timeout: time.Second, Strategy: "merge", MaxConcurrency: 1}, []string{"a", "b"}, false, registry, nil)
	if exit != 0 || response.Outcome != "success" {
		t.Fatalf("exit = %d, response = %+v", exit, response)
	}
	if a, b := await(t, requested), await(t, requested); a != 1 || b != 0 || len(requested) != 0 {
		t.Fatalf("limits sent = %d, %d, want 1, 0 once each", a, b)
	}
	if len(response.Executions) != 2 {
		t.Fatalf("executions = %+v, want two", response.Executions)
	}
	for i, name := range []string{"a", "b"} {
		if execution := response.Executions[i]; execution.Provider != name || execution.Status != "success" ||
			execution.DurationMS == nil || execution.Reason != nil || execution.Error != nil {
			t.Errorf("unexpected execution: %+v", execution)
		}
	}
	want := []retrieval.Result{
		{URL: b, Sources: []retrieval.Source{{Provider: "a", URL: b, Rank: 2}, {Provider: "b", URL: b, Rank: 2}}},
		{URL: a, Sources: []retrieval.Source{{Provider: "a", URL: a, Rank: 1}}},
	}
	if !reflect.DeepEqual(response.Results, want) {
		t.Fatalf("results = %+v, want %+v", response.Results, want)
	}
	if got := response.Warnings; len(got) != 3 || got[0].Code != "upstream_limit_applied" || *got[0].Provider != "a" ||
		got[1].Code != "upstream_limit_unavailable" || *got[1].Provider != "b" || got[2].Code != "invalid_results_dropped" || *got[2].Provider != "b" {
		t.Errorf("warnings = %+v", got)
	}
}

func TestMergeStopsWithCollectedResults(t *testing.T) {
	for _, tc := range []struct {
		name, reason, status, outcome string
		budget                        time.Duration
		exit                          int
	}{
		{"AC34 user cancellation first", "user_cancelled", "cancelled", "cancelled", 4 * time.Second, 130},
		{"AC35 command timeout first", "command_timeout", "timeout", "partial", 350 * time.Millisecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			started := make(chan string, 3)
			requestDone := make(chan error, 1)
			late := make(chan struct{})
			unwound := make(chan struct{})
			defer func() {
				if late != nil {
					close(late)
				}
			}()
			var calls [3]atomic.Int32
			registry := map[string]retrieval.SearchProvider{
				"a": mockProvider(func(context.Context, retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
					calls[0].Add(1)
					started <- "a"
					return []retrieval.Candidate{{URL: "https://example.org/a", Rank: 1}}, nil
				}),
				"b": mockProvider(func(ctx context.Context, _ retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
					defer close(unwound)
					calls[1].Add(1)
					started <- "b"
					<-ctx.Done()
					requestDone <- ctx.Err()
					<-late
					return []retrieval.Candidate{{URL: "https://example.org/late", Rank: 1}}, nil
				}),
				"c": mockProvider(func(context.Context, retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
					calls[2].Add(1)
					started <- "c"
					return nil, nil
				}),
			}
			pending := startMerge(ctx, retrieval.SearchOptions{Timeout: tc.budget, Strategy: "merge", MaxConcurrency: 1}, []string{"a", "b", "c"}, registry)
			if a, b := await(t, started), await(t, started); a != "a" || b != "b" {
				t.Fatalf("first calls = %q, %q", a, b)
			}
			if tc.exit == 130 {
				cancel(retrieval.ErrUserCancelled)
			}
			wantContextErr := context.DeadlineExceeded
			if tc.exit == 130 {
				wantContextErr = context.Canceled
			}
			if err := await(t, requestDone); err != wantContextErr {
				t.Fatalf("in-flight context error = %v, want %v", err, wantContextErr)
			}
			cancel(retrieval.ErrUserCancelled)
			reply := await(t, pending)
			response := reply.response
			if reply.exit != tc.exit || response.Outcome != tc.outcome || len(response.Results) != 1 || response.Results[0].URL != "https://example.org/a" {
				t.Fatalf("unexpected interrupted reply: %+v", reply)
			}
			if (tc.exit == 130 && (response.Error == nil || response.Error.Code != tc.reason)) || (tc.exit == 0 && response.Error != nil) {
				t.Errorf("overall error = %+v", response.Error)
			}
			if a := response.Executions[0]; a.Provider != "a" || a.Status != "success" || a.DurationMS == nil || a.Reason != nil || a.Error != nil {
				t.Errorf("A execution = %+v", a)
			}
			if b := response.Executions[1]; b.Provider != "b" || b.Status != tc.status || b.DurationMS == nil || *b.DurationMS < 0 ||
				b.Reason == nil || *b.Reason != tc.reason || b.Error == nil || b.Error.Code != tc.reason {
				t.Errorf("B execution = %+v", b)
			}
			assertUncalled(t, response.Executions[2], "c", "not_started", tc.reason)
			before, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			close(late)
			await(t, unwound)
			late = nil
			after, err := json.Marshal(response)
			if err != nil || string(before) != string(after) {
				t.Fatalf("response changed after late result: %s → %s (%v)", before, after, err)
			}
			if a, b, c := calls[0].Load(), calls[1].Load(), calls[2].Load(); a != 1 || b != 1 || c != 0 {
				t.Errorf("calls = %d, %d, %d, want 1, 1, 0", a, b, c)
			}
		})
	}
}

func TestMergeUsesSlotStartForProviderDeadline(t *testing.T) {
	const providerTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := make(chan struct{})
	type event struct {
		name     string
		deadline time.Time
		start    time.Time
	}
	started := make(chan event, 4)
	var active atomic.Int32
	var calls [4]atomic.Int32
	registry := make(map[string]retrieval.SearchProvider)
	names := []string{"a", "b", "c", "d"}
	for i, name := range names {
		i, name := i, name
		entry := mockProvider(func(ctx context.Context, _ retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
			calls[i].Add(1)
			if count := active.Add(1); count > 2 {
				t.Errorf("%d requests in flight, want at most 2", count)
			}
			defer active.Add(-1)
			deadline, _ := ctx.Deadline()
			started <- event{name, deadline, time.Now()}
			switch name {
			case "a", "c":
				select {
				case <-gate:
				case <-ctx.Done():
				}
			case "b":
				<-ctx.Done()
			}
			return nil, nil
		})
		if name == "b" || name == "d" {
			entry.Timeout = providerTimeout
		}
		registry[name] = entry
	}
	pending := startMerge(ctx, retrieval.SearchOptions{Timeout: 4 * time.Second, Strategy: "merge", MaxConcurrency: 2}, names, registry)
	a, b := await(t, started), await(t, started)
	if a.name == "b" {
		a, b = b, a
	}
	if a.name != "a" || b.name != "b" {
		t.Fatalf("first two calls = %q, %q, want A and B", a.name, b.name)
	}
	c := await(t, started)
	if c.name != "c" || !c.deadline.Equal(a.deadline) || c.start.Before(b.deadline) {
		t.Fatalf("third call = %+v, want C sharing A's command deadline %v after B's deadline %v", c, a.deadline, b.deadline)
	}
	close(gate)
	d := await(t, started)
	if d.name != "d" || d.deadline.Before(b.deadline.Add(providerTimeout)) || d.deadline.After(d.start.Add(providerTimeout)) {
		t.Fatalf("fourth call = %+v, want D with a fresh %s provider budget", d, providerTimeout)
	}
	reply := await(t, pending)
	if reply.exit != 1 || reply.response.Outcome != "error" || reply.response.Error.Code != "search_failed" || len(reply.response.Results) != 0 {
		t.Fatalf("unexpected reply: %+v", reply)
	}
	for i, entry := range reply.response.Executions {
		if entry.Provider != names[i] || entry.DurationMS == nil || calls[i].Load() != 1 {
			t.Errorf("execution = %+v, calls = %d", entry, calls[i].Load())
		}
		if entry.Provider == "b" {
			if entry.Status != "timeout" || entry.Reason == nil || *entry.Reason != "provider_timeout" || entry.Error == nil || entry.Error.Code != "provider_timeout" {
				t.Errorf("B execution = %+v", entry)
			}
		} else if entry.Status != "empty" || entry.Error != nil {
			t.Errorf("unexpected empty execution: %+v", entry)
		}
	}
}

func TestMergeKeepsProviderTimeoutDuringAdapterCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, queuedReason string
		budget                      time.Duration
		exit                        int
	}{
		{"then interrupted", "cancelled", "user_cancelled", 4 * time.Second, 130},
		{"then command expires", "partial", "command_timeout", 350 * time.Millisecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cleanup := make(chan struct{})
			defer close(cleanup)
			requestEnded := make(chan error, 1)
			b := mockProvider(func(ctx context.Context, _ retrieval.SearchRequest) ([]retrieval.Candidate, *retrieval.Error) {
				<-ctx.Done()
				requestEnded <- ctx.Err()
				<-cleanup
				return []retrieval.Candidate{{URL: "https://example.org/late", Rank: 1}}, nil
			})
			b.Timeout = 30 * time.Millisecond
			const kept = "https://example.org/kept"
			registry := map[string]retrieval.SearchProvider{
				"a": staticProvider(retrieval.Candidate{URL: kept, Rank: 1}),
				"b": b,
				"c": staticProvider(),
			}
			pending := startMerge(ctx, retrieval.SearchOptions{Timeout: tc.budget, Strategy: "merge", MaxConcurrency: 1}, []string{"a", "b", "c"}, registry)
			if err := await(t, requestEnded); err != context.DeadlineExceeded {
				t.Fatalf("provider context error = %v, want deadline exceeded", err)
			}
			if tc.exit == 130 {
				cancel(retrieval.ErrUserCancelled)
			}
			reply := await(t, pending)
			response := reply.response
			if reply.exit != tc.exit || response.Outcome != tc.outcome || len(response.Results) != 1 || response.Results[0].URL != kept {
				t.Fatalf("unexpected merge reply: %+v", reply)
			}
			if entry := response.Executions[1]; entry.Status != "timeout" || entry.Reason == nil || *entry.Reason != "provider_timeout" ||
				entry.Error == nil || entry.Error.Code != "provider_timeout" || entry.DurationMS == nil {
				t.Errorf("B execution = %+v, want provider_timeout", entry)
			}
			assertUncalled(t, response.Executions[2], "c", "not_started", tc.queuedReason)
		})
	}
}
