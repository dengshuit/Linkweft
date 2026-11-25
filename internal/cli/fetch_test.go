package cli_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"linkweft/internal/cli"
	"linkweft/internal/provider"
	"linkweft/internal/retrieval"
)

const (
	fetchURL       = "https://example.org/article?a=1&b=2"
	returnedURL    = "https://example.org/provider-result"
	tavilyBody     = `{"results":[{"url":"https://example.org/provider-result","raw_content":"# 汉🙂é字"}],"failed_results":[]}`
	exaBody        = `{"results":[{"id":"doc-42","url":"https://example.org/provider-result","text":"Example body","title":"Article"}]}`
	emptyTavily    = `{"results":[{"raw_content":" \n\t"}],"failed_results":[]}`
	emptyExa       = `{"results":[]}`
	untrustedError = "untrusted upstream error "
)

func fetchApp(t *testing.T, tavily, exa http.HandlerFunc) (cli.App, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var tavilyCalls, exaCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected a JSON POST request")
		}
		switch r.URL.RequestURI() {
		case "/extract":
			tavilyCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("x-api-key") != "" {
				t.Error("unexpected Tavily authentication headers")
			}
			assertJSON(t, r.Body, fmt.Sprintf(`{"urls":[%q],"format":"markdown","extract_depth":"basic"}`, fetchURL))
			if tavily != nil {
				tavily(w, r)
			}
		case "/contents":
			exaCalls.Add(1)
			if r.Header.Get("x-api-key") != exaKey || r.Header.Get("Authorization") != "" {
				t.Error("unexpected Exa authentication headers")
			}
			assertJSON(t, r.Body, fmt.Sprintf(`{"urls":[%q],"text":true}`, fetchURL))
			if exa != nil {
				exa(w, r)
			}
		default:
			t.Errorf("unexpected fetch endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return cli.App{
		NewTavilyFetch: func(key string) retrieval.FetchProvider {
			return provider.NewTavilyFetch(key, server.Client(), server.URL+"/extract")
		},
		NewExaFetch: func(key string) retrieval.FetchProvider {
			return provider.NewExaFetch(key, server.Client(), server.URL+"/contents")
		},
	}, &tavilyCalls, &exaCalls
}

func fetchArgs(options ...string) []string {
	return append([]string{"fetch", fetchURL}, options...)
}

func fetchResponse(t *testing.T, stdout, outcome string, document bool) map[string]any {
	t.Helper()
	obj := decode(t, strings.NewReader(stdout))
	if len(obj) != 8 || obj["schema_version"] != float64(1) || obj["operation"] != "fetch" || obj["outcome"] != outcome {
		t.Fatalf("unexpected fetch envelope: %#v", obj)
	}
	if results, ok := obj["results"].([]any); !ok || len(results) != 0 {
		t.Fatalf("expected results = [], got %#v", obj["results"])
	}
	for _, key := range []string{"executions", "warnings"} {
		if _, ok := obj[key].([]any); !ok {
			t.Fatalf("%s must be an array: %#v", key, obj[key])
		}
	}
	_, isObject := obj["document"].(map[string]any)
	if document != isObject || (!document && obj["document"] != nil) {
		t.Fatalf("unexpected document: %#v", obj["document"])
	}
	if issue, ok := obj["error"]; !ok || (outcome == "success" && issue != nil) {
		t.Errorf("unexpected overall error: %#v", obj)
	}
	return obj
}

func bodyResponse(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }
}

