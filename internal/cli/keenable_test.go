package cli_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"linkweft/internal/cli"
	"linkweft/internal/provider"
	"linkweft/internal/retrieval"
)

const keenableKey = "test-only-keenable-key"
const keenableBody = `{"results":[{"url":"https://example.org/provider-result","content":"# 汉🙂é字"}]}`

func addKeenable(t *testing.T, app *cli.App, key, q string, limit int, search, fetch http.HandlerFunc) (*atomic.Int32, *atomic.Int32) {
	t.Helper()
	var searches, fetches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected a JSON POST request")
		}
		if values := r.Header.Values("X-API-Key"); len(values) != 1 || values[0] != key || r.Header.Get("Authorization") != "" {
			t.Error("unexpected Keenable authentication headers")
		}
		switch r.URL.RequestURI() {
		case "/v1/search":
			searches.Add(1)
			assertJSON(t, r.Body, fmt.Sprintf(`{"query":%q,"max_results":%d,"disable_cards":true}`, q, limit))
			if search != nil {
				search(w, r)
				return
			}
		case "/v1/fetch":
			fetches.Add(1)
			assertJSON(t, r.Body, fmt.Sprintf(`{"urls":[%q]}`, fetchURL))
			if fetch != nil {
				fetch(w, r)
				return
			}
		}
		t.Errorf("unexpected Keenable request to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	app.NewKeenable = func(key, _ string) retrieval.SearchProvider {
		return provider.NewKeenable(key, server.Client(), server.URL)
	}
	app.NewKeenableFetch = func(key, _ string) retrieval.FetchProvider {
		return provider.NewKeenableFetch(key, server.Client(), server.URL)
	}
	return &searches, &fetches
}

func TestKeenableSearchContract(t *testing.T) {
	isolateConfig(t)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	const mixed = `{"results":[
		{"url":"ftp://example.org/ignored"},
		{"url":"https://example.org/one","title":"Title","snippet":"Extract","content":"Unused"},
		{"url":"https://example.org/two"},
		{"url":33},
		{"title":"Missing URL"}
	]}`
	for _, tc := range []struct {
		name, query, body, outcome, status, code string
		limit, upstream, exit                    int
		urls, warnings                           []string
	}{
		{"mixed results with capped request", query, mixed, "success", "success", "", 51, 50, 0,
			[]string{"https://example.org/one", "https://example.org/two"}, []string{"upstream_limit_applied", "invalid_results_dropped"}},
		{"URL query with local result limit", fetchURL, mixed, "success", "success", "", 1, 1, 0,
			[]string{"https://example.org/one"}, []string{"invalid_results_dropped"}},
		{"empty array", query, `{"results":[]}`, "empty", "empty", "", 10, 10, 0, nil, nil},
		{"all entries invalid", query, `{"results":[null,{},{"url":"//example.org"}]}`, "error", "error", "invalid_response", 10, 10, 1,
			nil, []string{"invalid_results_dropped"}},
		{"missing results", query, `{}`, "error", "error", "invalid_response", 10, 10, 1, nil, nil},
		{"wrong results type", query, `{"results":{}}`, "error", "error", "invalid_response", 10, 10, 1, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var app cli.App
			searches, fetches := addKeenable(t, &app, keenableKey, tc.query, tc.upstream, bodyResponse(tc.body), nil)
			stdout, stderr := run(t, context.Background(), app, tc.exit, "search", tc.query, "--providers", "keenable", "--limit", fmt.Sprint(tc.limit))
			obj := envelope(t, stdout, "search", tc.outcome)
			assertExecutions(t, obj, executionWant{provider: "keenable", status: tc.status, code: tc.code})
			assertResultURLs(t, obj, tc.urls...)
			if tc.exit == 1 {
				objectCode(t, obj["error"], "search_failed")
			}
			warnings := obj["warnings"].([]any)
			if len(warnings) != len(tc.warnings) {
				t.Fatalf("warnings = %#v, want %v", warnings, tc.warnings)
			}
			for i, code := range tc.warnings {
				warning := warnings[i].(map[string]any)
				if warning["code"] != code || warning["provider"] != "keenable" || !strings.Contains(stderr, code+":") {
					t.Errorf("warning = %#v, stderr = %q", warning, stderr)
				}
			}
			results := obj["results"].([]any)
			if len(results) != len(tc.urls) {
				t.Fatalf("results = %#v, want %d items", results, len(tc.urls))
			}
			for i, value := range results {
				var title, snippet any
				if i == 0 {
					title, snippet = "Title", "Extract"
				}
				want := map[string]any{
					"url": tc.urls[i], "title": title, "snippet": snippet, "content": nil,
					"sources": []any{map[string]any{"provider": "keenable", "url": tc.urls[i], "rank": float64(i + 2)}},
				}
				if !reflect.DeepEqual(value, want) {
					t.Errorf("result = %#v, want %#v", value, want)
				}
			}
			assertCalls(t, searches, 1)
			assertCalls(t, fetches, 0)
		})
	}
}

