package cli_test

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"linkweft/internal/cli"
	"linkweft/internal/retrieval"
)

// Fixture arrays use Tavily, Exa, Keenable, Firecrawl order for both operations.
type fourProviderCalls [4]*atomic.Int32

func (calls fourProviderCalls) assert(t *testing.T, want [4]int32) {
	t.Helper()
	for i, count := range calls {
		if got := count.Load(); got != want[i] {
			t.Errorf("%s calls = %d, want %d", [4]string{"tavily", "exa", "keenable", "firecrawl"}[i], got, want[i])
		}
	}
}

func configureFourProviders(t *testing.T) {
	t.Helper()
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	t.Setenv("FIRECRAWL_API_KEY", firecrawlKey)
}

func fourProviderApp(t *testing.T, search, fetch [4]http.HandlerFunc) (cli.App, fourProviderCalls, fourProviderCalls) {
	t.Helper()
	tavily, tavilySearch := testApp(t, testKey, 10, search[0])
	exa, exaSearch := exaApp(t, exaKey, 10, search[1])
	app, tavilyFetch, exaFetch := fetchApp(t, fetch[0], fetch[1])
	app.NewTavily, app.NewExa = tavily.NewTavily, exa.NewExa
	keenableSearch, keenableFetch := addKeenable(t, &app, keenableKey, query, 10, search[2], fetch[2])
	firecrawlSearch, firecrawlFetch := addFirecrawl(t, &app, firecrawlKey, 10, search[3], fetch[3])
	return app, fourProviderCalls{tavilySearch, exaSearch, keenableSearch, firecrawlSearch},
		fourProviderCalls{tavilyFetch, exaFetch, keenableFetch, firecrawlFetch}
}

func TestFirecrawlFourProviderSearchFallback(t *testing.T) {
	isolateConfig(t)
	configureFourProviders(t)
	config := testConfig(t, "[search]\npriority = ['firecrawl']")
	const hit = `{"results":[{"url":"https://example.org/article?a=1&b=2"}]}`
	const firecrawlHit = `{"success":true,"data":{"web":[{"url":"https://example.org/article?a=1&b=2"}]}}`
	for _, tc := range []struct {
		name, tavily, exa string
		opts              []string
		want              []executionWant
		calls             [4]int32
	}{
		{"built-in order stops after Exa", hit, hit, nil, []executionWant{
			{provider: "exa", status: "success"},
			{"keenable", "not_started", "fallback_satisfied", ""},
			{"firecrawl", "not_started", "fallback_satisfied", ""},
			{"tavily", "not_started", "fallback_satisfied", ""},
		}, [4]int32{0, 1, 0, 0}},
		{"built-in order falls through to Firecrawl", emptyExa, emptyExa, nil, []executionWant{
			{provider: "exa", status: "empty"},
			{provider: "keenable", status: "empty"}, {provider: "firecrawl", status: "success"},
			{"tavily", "not_started", "fallback_satisfied", ""},
		}, [4]int32{0, 1, 1, 1}},
		{"configured order moves Firecrawl first", hit, hit, []string{"--config", config}, []executionWant{
			{provider: "firecrawl", status: "success"},
			{"exa", "not_started", "fallback_satisfied", ""},
			{"keenable", "not_started", "fallback_satisfied", ""},
			{"tavily", "not_started", "fallback_satisfied", ""},
		}, [4]int32{0, 0, 0, 1}},
		{"explicit subset wins over configuration", hit, hit, []string{"--config", config, "--providers", "exa,firecrawl"}, []executionWant{
			{provider: "exa", status: "success"}, {"firecrawl", "not_started", "fallback_satisfied", ""},
		}, [4]int32{0, 1, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, searches, fetches := fourProviderApp(t, [4]http.HandlerFunc{
				bodyResponse(tc.tavily), bodyResponse(tc.exa), bodyResponse(emptyExa), bodyResponse(firecrawlHit),
			}, [4]http.HandlerFunc{})
			stdout, _ := run(t, context.Background(), app, 0, append([]string{"search", query}, tc.opts...)...)
			obj := envelope(t, stdout, "search", "success")
			assertResultURLs(t, obj, fetchURL)
			assertExecutions(t, obj, tc.want...)
			searches.assert(t, tc.calls)
			fetches.assert(t, [4]int32{})
		})
	}
}