func TestFetchPreflight(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	app, tavilyCalls, exaCalls := fetchApp(t, nil, nil)
	cases := []struct {
		name, code string
		args       []string
	}{
		{"missing URL", "invalid_argument", []string{"fetch"}},
		{"non-HTTP URL", "invalid_argument", []string{"fetch", "file:///tmp/page"}},
		{"relative URL", "invalid_argument", []string{"fetch", "example.org/article"}},
		{"missing hostname", "invalid_argument", []string{"fetch", "https://:443/"}},
		{"bracketed domain name", "invalid_argument", []string{"fetch", "https://[example.org]/"}},
		{"bracketed IPv4 address", "invalid_argument", []string{"fetch", "https://[192.0.2.1]/"}},
		{"unbracketed IPv6 address", "invalid_argument", []string{"fetch", "https://2001:db8::1/"}},
		{"port out of range", "invalid_argument", []string{"fetch", "https://example.org:65536/page"}},
		{"malformed URL", "invalid_argument", []string{"fetch", "https://example.org/?bad=%zz"}},
		{"extra URL", "invalid_argument", fetchArgs("https://example.org/extra")},
		{"search-only strategy", "invalid_argument", fetchArgs("--strategy", "merge")},
		{"search-only limit", "invalid_argument", fetchArgs("--limit", "3")},
		{"invalid output", "invalid_argument", fetchArgs("--output", "xml")},
		{"invalid max-chars", "invalid_argument", fetchArgs("--max-chars", "0")},
		{"invalid timeout", "invalid_argument", fetchArgs("--timeout", "45")},
		{"duplicate provider", "invalid_argument", fetchArgs("--providers", "tavily, tavily")},
		{"unknown provider", "invalid_argument", fetchArgs("--providers", "tavily, unknown")},
		{"empty provider", "invalid_argument", fetchArgs("--providers", "tavily, ")},
		{"missing later credentials", "missing_credentials", fetchArgs("--providers", "tavily, exa")},
		{"unsupported later provider", "unsupported_capability", fetchArgs("--providers", "tavily, anysearch")},
		{"missing config", "invalid_config", fetchArgs("--config", filepath.Join(t.TempDir(), "absent.toml"))},
		{"duplicate config priority", "invalid_config", fetchArgs("--config", testConfig(t, "[fetch]\npriority = ['exa', 'exa']"))},
		{"invalid config count", "invalid_config", fetchArgs("--config", testConfig(t, "[fetch]\nmax_chars = 0"))},
		{"invalid config timeout", "invalid_config", fetchArgs("--config", testConfig(t, "[fetch]\ntimeout = '0s'"))},
		{"invalid provider timeout", "invalid_config", fetchArgs("--config", testConfig(t, "[providers.tavily]\nfetch_timeout = '45'"))},
		{"URL contains encoded credential", "invalid_argument", []string{"fetch", "https://example.org/?key=" + strings.ReplaceAll(testKey, "-", "%2D")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := run(t, context.Background(), app, 2, tc.args...)
			obj := envelope(t, stdout, "fetch", "error")
			objectCode(t, obj["error"], tc.code)
			assertExecutions(t, obj)
			if strings.Contains(stdout+stderr, testKey) || strings.Contains(stdout+stderr, "%2D") {
				t.Error("preflight output exposes a credential")
			}
		})
	}
	assertCalls(t, tavilyCalls, 0)
	assertCalls(t, exaCalls, 0)
}

func TestFetchTavilyOutput(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	app, tavilyCalls, exaCalls := fetchApp(t, bodyResponse(tavilyBody), nil)
	config := testConfig(t, "[fetch]\nmax_chars = 3")
	stdout, _ := run(t, context.Background(), app, 0, fetchArgs("--config", config, "--max-chars", "6")...)
	obj := fetchResponse(t, stdout, "success", true)
	doc := obj["document"].(map[string]any)
	content := doc["content"].(map[string]any)
	if len(doc) != 6 || doc["requested_url"] != fetchURL || doc["returned_url"] != returnedURL ||
		doc["final_url"] != nil || doc["title"] != nil || doc["provider"] != "tavily" {
		t.Errorf("unexpected document metadata: %#v", doc)
	}
	if len(content) != 6 || content["text"] != "# 汉🙂é" || content["format"] != "markdown" ||
		content["provider"] != "tavily" || content["local_truncated"] != true || content["upstream_truncated"] != nil ||
		content["freshness"] != "unknown" {
		t.Errorf("unexpected content: %#v", content)
	}
	assertExecutions(t, obj, executionWant{provider: "tavily", status: "success"})

	stdout, stderr := run(t, context.Background(), app, 0, fetchArgs("--config", config, "--output", "text")...)
	if stdout != "# 汉\n" || stderr != "tavily: success\n" {
		t.Errorf("unexpected text streams: stdout = %q, stderr = %q", stdout, stderr)
	}
	assertCalls(t, tavilyCalls, 2)
	assertCalls(t, exaCalls, 0)
}