func TestKeenableFetchContract(t *testing.T) {
	isolateConfig(t)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	for _, tc := range []struct {
		name, fields, freshness string
		truncated, address      any
	}{
		{"cached and truncated", `,"truncated":true,"usage":{"sku":"fetch"}`, "cached", true, returnedURL},
		{"live and untruncated", `,"truncated":false,"usage":{"sku":"fetch.live"}`, "live", false, returnedURL},
		{"missing metadata", "", "unknown", nil, nil},
		{"unrecognized SKU", `,"usage":{"sku":"another.sku"}`, "unknown", nil, returnedURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := `{"content":"# 汉🙂é字","title":"Article"`
			if tc.address != nil {
				result += fmt.Sprintf(`,"url":%q`, tc.address)
			}
			var app cli.App
			searches, fetches := addKeenable(t, &app, keenableKey, query, 10, nil, bodyResponse(`{"results":[`+result+tc.fields+`}]}`))
			args := fetchArgs("--providers", "keenable", "--max-chars", "4")
			stdout, stderr := run(t, context.Background(), app, 0, args...)
			obj := fetchResponse(t, stdout, "success", true)
			assertExecutions(t, obj, executionWant{provider: "keenable", status: "success"})
			want := map[string]any{
				"requested_url": fetchURL, "returned_url": tc.address, "final_url": tc.address,
				"provider": "keenable", "title": "Article",
				"content": map[string]any{"format": "markdown", "text": "# 汉🙂", "provider": "keenable",
					"local_truncated": true, "upstream_truncated": tc.truncated, "freshness": tc.freshness},
			}
			if !reflect.DeepEqual(obj["document"], want) || stderr != "" {
				t.Errorf("document = %#v, want %#v; stderr = %q", obj["document"], want, stderr)
			}
			assertCalls(t, fetches, 1)

			stdout, stderr = run(t, context.Background(), app, 0, append(args, "--output", "text")...)
			if stdout != "# 汉🙂\n" || stderr != "keenable: success\n" {
				t.Errorf("unexpected text stdout = %q, stderr = %q", stdout, stderr)
			}
			assertCalls(t, searches, 0)
			assertCalls(t, fetches, 2)
		})
	}
}

func TestKeenableCredentialsAndPreflight(t *testing.T) {
	isolateConfig(t)
	config := testConfig(t, "[providers.keenable]\napi_key = '"+fileKey+"'")
	for _, tc := range []struct{ name, env, path, key string }{
		{"file only", "", config, fileKey},
		{"environment overrides file", keenableKey, config, keenableKey},
		{"environment with absent default config", keenableKey, "", keenableKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEENABLE_API_KEY", tc.env)
			var app cli.App
			searches, fetches := addKeenable(t, &app, tc.key, query, 10, bodyResponse(`{"results":[]}`), bodyResponse(keenableBody))
			options := []string{"--providers", "keenable"}
			if tc.path != "" {
				options = append(options, "--config", tc.path)
			}
			stdout, _ := run(t, context.Background(), app, 0, append([]string{"search", query}, options...)...)
			assertExecutions(t, envelope(t, stdout, "search", "empty"), executionWant{provider: "keenable", status: "empty"})
			stdout, _ = run(t, context.Background(), app, 0, fetchArgs(options...)...)
			assertExecutions(t, fetchResponse(t, stdout, "success", true), executionWant{provider: "keenable", status: "success"})
			assertCalls(t, searches, 1)
			assertCalls(t, fetches, 1)
		})
	}
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	tavily, tavilySearch := testApp(t, testKey, 10, bodyResponse(`{"results":[]}`))
	exa, exaSearch := exaApp(t, exaKey, 10, bodyResponse(`{"results":[]}`))
	app, tavilyFetch, exaFetch := fetchApp(t, nil, nil)
	app.NewTavily, app.NewExa = tavily.NewTavily, exa.NewExa
	searches, fetches := addKeenable(t, &app, keenableKey, query, 10, nil, nil)
	for _, tc := range []struct {
		name, key, code string
		extra           []string
	}{
		{"missing credentials", "", "missing_credentials", nil},
		{"missing explicit config", keenableKey, "invalid_config", []string{"--config", filepath.Join(t.TempDir(), "absent.toml")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEENABLE_API_KEY", tc.key)
			for _, command := range [][]string{{"search", query}, {"fetch", fetchURL}} {
				args := append(command, "--providers", "tavily,keenable,exa")
				stdout, _ := run(t, context.Background(), app, 2, append(args, tc.extra...)...)
				obj := envelope(t, stdout, command[0], "error")
				objectCode(t, obj["error"], tc.code)
				assertExecutions(t, obj)
			}
			for _, calls := range []*atomic.Int32{tavilySearch, exaSearch, searches, tavilyFetch, exaFetch, fetches} {
				assertCalls(t, calls, 0)
			}
		})
	}
}