func TestFirecrawlFetchRouteIsIndependent(t *testing.T) {
	isolateConfig(t)
	configureFourProviders(t)
	config := testConfig(t, "[search]\npriority = ['exa']\n[fetch]\npriority = ['tavily']")
	app, searches, fetches := fourProviderApp(t, [4]http.HandlerFunc{
		nil, bodyResponse(fmt.Sprintf(`{"results":[{"url":%q}]}`, fetchURL)), nil, nil,
	}, [4]http.HandlerFunc{bodyResponse(tavilyBody), nil, nil, bodyResponse(firecrawlBody)})
	stdout, _ := run(t, context.Background(), app, 0, "search", query, "--config", config)
	obj := envelope(t, stdout, "search", "success")
	assertResultURLs(t, obj, fetchURL)
	assertExecutions(t, obj, executionWant{provider: "exa", status: "success"},
		executionWant{"keenable", "not_started", "fallback_satisfied", ""},
		executionWant{"firecrawl", "not_started", "fallback_satisfied", ""},
		executionWant{"tavily", "not_started", "fallback_satisfied", ""})
	found := obj["results"].([]any)[0].(map[string]any)["url"].(string)
	fetches.assert(t, [4]int32{})

	stdout, _ = run(t, context.Background(), app, 0, "fetch", found, "--config", config, "--providers", "firecrawl,tavily")
	obj = fetchResponse(t, stdout, "success", true)
	assertExecutions(t, obj, executionWant{provider: "firecrawl", status: "success"},
		executionWant{"tavily", "not_started", "fallback_satisfied", ""})
	doc := obj["document"].(map[string]any)
	if doc["provider"] != "firecrawl" || doc["requested_url"] != found {
		t.Errorf("unexpected document: %#v", doc)
	}
	fetches.assert(t, [4]int32{0, 0, 0, 1})

	stdout, _ = run(t, context.Background(), app, 0, fetchArgs("--config", config)...)
	obj = fetchResponse(t, stdout, "success", true)
	assertExecutions(t, obj, executionWant{provider: "tavily", status: "success"},
		executionWant{"keenable", "not_started", "fallback_satisfied", ""},
		executionWant{"exa", "not_started", "fallback_satisfied", ""},
		executionWant{"firecrawl", "not_started", "fallback_satisfied", ""})
	fetches.assert(t, [4]int32{1, 0, 0, 1})
	searches.assert(t, [4]int32{0, 1, 0, 0})
}

func TestFirecrawlFourProviderMergeInterruptions(t *testing.T) {
	isolateConfig(t)
	configureFourProviders(t)
	for _, tc := range []struct {
		outcome, timeout, status, reason string
		exit                             int
	}{
		{"partial", "500ms", "timeout", "command_timeout", 0},
		{"cancelled", "4s", "cancelled", "user_cancelled", 130},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			app, searches, fetches := fourProviderApp(t, [4]http.HandlerFunc{
				bodyResponse(`{"results":[{"url":"https://example.org/tavily"}]}`), nil, nil,
				func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"success":true,"data":`)
					w.(http.Flusher).Flush()
					if tc.exit == 130 {
						cancel(retrieval.ErrUserCancelled)
					}
					waitForRequestCancellation(t, r)
				},
			}, [4]http.HandlerFunc{})
			config := testConfig(t, fmt.Sprintf(`[search]
priority = ['tavily', 'firecrawl']
max_concurrency = 1
timeout = %q
[providers.firecrawl]
search_timeout = '4s'`, tc.timeout))
			stdout, _ := run(t, ctx, app, tc.exit, "search", query, "--strategy", "merge", "--config", config)
			obj := envelope(t, stdout, "search", tc.outcome)
			assertResultURLs(t, obj, "https://example.org/tavily")
			assertExecutions(t, obj, executionWant{provider: "tavily", status: "success"},
				executionWant{"firecrawl", tc.status, tc.reason, tc.reason},
				executionWant{"exa", "not_started", tc.reason, ""},
				executionWant{"keenable", "not_started", tc.reason, ""})
			if tc.exit == 130 {
				objectCode(t, obj["error"], "user_cancelled")
			} else if obj["error"] != nil {
				t.Errorf("overall error = %#v, want null", obj["error"])
			}
			searches.assert(t, [4]int32{1, 0, 0, 1})
			fetches.assert(t, [4]int32{})
		})
	}
}

func TestFirecrawlFetchSharesCommandBudget(t *testing.T) {
	isolateConfig(t)
	configureFourProviders(t)
	stall := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"error":%q,`, untrustedError+firecrawlKey)
		w.(http.Flusher).Flush()
		waitForRequestCancellation(t, r)
	}
	app, searches, fetches := fourProviderApp(t, [4]http.HandlerFunc{}, [4]http.HandlerFunc{
		stall, nil, nil, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			stall(w, r)
		},
	})
	config := testConfig(t, `[fetch]
priority = ['firecrawl', 'tavily', 'exa']
timeout = '500ms'
[providers.firecrawl]
fetch_timeout = '150ms'
[providers.tavily]
fetch_timeout = '4s'`)
	stdout, stderr := run(t, context.Background(), app, 1, fetchArgs("--config", config)...)
	obj := fetchResponse(t, stdout, "error", false)
	objectCode(t, obj["error"], "fetch_failed")
	assertExecutions(t, obj, executionWant{"firecrawl", "timeout", "provider_timeout", "provider_timeout"},
		executionWant{"tavily", "timeout", "command_timeout", "command_timeout"},
		executionWant{"exa", "not_started", "command_timeout", ""},
		executionWant{"keenable", "not_started", "command_timeout", ""})
	assertFirecrawlOutputControlled(t, stdout, stderr)
	fetches.assert(t, [4]int32{1, 0, 0, 1})
	searches.assert(t, [4]int32{})
}