func TestFetchDefaultCharacterLimit(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	const maxChars = 50_000
	page := strings.Repeat("界", maxChars)
	for _, suffix := range []string{"", "🙂"} {
		app, calls, _ := fetchApp(t, bodyResponse(fmt.Sprintf(`{"results":[{"raw_content":%q}]}`, page+suffix)), nil)
		stdout, _ := run(t, context.Background(), app, 0, fetchArgs()...)
		content := fetchResponse(t, stdout, "success", true)["document"].(map[string]any)["content"].(map[string]any)
		if content["text"] != page || content["local_truncated"] != (suffix != "") {
			t.Error("unexpected body or truncation marker at the default 50,000-code-point limit")
		}
		assertCalls(t, calls, 1)
	}
}

func TestFetchFallback(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	config := testConfig(t, "[search]\npriority = ['tavily']\n[fetch]\npriority = ['exa']")
	tavilyFailure := fmt.Sprintf(`{"failed_results":[{"url":%q,"error":%q}],"results":[{"raw_content":"must not win"}]}`, fetchURL, untrustedError+testKey)
	exaFailure := fmt.Sprintf(`{"statuses":[{"id":%q,"status":"error","error":{"tag":%q,"httpStatusCode":403}}],"results":[{"id":"doc-42","text":"must not win"}]}`, fetchURL, untrustedError+exaKey)
	cases := []struct {
		name, tavily, exa, selected, winner string
		want                                []executionWant
		tavilyCalls, exaCalls               int32
	}{
		{"fetch priority and stop on hit", tavilyBody, exaBody, "", "exa",
			[]executionWant{{"exa", "success", "", ""}, {"tavily", "not_started", "fallback_satisfied", ""}}, 0, 1},
		{"Tavily explicitly selected", tavilyBody, exaBody, "tavily", "tavily",
			[]executionWant{{"tavily", "success", "", ""}}, 1, 0},
		{"Tavily empty then Exa", emptyTavily, exaBody, "tavily, exa", "exa",
			[]executionWant{{"tavily", "empty", "", ""}, {"exa", "success", "", ""}}, 1, 1},
		{"Tavily failed_results then Exa", tavilyFailure, exaBody, "tavily,exa", "exa",
			[]executionWant{{"tavily", "error", "", "upstream_error"}, {"exa", "success", "", ""}}, 1, 1},
		{"Exa blocked then Tavily", tavilyBody, exaFailure, "", "tavily",
			[]executionWant{{"exa", "error", "", "content_blocked"}, {"tavily", "success", "", ""}}, 1, 1},
		{"both empty", emptyTavily, emptyExa, "", "",
			[]executionWant{{"exa", "empty", "", ""}, {"tavily", "empty", "", ""}}, 1, 1},
		{"explicit empty never expands", emptyTavily, exaBody, "tavily", "",
			[]executionWant{{"tavily", "empty", "", ""}}, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, tavilyCalls, exaCalls := fetchApp(t, bodyResponse(tc.tavily), bodyResponse(tc.exa))
			args := fetchArgs("--config", config)
			if tc.selected != "" {
				args = append(args, "--providers", tc.selected)
			}
			code, outcome := 0, "success"
			if tc.winner == "" {
				code, outcome = 1, "error"
			}
			stdout, stderr := run(t, context.Background(), app, code, args...)
			obj := fetchResponse(t, stdout, outcome, tc.winner != "")
			if tc.winner != "" {
				if obj["document"].(map[string]any)["provider"] != tc.winner {
					t.Errorf("unexpected winning document: %#v", obj["document"])
				}
			} else {
				objectCode(t, obj["error"], "fetch_failed")
			}
			assertExecutions(t, obj, tc.want...)
			assertCalls(t, tavilyCalls, tc.tavilyCalls)
			assertCalls(t, exaCalls, tc.exaCalls)
			for _, value := range []string{testKey, exaKey, untrustedError, "must not win"} {
				if strings.Contains(stdout+stderr, value) {
					t.Error("output exposed failed URL data")
				}
			}
		})
	}
}