func TestKeenableFetchFailures(t *testing.T) {
	isolateConfig(t)
	t.Setenv("KEENABLE_API_KEY", keenableKey)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	const privateMessage = untrustedError + keenableKey + " " + fileKey
	config := testConfig(t, "[providers.keenable]\napi_key = '"+fileKey+"'")
	pageError := func(status int, code string) string {
		return fmt.Sprintf(`{"results":[{"content":"must not be returned","error":{"status":%d,"code":%q,"message":%q}}]}`, status, code, privateMessage)
	}
	httpError := func(code string) string {
		return fmt.Sprintf(`{"code":%q,"error":%q,"message":%q}`, code, privateMessage, privateMessage)
	}
	for _, tc := range []struct {
		name, body, code string
		status           int
		fallback         bool
	}{
		{"blank content", `{"results":[{"content":" \n\t"}]}`, "", 200, true},
		{"blank content without fallback", `{"results":[{"content":" \n\t"}]}`, "", 200, false},
		{"page error overrides content", pageError(403, "target_blocked"), "content_blocked", 200, true},
		{"page rate limited", pageError(429, "target_rate_limited"), "rate_limited", 200, true},
		{"unknown page error", pageError(418, "teapot"), "upstream_error", 200, false},
		{"missing array", `{}`, "invalid_response", 200, false},
		{"null array", `{"results":null}`, "invalid_response", 200, false},
		{"wrong array type", `{"results":{}}`, "invalid_response", 200, false},
		{"zero pages", `{"results":[]}`, "invalid_response", 200, false},
		{"two pages", `{"results":[{},{}]}`, "invalid_response", 200, false},
		{"null page", `{"results":[null]}`, "invalid_response", 200, false},
		{"page blocked", httpError("target_blocked"), "content_blocked", 403, true},
		{"private URL blocked", httpError("private_url_not_allowed"), "content_blocked", 403, false},
		{"credentials forbidden", httpError("credentials_forbidden"), "auth_failed", 403, true},
		{"paywall", httpError("target_paywalled"), "content_blocked", 422, false},
		{"unauthorized", httpError("missing_api_key"), "auth_failed", 401, false},
		{"insufficient credits", httpError("insufficient_credits"), "upstream_error", 402, false},
		{"rate limited", httpError("rate_limit_exceeded"), "rate_limited", 429, true},
		{"upstream failure", httpError("internal_error"), "upstream_error", 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, tavily, exa := fetchApp(t, bodyResponse(tavilyBody), bodyResponse(exaBody))
			searches, fetches := addKeenable(t, &app, keenableKey, query, 10, nil, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			selected, outcome, exit, fallbackCalls := "keenable", "error", 1, int32(0)
			status := "error"
			if tc.code == "" {
				status = "empty"
			}
			want := []executionWant{{provider: "keenable", status: status, code: tc.code}}
			if tc.fallback {
				selected, outcome, exit, fallbackCalls = "keenable,tavily,exa", "success", 0, 1
				want = append(want, executionWant{provider: "tavily", status: "success"},
					executionWant{"exa", "not_started", "fallback_satisfied", ""})
			}
			args := fetchArgs("--providers", selected, "--config", config)
			stdout, stderr := run(t, context.Background(), app, exit, args...)
			obj := fetchResponse(t, stdout, outcome, tc.fallback)
			assertExecutions(t, obj, want...)
			if tc.fallback {
				if doc := obj["document"].(map[string]any); doc["provider"] != "tavily" {
					t.Errorf("unexpected fallback document: %#v", doc)
				}
			} else {
				objectCode(t, obj["error"], "fetch_failed")
			}
			assertKeenableOutputControlled(t, stdout, stderr)
			assertCalls(t, fetches, 1)
			assertCalls(t, tavily, fallbackCalls)

			stdout, stderr = run(t, context.Background(), app, exit, append(args, "--output", "text")...)
			diagnostic := "keenable: " + status
			if tc.code != "" {
				diagnostic += ": " + tc.code
			}
			wantText := ""
			if tc.fallback {
				wantText = "# 汉🙂é字\n"
			}
			if stdout != wantText || !strings.Contains(stderr, diagnostic) {
				t.Errorf("unexpected text stdout = %q, stderr = %q", stdout, stderr)
			}
			assertKeenableOutputControlled(t, stdout, stderr)
			assertCalls(t, fetches, 2)
			assertCalls(t, tavily, 2*fallbackCalls)
			assertCalls(t, searches, 0)
			assertCalls(t, exa, 0)
		})
	}
}

func assertKeenableOutputControlled(t *testing.T, stdout, stderr string) {
	t.Helper()
	for _, private := range []string{untrustedError, keenableKey, fileKey, "must not be returned"} {
		if strings.Contains(stdout, private) || strings.Contains(stderr, private) {
			t.Error("output exposed upstream error data, failed page content, or a fixture credential")
		}
	}
}
