package cli_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"linkweft/internal/cli"
	"linkweft/internal/retrieval"
)

func TestKeenableThreeProviderFallback(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	config := testConfig(t, "[search]\npriority = ['keenable','exa']")
	for _, tc := range []struct {
		name, winner string
		options      []string
		executions   []executionWant
		calls        [3]int32
	}{
		{"built-in order", "exa", nil, []executionWant{
			{provider: "exa", status: "success"},
			{"keenable", "not_started", "fallback_satisfied", ""},
			{"tavily", "not_started", "fallback_satisfied", ""},
		}, [3]int32{0, 1, 0}},
		{"configured order and empty fallback", "exa", []string{"--config", config}, []executionWant{
			{provider: "keenable", status: "empty"},
			{provider: "exa", status: "success"},
			{"tavily", "not_started", "fallback_satisfied", ""},
		}, [3]int32{0, 1, 1}},
		{"explicit order wins", "exa", []string{"--config", config, "--providers", "exa,keenable,tavily"}, []executionWant{
			{provider: "exa", status: "success"},
			{"keenable", "not_started", "fallback_satisfied", ""},
			{"tavily", "not_started", "fallback_satisfied", ""},
		}, [3]int32{0, 1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, tavily := testApp(t, testKey, 10, bodyResponse(`{"results":[{"url":"https://example.org/tavily"}]}`))
			exa, exaCalls := exaApp(t, exaKey, 10, bodyResponse(`{"results":[{"url":"https://example.org/exa"}]}`))
			app.NewExa = exa.NewExa
			keenable, fetches := addKeenable(t, &app, keenableKey, query, 10, bodyResponse(`{"results":[]}`), nil)
			stdout, _ := run(t, context.Background(), app, 0, append([]string{"search", query}, tc.options...)...)
			obj := envelope(t, stdout, "search", "success")
			assertResultURLs(t, obj, "https://example.org/"+tc.winner)
			assertExecutions(t, obj, tc.executions...)
			assertCalls(t, tavily, tc.calls[0])
			assertCalls(t, exaCalls, tc.calls[1])
			assertCalls(t, keenable, tc.calls[2])
			assertCalls(t, fetches, 0)
		})
	}
}

func TestKeenableInvalidSearchURLFallsBack(t *testing.T) {
	isolateConfig(t)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	t.Setenv("EXA_API_KEY", exaKey)
	const fallbackURL = "https://example.org/exa"
	for _, tc := range []struct{ name, url string }{
		{"port out of range", "https://example.org:65536/page"},
		{"bracketed hostname", "https://[example.org]/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exa, exaCalls := exaApp(t, exaKey, 10, bodyResponse(fmt.Sprintf(`{"results":[{"url":%q}]}`, fallbackURL)))
			app := cli.App{NewExa: exa.NewExa}
			searches, fetches := addKeenable(t, &app, keenableKey, query, 10,
				bodyResponse(fmt.Sprintf(`{"results":[{"url":%q}]}`, tc.url)), nil)
			stdout, stderr := run(t, context.Background(), app, 0, "search", query, "--providers", "keenable,exa")
			obj := envelope(t, stdout, "search", "success")
			assertCalls(t, searches, 1)
			assertCalls(t, exaCalls, 1)
			assertCalls(t, fetches, 0)
			assertResultURLs(t, obj, fallbackURL)
			assertExecutions(t, obj, executionWant{provider: "keenable", status: "error", code: "invalid_response"},
				executionWant{provider: "exa", status: "success"})
			warnings := obj["warnings"].([]any)
			if len(warnings) != 1 {
				t.Fatalf("warnings = %#v, want one diagnostic", warnings)
			}
			objectCode(t, warnings[0], "invalid_results_dropped")
			if !strings.Contains(stderr, "invalid_results_dropped:") {
				t.Errorf("missing discarded URL diagnostic in stderr: %q", stderr)
			}
		})
	}
}