func TestFetchExaAssociatesContentsWithRequestedURL(t *testing.T) {
	isolateConfig(t)
	t.Setenv("EXA_API_KEY", exaKey)
	const markdown = "# Example body\n\n- **Point**"
	usable := fmt.Sprintf(`{"id":"doc-42","url":"https://example.org/provider-result","text":%q,"title":"Article"}`, markdown)
	cases := []struct{ name, results, statuses, freshness, issue string }{
		{"missing statuses", usable, "", "unknown", ""},
		{"only one usable body", `{"id":"empty","text":" "},` + usable, "", "unknown", ""},
		{"document ID is unrelated to status matching", usable, fmt.Sprintf(`{"id":"doc-42","status":"error"},{"id":%q,"status":"success","source":"cached"}`, fetchURL), "cached", ""},
		{"explicitly crawled body", usable, fmt.Sprintf(`{"id":%q,"status":"success","source":"crawled"}`, fetchURL), "live", ""},
		{"contradictory statuses", usable, fmt.Sprintf(`{"id":%q,"status":"success"},{"id":%q,"status":"error"}`, fetchURL, fetchURL), "", "invalid_response"},
		{"ambiguous results", usable + `,{"id":"doc-99","text":"other body"}`, "", "", "invalid_response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"results":[%s]`, tc.results)
			if tc.statuses != "" {
				body += fmt.Sprintf(`,"statuses":[%s]`, tc.statuses)
			}
			app, _, calls := fetchApp(t, nil, bodyResponse(body+"}"))
			args := fetchArgs("--providers", "exa")
			if tc.issue == "" {
				stdout, _ := run(t, context.Background(), app, 0, args...)
				obj := fetchResponse(t, stdout, "success", true)
				doc := obj["document"].(map[string]any)
				content := doc["content"].(map[string]any)
				if doc["requested_url"] != fetchURL || doc["returned_url"] != returnedURL || doc["final_url"] != nil ||
					doc["title"] != "Article" || doc["provider"] != "exa" || content["text"] != markdown ||
					content["format"] != "markdown" || content["local_truncated"] != false || content["upstream_truncated"] != nil ||
					content["freshness"] != tc.freshness || content["provider"] != "exa" {
					t.Errorf("unexpected Exa document: %#v", doc)
				}
			} else {
				stdout, _ := run(t, context.Background(), app, 1, args...)
				obj := fetchResponse(t, stdout, "error", false)
				objectCode(t, obj["error"], "fetch_failed")
				assertExecutions(t, obj, executionWant{"exa", "error", "", tc.issue})
			}
			assertCalls(t, calls, 1)
		})
	}
}

