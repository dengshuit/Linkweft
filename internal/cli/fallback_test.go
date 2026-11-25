package cli_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"linkweft/internal/cli"
	"linkweft/internal/provider"
	"linkweft/internal/retrieval"
)

const exaKey = "test-only-exa-key"

func exaApp(t *testing.T, key string, limit int, handle http.HandlerFunc) (cli.App, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/search" {
			t.Errorf("unexpected Exa request: %s %s", r.Method, r.URL.RequestURI())
		}
		if r.Header.Get("x-api-key") != key || r.Header.Get("Authorization") != "" {
			t.Error("unexpected Exa authentication headers")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected JSON request")
		}
		assertJSON(t, r.Body, fmt.Sprintf(`{
			"query":%q,"numResults":%d,"type":"auto","contents":{"text":false,"highlights":false}
		}`, query, limit))
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	return cli.App{NewExa: func(apiKey string) retrieval.SearchProvider {
		return provider.NewExa(apiKey, server.Client(), server.URL+"/search")
	}}, &calls
}

func TestExaSearchContract(t *testing.T) {
	isolateConfig(t)
	path := testConfig(t, "[providers.exa]\napi_key = '"+fileKey+"'\nsearch_timeout = '2s'")
	for _, tc := range []struct{ name, env, key string }{
		{"file key", "", fileKey},
		{"environment key overrides file", exaKey, exaKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EXA_API_KEY", tc.env)
			app, calls := exaApp(t, tc.key, 100, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"results":[{"url":"/invalid"},{"url":%q},
					{"url":"https://example.org/article","title":"Article","text":"not requested"}]}`,
					"http://"+r.Host+"/result")
			})
			stdout, stderr := run(t, context.Background(), app, 0, "search", query, "--providers", "exa", "--config", path, "--limit", "101")
			response := envelope(t, stdout, "search", "success")
			assertExecutions(t, response, executionWant{provider: "exa", status: "success"})
			results := response["results"].([]any)
			if len(results) != 2 {
				t.Fatalf("results = %#v, want two results", results)
			}
			first := results[0].(map[string]any)
			address, _ := first["url"].(string)
			if !strings.HasPrefix(address, "http://127.0.0.1:") || !strings.HasSuffix(address, "/result") {
				t.Errorf("unexpected first URL: %q", address)
			}
			want := []any{
				map[string]any{"url": address, "title": nil, "snippet": nil, "content": nil,
					"sources": []any{map[string]any{"provider": "exa", "url": address, "rank": float64(2)}}},
				map[string]any{"url": "https://example.org/article", "title": "Article", "snippet": nil, "content": nil,
					"sources": []any{map[string]any{"provider": "exa", "url": "https://example.org/article", "rank": float64(3)}}},
			}
			if !reflect.DeepEqual(results, want) {
				t.Errorf("results = %#v, want %#v", results, want)
			}
			warnings := response["warnings"].([]any)
			if len(warnings) != 2 {
				t.Fatalf("unexpected warnings: %#v", warnings)
			}
			for i, code := range []string{"upstream_limit_applied", "invalid_results_dropped"} {
				objectCode(t, warnings[i], code)
				if warnings[i].(map[string]any)["provider"] != "exa" || !strings.Contains(stderr, code) {
					t.Errorf("unexpected diagnostic for %s: %#v, stderr = %q", code, warnings[i], stderr)
				}
			}
			assertCalls(t, calls, 1)
		})
	}
}

type executionWant struct {
	provider, status, reason, code string
}

func assertExecutions(t *testing.T, response map[string]any, want ...executionWant) {
	t.Helper()
	items := response["executions"].([]any)
	if len(items) != len(want) {
		t.Fatalf("executions = %#v, want %d records", items, len(want))
	}
	for i, expected := range want {
		entry, ok := items[i].(map[string]any)
		var reason any
		if expected.reason != "" {
			reason = expected.reason
		}
		if !ok || len(entry) != 5 || entry["provider"] != expected.provider || entry["status"] != expected.status || entry["reason"] != reason {
			t.Fatalf("execution %d = %#v, want %#v", i, items[i], expected)
		}
		if expected.status == "skipped" || expected.status == "not_started" {
			if entry["duration_ms"] != nil {
				t.Errorf("expected null duration for an uncalled provider: %#v", entry)
			}
		} else if duration, ok := entry["duration_ms"].(float64); !ok || duration < 0 {
			t.Errorf("expected a nonnegative duration: %#v", entry)
		}
		if expected.code != "" {
			objectCode(t, entry["error"], expected.code)
		} else if entry["error"] != nil {
			t.Errorf("expected null execution error: %#v", entry)
		}
	}
}

func TestSearchFallback(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	const (
		empty     = `{"results":[]}`
		tavilyHit = `{"results":[{"url":"https://example.org/tavily"}]}`
		exaHits   = `{"results":[{"url":"https://example.org/z"},{"url":"https://example.org/a"}]}`
	)
	tavilyEmpty := executionWant{provider: "tavily", status: "empty"}
	exaEmpty := executionWant{provider: "exa", status: "empty"}
	tavilySuccess := executionWant{provider: "tavily", status: "success"}
	exaSuccess := executionWant{provider: "exa", status: "success"}
	tavilyPending := executionWant{provider: "tavily", status: "not_started", reason: "fallback_satisfied"}
	exaPending := executionWant{provider: "exa", status: "not_started", reason: "fallback_satisfied"}
	tavilyFailure := executionWant{provider: "tavily", status: "error", code: "upstream_error"}
	tavilyInvalid := executionWant{provider: "tavily", status: "error", code: "invalid_response"}
	exaFailure := executionWant{provider: "exa", status: "error", code: "rate_limited"}
	for _, tc := range []struct {
		name, priority, providers, tavilyBody, exaBody, outcome string
		exit                                                    int
		executions                                              []executionWant
		calls, urls                                             []string
	}{
		{
			name: "AC01 default order and empty fallback", tavilyBody: empty, exaBody: exaHits, outcome: "success",
			executions: []executionWant{tavilyEmpty, exaSuccess}, calls: []string{"tavily", "exa"},
			urls: []string{"https://example.org/z", "https://example.org/a"},
		},
		{
			name: "AC02 stop after one result", tavilyBody: tavilyHit, exaBody: exaHits, outcome: "success",
			executions: []executionWant{tavilySuccess, exaPending}, calls: []string{"tavily"},
			urls: []string{"https://example.org/tavily"},
		},
		{
			name: "AC05 explicit order overrides priority", priority: "tavily", providers: "exa,tavily",
			tavilyBody: tavilyHit, exaBody: exaHits, outcome: "success",
			executions: []executionWant{exaSuccess, tavilyPending}, calls: []string{"exa"},
			urls: []string{"https://example.org/z", "https://example.org/a"},
		},
		{
			name: "AC05 configured priority and built-in completion", priority: "exa",
			tavilyBody: tavilyHit, exaBody: empty, outcome: "success",
			executions: []executionWant{exaEmpty, tavilySuccess}, calls: []string{"exa", "tavily"},
			urls: []string{"https://example.org/tavily"},
		},
		{
			name: "AC04 AC26 single Exa failure", providers: "exa", tavilyBody: tavilyHit,
			outcome: "error", exit: 1, executions: []executionWant{exaFailure}, calls: []string{"exa"},
		},
		{
			name: "AC07 AC26 recover from upstream failure", exaBody: exaHits, outcome: "success",
			executions: []executionWant{tavilyFailure, exaSuccess}, calls: []string{"tavily", "exa"},
			urls: []string{"https://example.org/z", "https://example.org/a"},
		},
		{
			name: "AC12 recover from all invalid URLs", tavilyBody: `{"results":[{"url":"/invalid"}]}`, exaBody: exaHits, outcome: "success",
			executions: []executionWant{tavilyInvalid, exaSuccess}, calls: []string{"tavily", "exa"},
			urls: []string{"https://example.org/z", "https://example.org/a"},
		},
		{
			name: "AC09 all empty", tavilyBody: empty, exaBody: empty, outcome: "empty",
			executions: []executionWant{tavilyEmpty, exaEmpty}, calls: []string{"tavily", "exa"},
		},
		{
			name: "AC10 empty and failure", tavilyBody: empty, outcome: "error", exit: 1,
			executions: []executionWant{tavilyEmpty, exaFailure}, calls: []string{"tavily", "exa"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var order []string
			handler := func(name, body string, failureStatus int) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					order = append(order, name)
					mu.Unlock()
					if body == "" {
						w.WriteHeader(failureStatus)
						return
					}
					fmt.Fprint(w, body)
				}
			}
			app, _ := testApp(t, testKey, 10, handler("tavily", tc.tavilyBody, http.StatusServiceUnavailable))
			exa, _ := exaApp(t, exaKey, 10, handler("exa", tc.exaBody, http.StatusTooManyRequests))
			app.NewExa = exa.NewExa
			args := []string{"search", query}
			if tc.priority != "" {
				path := testConfig(t, fmt.Sprintf("[search]\npriority = [%q]", tc.priority))
				args = append(args, "--config", path)
			}
			if tc.providers != "" {
				args = append(args, "--providers", tc.providers)
			}
			stdout, _ := run(t, context.Background(), app, tc.exit, args...)
			response := envelope(t, stdout, "search", tc.outcome)
			assertExecutions(t, response, tc.executions...)
			if tc.exit != 0 {
				objectCode(t, response["error"], "search_failed")
			}
			assertResultURLs(t, response, tc.urls...)
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(order, tc.calls) {
				t.Errorf("HTTP call order = %v, want %v", order, tc.calls)
			}
		})
	}
}

func assertResultURLs(t *testing.T, response map[string]any, want ...string) {
	t.Helper()
	var urls []string
	for _, item := range response["results"].([]any) {
		address, _ := item.(map[string]any)["url"].(string)
		urls = append(urls, address)
	}
	if !slices.Equal(urls, want) {
		t.Errorf("result URLs = %v, want %v", urls, want)
	}
}

func TestSearchStrategyPreflight(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("ANYSEARCH_API_KEY", "test-only-anysearch-key")
	app, tavilyCalls := testApp(t, testKey, 10, func(http.ResponseWriter, *http.Request) {})
	exa, exaCalls := exaApp(t, exaKey, 10, func(http.ResponseWriter, *http.Request) {})
	app.NewExa = exa.NewExa
	for _, tc := range []struct {
		name, providers, issue string
		options                []string
	}{
		{"AC06 missing Exa key", "tavily,exa", "missing_credentials", nil},
		{"AC06 incompatible provider", "tavily,anysearch", "unsupported_capability", nil},
		{"AC37 unknown provider", "tavily,unknown", "invalid_argument", nil},
		{"invalid Exa timeout", "tavily,exa", "invalid_config",
			[]string{"--config", testConfig(t, "[providers.exa]\nsearch_timeout = '0s'")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, strategy := range []string{"fallback", "merge"} {
				args := append([]string{"search", query, "--strategy", strategy, "--providers", tc.providers}, tc.options...)
				stdout, _ := run(t, context.Background(), app, 2, args...)
				response := envelope(t, stdout, "search", "error")
				objectCode(t, response["error"], tc.issue)
				assertExecutions(t, response)
				assertResultURLs(t, response)
			}
			assertCalls(t, tavilyCalls, 0)
			assertCalls(t, exaCalls, 0)
		})
	}
}

func TestSearchFallbackInterruption(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	for _, tc := range []struct {
		name, platform, command, outcome string
		cancel                           bool
		exit                             int
		issue                            string
		exa, tavily                      executionWant
		tavilyCalls                      int32
		urls                             []string
	}{
		{
			name: "AC26 Exa provider timeout permits fallback", platform: "150ms", command: "3s", outcome: "success",
			exa:    executionWant{"exa", "timeout", "provider_timeout", "provider_timeout"},
			tavily: executionWant{provider: "tavily", status: "success"}, tavilyCalls: 1,
			urls: []string{"https://example.org/tavily"},
		},
		{
			name: "AC10 AC26 command budget prevents fallback", platform: "3s", command: "250ms", outcome: "error",
			exit: 1, issue: "search_failed",
			exa:    executionWant{"exa", "timeout", "command_timeout", "command_timeout"},
			tavily: executionWant{provider: "tavily", status: "not_started", reason: "command_timeout"},
		},
		{
			name: "user cancellation prevents fallback", platform: "3s", command: "3s", outcome: "cancelled",
			cancel: true, exit: 130, issue: "user_cancelled",
			exa:    executionWant{"exa", "cancelled", "user_cancelled", "user_cancelled"},
			tavily: executionWant{provider: "tavily", status: "not_started", reason: "user_cancelled"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			app, calls := testApp(t, testKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"results":[{"url":"https://example.org/tavily"}]}`)
			})
			exa, exaCalls := exaApp(t, exaKey, 10, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"results":`)
				w.(http.Flusher).Flush()
				if tc.cancel {
					cancel(retrieval.ErrUserCancelled)
				}
				select {
				case <-r.Context().Done():
				case <-time.After(4 * time.Second):
					t.Error("Exa request context was not cancelled")
				}
			})
			app.NewExa = exa.NewExa
			path := testConfig(t, fmt.Sprintf("[search]\ntimeout = %q\n[providers.exa]\nsearch_timeout = %q", tc.command, tc.platform))
			stdout, _ := run(t, ctx, app, tc.exit, "search", query, "--providers", "exa,tavily", "--config", path)
			response := envelope(t, stdout, "search", tc.outcome)
			assertExecutions(t, response, tc.exa, tc.tavily)
			assertResultURLs(t, response, tc.urls...)
			if tc.issue != "" {
				objectCode(t, response["error"], tc.issue)
			}
			assertCalls(t, exaCalls, 1)
			assertCalls(t, calls, tc.tavilyCalls)
		})
	}
}