func TestKeenableFetchRouteIsIndependent(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	config := testConfig(t, "[search]\npriority = ['tavily']\n[fetch]\npriority = ['keenable']")
	app, tavilyFetch, exaFetch := fetchApp(t, bodyResponse(tavilyBody), bodyResponse(exaBody))
	tavily, tavilySearch := testApp(t, testKey, 10, bodyResponse(fmt.Sprintf(`{"results":[{"url":%q}]}`, fetchURL)))
	exa, exaSearch := exaApp(t, exaKey, 10, bodyResponse(`{"results":[]}`))
	app.NewTavily, app.NewExa = tavily.NewTavily, exa.NewExa
	searches, fetches := addKeenable(t, &app, keenableKey, query, 10, nil, bodyResponse(keenableBody))

	stdout, _ := run(t, context.Background(), app, 0, "search", query, "--config", config)
	obj := envelope(t, stdout, "search", "success")
	assertResultURLs(t, obj, fetchURL)
	assertExecutions(t, obj, executionWant{provider: "tavily", status: "success"},
		executionWant{"exa", "not_started", "fallback_satisfied", ""},
		executionWant{"keenable", "not_started", "fallback_satisfied", ""})
	assertCalls(t, fetches, 0)

	results := obj["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected a single result, got %#v", results)
	}
	found := results[0].(map[string]any)["url"].(string)
	stdout, _ = run(t, context.Background(), app, 0, "fetch", found, "--config", config)
	obj = fetchResponse(t, stdout, "success", true)
	assertExecutions(t, obj, executionWant{provider: "keenable", status: "success"},
		executionWant{"exa", "not_started", "fallback_satisfied", ""},
		executionWant{"tavily", "not_started", "fallback_satisfied", ""})
	assertCalls(t, fetches, 1)

	stdout, _ = run(t, context.Background(), app, 0, fetchArgs()...)
	obj = fetchResponse(t, stdout, "success", true)
	assertExecutions(t, obj, executionWant{provider: "keenable", status: "success"},
		executionWant{"exa", "not_started", "fallback_satisfied", ""},
		executionWant{"tavily", "not_started", "fallback_satisfied", ""})
	assertCalls(t, tavilyFetch, 0)
	assertCalls(t, exaFetch, 0)
	assertCalls(t, fetches, 2)
	assertCalls(t, tavilySearch, 1)
	assertCalls(t, exaSearch, 0)
	assertCalls(t, searches, 0)
}

func TestFirecrawlFourProviderMerge(t *testing.T) {
	isolateConfig(t)
	configureFourProviders(t)
	const canonical, tavilyURL = "https://example.org/shared", "HTTPS://EXAMPLE.ORG:443/shared"
	const exaURL, keenableURL = "https://example.org/exa", "https://example.org/keenable"
	app, tavily := testApp(t, testKey, 10, bodyResponse(fmt.Sprintf(`{"results":[{"url":%q}]}`, tavilyURL)))
	exa, exaCalls := exaApp(t, exaKey, 10, bodyResponse(fmt.Sprintf(`{"results":[{"url":%q},{"url":%q}]}`, exaURL, canonical)))
	app.NewExa = exa.NewExa
	keenable, fetches := addKeenable(t, &app, keenableKey, query, 10, bodyResponse(fmt.Sprintf(
		`{"results":[{"url":%q},{},{"url":%q,"title":"Shared","snippet":"Keenable snippet"}]}`, keenableURL, canonical)), nil)
	firecrawl, scrapes := addFirecrawl(t, &app, firecrawlKey, 10, bodyResponse(fmt.Sprintf(
		`{"success":true,"data":{"web":[{"url":%q,"position":8,"title":"Firecrawl title","description":"Firecrawl snippet"}]}}`, canonical)), nil)
	stdout, _ := run(t, context.Background(), app, 0, "search", query, "--strategy", "merge")
	obj := envelope(t, stdout, "search", "success")
	assertExecutions(t, obj, executionWant{provider: "exa", status: "success"},
		executionWant{provider: "keenable", status: "success"}, executionWant{provider: "firecrawl", status: "success"},
		executionWant{provider: "tavily", status: "success"})
	assertResultURLs(t, obj, canonical, exaURL, keenableURL)
	want := map[string]any{
		"url": canonical, "title": "Shared", "snippet": "Keenable snippet", "content": nil,
		"sources": []any{
			map[string]any{"provider": "exa", "url": canonical, "rank": float64(2)},
			map[string]any{"provider": "keenable", "url": canonical, "rank": float64(3)},
			map[string]any{"provider": "firecrawl", "url": canonical, "rank": float64(8)},
			map[string]any{"provider": "tavily", "url": tavilyURL, "rank": float64(1)},
		},
	}
	if results := obj["results"].([]any); len(results) == 0 || !reflect.DeepEqual(results[0], want) {
		t.Errorf("expected leading fused result %#v, got %#v", want, results)
	}
	assertCalls(t, tavily, 1)
	assertCalls(t, exaCalls, 1)
	assertCalls(t, keenable, 1)
	assertCalls(t, firecrawl, 1)
	assertCalls(t, fetches, 0)
	assertCalls(t, scrapes, 0)
}