func TestFetchBudgetAndCancellation(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	stall := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("HTTP body read was not cancelled")
		}
	}
	t.Run("provider deadline then remaining command budget", func(t *testing.T) {
		config := testConfig(t, `[fetch]
timeout = "10s"
[providers.tavily]
search_timeout = "20ms"
fetch_timeout = "200ms"
[providers.exa]
fetch_timeout = "2s"`)
		app, tavilyCalls, exaCalls := fetchApp(t, stall, stall)
		stdout, _ := run(t, context.Background(), app, 1, fetchArgs("--config", config, "--timeout", "400ms")...)
		obj := fetchResponse(t, stdout, "error", false)
		objectCode(t, obj["error"], "fetch_failed")
		assertExecutions(t, obj,
			executionWant{"tavily", "timeout", "provider_timeout", "provider_timeout"},
			executionWant{"exa", "timeout", "command_timeout", "command_timeout"})
		executions := obj["executions"].([]any)
		first := executions[0].(map[string]any)["duration_ms"].(float64)
		second := executions[1].(map[string]any)["duration_ms"].(float64)
		if first < 150 || second >= 350 {
			t.Errorf("expected operation-specific deadline and shared budget, got %.0fms then %.0fms", first, second)
		}
		assertCalls(t, tavilyCalls, 1)
		assertCalls(t, exaCalls, 1)
	})
	t.Run("config command budget prevents later request", func(t *testing.T) {
		config := testConfig(t, "[fetch]\ntimeout = '150ms'")
		app, tavilyCalls, exaCalls := fetchApp(t, stall, bodyResponse(exaBody))
		stdout, _ := run(t, context.Background(), app, 1, fetchArgs("--config", config)...)
		obj := fetchResponse(t, stdout, "error", false)
		objectCode(t, obj["error"], "fetch_failed")
		assertExecutions(t, obj,
			executionWant{"tavily", "timeout", "command_timeout", "command_timeout"},
			executionWant{"exa", "not_started", "command_timeout", ""})
		assertCalls(t, tavilyCalls, 1)
		assertCalls(t, exaCalls, 0)
	})
	t.Run("user cancellation stops fallback", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		app, tavilyCalls, exaCalls := fetchApp(t, func(w http.ResponseWriter, r *http.Request) {
			cancel(retrieval.ErrUserCancelled)
			stall(w, r)
		}, bodyResponse(exaBody))
		stdout, _ := run(t, ctx, app, 130, fetchArgs()...)
		obj := fetchResponse(t, stdout, "cancelled", false)
		objectCode(t, obj["error"], "user_cancelled")
		assertExecutions(t, obj,
			executionWant{"tavily", "cancelled", "user_cancelled", "user_cancelled"},
			executionWant{"exa", "not_started", "user_cancelled", ""})
		assertCalls(t, tavilyCalls, 1)
		assertCalls(t, exaCalls, 0)
	})
}

func TestFetchProtectsCredentials(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	config := testConfig(t, "[providers.tavily]\napi_key = '"+fileKey+"'")
	encoded := strings.ReplaceAll(fileKey, "-", "%2D")
	t.Run("body with embedded credential cannot satisfy fallback", func(t *testing.T) {
		unsafeBody := fmt.Sprintf(`{"results":[{"raw_content":%q}]}`, "Document https://example.org/?key="+encoded)
		safeBody := fmt.Sprintf(`{"results":[{"id":"doc-42","url":%q,"title":%q,"text":"Safe body"}]}`,
			"https://example.org/?key="+encoded, "Title with "+testKey)
		app, tavilyCalls, exaCalls := fetchApp(t, bodyResponse(unsafeBody), bodyResponse(safeBody))
		for _, format := range []string{"json", "text"} {
			stdout, stderr := run(t, context.Background(), app, 0, fetchArgs("--config", config, "--output", format)...)
			for _, value := range []string{testKey, fileKey, exaKey, encoded} {
				if strings.Contains(stdout+stderr, value) {
					t.Fatal("output exposed a credential")
				}
			}
			if format == "json" {
				obj := fetchResponse(t, stdout, "success", true)
				doc := obj["document"].(map[string]any)
				if doc["returned_url"] != nil || doc["title"] != nil || doc["requested_url"] != fetchURL {
					t.Error("unsafe metadata was not removed")
				}
				assertExecutions(t, obj, executionWant{"tavily", "error", "", "invalid_response"}, executionWant{provider: "exa", status: "success"})
			} else if stdout != "Safe body\n" || !strings.Contains(stderr, "invalid_response") {
				t.Error("expected body on stdout and execution diagnostic on stderr")
			}
		}
		assertCalls(t, tavilyCalls, 2)
		assertCalls(t, exaCalls, 2)
	})
	t.Run("upstream HTTP failure is controlled without retry", func(t *testing.T) {
		app, tavilyCalls, exaCalls := fetchApp(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, untrustedError+testKey+" "+fileKey)
		}, bodyResponse(exaBody))
		stdout, stderr := run(t, context.Background(), app, 0, fetchArgs("--config", config)...)
		obj := fetchResponse(t, stdout, "success", true)
		assertExecutions(t, obj, executionWant{"tavily", "error", "", "rate_limited"}, executionWant{provider: "exa", status: "success"})
		for _, value := range []string{untrustedError, testKey, fileKey} {
			if strings.Contains(stdout+stderr, value) {
				t.Fatal("output exposed an upstream error body")
			}
		}
		assertCalls(t, tavilyCalls, 1)
		assertCalls(t, exaCalls, 1)
	})
}