func TestKeenableThreeProviderMergeInterruptions(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	for _, tc := range []struct {
		name, command, platform, outcome string
		status, reason                   string
		exit                             int
		exaCalls                         int32
	}{
		{"provider timeout", "4s", "150ms", "partial", "timeout", "provider_timeout", 0, 1},
		{"command timeout", "500ms", "4s", "partial", "timeout", "command_timeout", 0, 0},
		{"user interruption", "4s", "4s", "cancelled", "cancelled", "user_cancelled", 130, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			app, tavily := testApp(t, testKey, 10, bodyResponse(`{"results":[{"url":"https://example.org/tavily"}]}`))
			exa, exaCalls := exaApp(t, exaKey, 10, bodyResponse(`{"results":[]}`))
			app.NewExa = exa.NewExa
			keenable, fetches := addKeenable(t, &app, keenableKey, query, 10, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"results":`)
				w.(http.Flusher).Flush()
				if tc.exit == 130 {
					cancel(retrieval.ErrUserCancelled)
				}
				waitForRequestCancellation(t, r)
			}, nil)
			config := testConfig(t, fmt.Sprintf(`[search]
priority = ['tavily', 'keenable']
max_concurrency = 1
timeout = %q
[providers.keenable]
search_timeout = %q
fetch_timeout = "8s"`, tc.command, tc.platform))
			stdout, _ := run(t, ctx, app, tc.exit, "search", query, "--strategy", "merge", "--config", config)
			obj := envelope(t, stdout, "search", tc.outcome)
			assertResultURLs(t, obj, "https://example.org/tavily")
			exaExecution := executionWant{"exa", "not_started", tc.reason, ""}
			if tc.exaCalls == 1 {
				exaExecution = executionWant{provider: "exa", status: "empty"}
			}
			assertExecutions(t, obj, executionWant{provider: "tavily", status: "success"},
				executionWant{"keenable", tc.status, tc.reason, tc.reason}, exaExecution)
			if tc.exit == 130 {
				objectCode(t, obj["error"], "user_cancelled")
			} else if obj["error"] != nil {
				t.Errorf("overall error = %#v, want null", obj["error"])
			}
			assertCalls(t, tavily, 1)
			assertCalls(t, keenable, 1)
			assertCalls(t, exaCalls, tc.exaCalls)
			assertCalls(t, fetches, 0)
		})
	}
}

func TestKeenableFetchSharesCommandBudget(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	stall := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"message":%q,`, untrustedError+keenableKey)
		w.(http.Flusher).Flush()
		waitForRequestCancellation(t, r)
	}
	app, tavily, exa := fetchApp(t, stall, bodyResponse(exaBody))
	searches, fetches := addKeenable(t, &app, keenableKey, query, 10, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		stall(w, r)
	})
	config := testConfig(t, `[fetch]
timeout = "500ms"
[providers.keenable]
search_timeout = "4s"
fetch_timeout = "150ms"
[providers.tavily]
fetch_timeout = "4s"`)
	stdout, stderr := run(t, context.Background(), app, 1, fetchArgs("--providers", "keenable,tavily,exa", "--config", config)...)
	obj := fetchResponse(t, stdout, "error", false)
	objectCode(t, obj["error"], "fetch_failed")
	assertExecutions(t, obj, executionWant{"keenable", "timeout", "provider_timeout", "provider_timeout"},
		executionWant{"tavily", "timeout", "command_timeout", "command_timeout"},
		executionWant{"exa", "not_started", "command_timeout", ""})
	assertKeenableOutputControlled(t, stdout, stderr)
	assertCalls(t, fetches, 1)
	assertCalls(t, tavily, 1)
	assertCalls(t, exa, 0)
	assertCalls(t, searches, 0)
}

func waitForRequestCancellation(t *testing.T, r *http.Request) {
	t.Helper()
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
		t.Error("HTTP request was not cancelled")
	}
}