func TestFetchAutomaticBuiltInOrder(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	t.Setenv("FIRECRAWL_API_KEY", firecrawlKey)
	t.Setenv("ANYSEARCH_API_KEY", "test-only-anysearch-key")
	t.Setenv("TINYFISH_API_KEY", "test-only-tinyfish-key")
	app, tavilyCalls, exaCalls := fetchApp(t, bodyResponse(tavilyBody), bodyResponse(exaBody))
	keenableSearch, keenableFetch := addKeenable(t, &app, keenableKey, query, 10, nil, bodyResponse(keenableBody))
	firecrawlSearch, firecrawlFetch := addFirecrawl(t, &app, firecrawlKey, 10, nil, bodyResponse(firecrawlBody))
	stdout, _ := run(t, context.Background(), app, 0, fetchArgs()...)
	obj := fetchResponse(t, stdout, "success", true)
	assertExecutions(t, obj,
		executionWant{provider: "firecrawl", status: "success"},
		executionWant{"tavily", "not_started", "fallback_satisfied", ""},
		executionWant{"exa", "not_started", "fallback_satisfied", ""},
		executionWant{"keenable", "not_started", "fallback_satisfied", ""},
		executionWant{"tinyfish", "skipped", "unsupported_capability", ""},
		executionWant{"anysearch", "skipped", "unsupported_capability", ""})
	assertCalls(t, tavilyCalls, 0)
	assertCalls(t, exaCalls, 0)
	assertCalls(t, keenableSearch, 0)
	assertCalls(t, keenableFetch, 0)
	assertCalls(t, firecrawlSearch, 0)
	assertCalls(t, firecrawlFetch, 1)
}

func TestSearchUsesSearchEndpointsWithFetchEnabled(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	config := testConfig(t, "[search]\npriority = ['tavily']\n[fetch]\npriority = ['exa']")
	app, tavilyFetches, exaFetches := fetchApp(t, nil, bodyResponse(exaBody))
	tavily, tavilySearches := testApp(t, testKey, 10, bodyResponse(fmt.Sprintf(`{"results":[{"url":%q}]}`, fetchURL)))
	exa, exaSearches := exaApp(t, exaKey, 10, bodyResponse(`{"results":[]}`))
	app.NewTavily, app.NewExa = tavily.NewTavily, exa.NewExa
	stdout, _ := run(t, context.Background(), app, 0, "search", query, "--config", config)
	obj := envelope(t, stdout, "search", "success")
	assertResultURLs(t, obj, fetchURL)
	assertExecutions(t, obj, executionWant{provider: "tavily", status: "success"},
		executionWant{"exa", "not_started", "fallback_satisfied", ""})
	assertCalls(t, tavilySearches, 1)
	assertCalls(t, exaSearches, 0)

	stdout, _ = run(t, context.Background(), app, 0, "search", query, "--config", config, "--strategy", "merge")
	obj = envelope(t, stdout, "search", "success")
	assertResultURLs(t, obj, fetchURL)
	assertExecutions(t, obj, executionWant{provider: "tavily", status: "success"},
		executionWant{provider: "exa", status: "empty"})
	assertCalls(t, tavilySearches, 2)
	assertCalls(t, exaSearches, 1)
	assertCalls(t, tavilyFetches, 0)
	assertCalls(t, exaFetches, 0)

	found := obj["results"].([]any)[0].(map[string]any)["url"].(string)
	stdout, _ = run(t, context.Background(), app, 0, "fetch", found, "--config", config, "--providers", "exa")
	obj = fetchResponse(t, stdout, "success", true)
	assertExecutions(t, obj, executionWant{provider: "exa", status: "success"})
	assertCalls(t, tavilyFetches, 0)
	assertCalls(t, exaFetches, 1)
}
