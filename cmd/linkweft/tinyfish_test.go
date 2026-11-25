package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"linkweft/internal/retrieval"
)

const (
	tinyFishHit      = `{"results":[{"url":"https://example.org/article"}]}`
	tinyFishDocument = `{"results":[{"url":"https://example.org/article","text":"fixture body"}],"errors":[]}`
)

var tinyFishOperations = []gatewayFixture{
	{"search", "fixture", "/?page=0&query=fixture", tinyFishHit},
	{"fetch", gatewayArticle, "/", tinyFishDocument},
}

func tinyFishConfig(t *testing.T, serverURL string) string {
	t.Helper()
	return writeCLIConfig(t, fmt.Sprintf("[providers.tinyfish]\napi_key = %q\nbase_url = %q\n", gatewayKey, serverURL))
}

func assertTinyFishSearchRequest(t *testing.T, r *http.Request, query string) {
	t.Helper()
	if r.Method != http.MethodGet || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		t.Error("TinyFish search must use GET without a request body")
	}
	if r.Body != nil {
		if body, err := io.ReadAll(r.Body); err != nil || len(body) != 0 {
			t.Error("TinyFish search must have an empty request body")
		}
	}
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || !reflect.DeepEqual(params, url.Values{"query": {query}, "page": {"0"}}) {
		t.Errorf("search parameters = %v, want the original query and page=0 only", params)
	}
}

func TestTinyFishSearchContract(t *testing.T) {
	isolateProviderKeys(t)
	const query = "  C++ & 中文\t检索  "
	const mixed = `{"results":[
		{"url":"/invalid","position":1},
		{"url":"https://example.org/article","position":9,"title":"Title","snippet":"Summary","content":"ignored"},
		{"url":"https://example.org/missing"},
		{"url":"https://example.org/negative","position":-1},
		{"url":"https://example.org/fraction","position":1.5},
		{"url":"https://example.org/string","position":"2"},
		{"url":"https://example.org/null","position":null},
		{"url":"https://example.org/zero","position":0},
		{"url":"https://example.org/overflow","position":9223372036854775808},
		{"url":"https://example.org/past-limit","position":2}
	]}`
	title, snippet := "Title", "Summary"
	want := []retrieval.Result{{
		URL: gatewayArticle, Title: &title, Snippet: &snippet,
		Sources: []retrieval.Source{{Provider: "tinyfish", URL: gatewayArticle, Rank: 9}},
	}}
	for i, suffix := range []string{"missing", "negative", "fraction", "string", "null", "zero", "overflow"} {
		address := "https://example.org/" + suffix
		want = append(want, retrieval.Result{
			URL: address, Sources: []retrieval.Source{{Provider: "tinyfish", URL: address, Rank: i + 3}},
		})
	}
	for _, tc := range []struct {
		name, body, limit, outcome, status, issue string
		exit                                      int
		results                                   []retrieval.Result
		dropped                                   bool
	}{
		{"validate before limiting", mixed, "2", "success", "success", "", 0, want[:2], true},
		{"position fallback", mixed, "8", "success", "success", "", 0, want, true},
		{"empty", `{"results":[]}`, "2", "empty", "empty", "", 0, nil, false},
		{"missing", `{}`, "2", "error", "error", "invalid_response", 1, nil, false},
		{"null", `{"results":null}`, "2", "error", "error", "invalid_response", 1, nil, false},
		{"wrong type", `{"results":{}}`, "2", "error", "error", "invalid_response", 1, nil, false},
		{"all invalid", `{"results":[{"url":"/relative"}, {"url":"ftp://example.org"}]}`, "2", "error", "error", "invalid_response", 1, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var searches, fetches atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/search/":
					searches.Add(1)
					assertTinyFishSearchRequest(t, r, query)
					if r.Header.Get("X-API-Key") != gatewayKey {
						t.Error("unexpected search authentication")
					}
					io.WriteString(w, tc.body)
				case "/fetch/":
					fetches.Add(1)
				default:
					t.Errorf("unexpected request path: %q", r.URL.Path)
				}
			}))
			defer server.Close()
			path := writeCLIConfig(t, fmt.Sprintf("[providers.tinyfish]\napi_key = %q\nsearch_base_url = %q\nfetch_base_url = %q\n", gatewayKey, server.URL+"/search", server.URL+"/fetch"))
			response, _, stderr := runGatewayCLI(t, newApp(server.Client()), tc.exit,
				"search", query, "--providers", "tinyfish", "--limit", tc.limit, "--config", path)
			assertGatewayExecutions(t, response, gatewayExecution{"tinyfish", tc.status, "", tc.issue})
			if response.Outcome != tc.outcome || response.Results == nil || !slices.EqualFunc(response.Results, tc.results, func(a, b retrieval.Result) bool {
				return reflect.DeepEqual(a, b)
			}) || response.Document != nil || stderr != gatewayWarningOutput(response.Warnings) {
				t.Errorf("unexpected search response: %+v, stderr = %q", response, stderr)
			}
			warnings := []string{"upstream_limit_unavailable"}
			if tc.dropped {
				warnings = append(warnings, "invalid_results_dropped")
			}
			if !slices.EqualFunc(response.Warnings, warnings, func(got retrieval.Warning, want string) bool {
				return got.Code == want && got.Provider != nil && *got.Provider == "tinyfish"
			}) {
				t.Errorf("warnings = %+v, want %v", response.Warnings, warnings)
			}
			if searches.Load() != 1 || fetches.Load() != 0 {
				t.Errorf("requests: search = %d, fetch = %d", searches.Load(), fetches.Load())
			}
		})
	}
}

func TestTinyFishCredentialsAndPreflight(t *testing.T) {
	isolateProviderKeys(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)
	const envKey = "offline-tinyfish-env-key"
	for _, op := range tinyFishOperations {
		t.Run(op.name, func(t *testing.T) {
			for _, tc := range []struct{ name, key, config, issue string }{
				{"environment overrides file", envKey, writeCLIConfig(t, "[providers.tinyfish]\napi_key = '"+gatewayKey+"'\n"), ""},
				{"default config absent", envKey, "", ""},
				{"explicit config absent", envKey, filepath.Join(dir, "absent.toml"), "invalid_config"},
				{"missing credentials", "", "", "missing_credentials"},
				{"invalid base URL", envKey, writeCLIConfig(t, "[providers.tinyfish]\n"+op.name+"_base_url = '/invalid'\n"), "invalid_config"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var calls int
					client := loopbackClient(t, func(r *http.Request, _ http.RoundTripper) (*http.Response, error) {
						calls++
						if r.URL.RequestURI() != op.uri || r.Header.Get("X-API-Key") != envKey || r.Header.Get("Authorization") != "" {
							t.Error("unexpected request URI or effective credential")
						}
						if r.Body != nil {
							defer r.Body.Close()
						}
						w := httptest.NewRecorder()
						io.WriteString(w, op.response)
						return w.Result(), nil
					})
					args := []string{op.name, op.arg, "--providers", "tinyfish"}
					if tc.config != "" {
						args = append(args, "--config", tc.config)
					}
					t.Setenv("TINYFISH_API_KEY", tc.key)
					if tc.issue != "" {
						response, _, stderr := runGatewayCLI(t, newApp(client), 2, args...)
						assertGatewayExecutions(t, response)
						if calls != 0 || response.Outcome != "error" || response.Error == nil || response.Error.Code != tc.issue || stderr != "" {
							t.Errorf("calls = %d, response = %+v, stderr = %q", calls, response, stderr)
						}
						return
					}
					response, _, stderr := runGatewayCLI(t, newApp(client), 0, args...)
					assertGatewayExecutions(t, response, gatewayExecution{provider: "tinyfish", status: "success"})
					if calls != 1 || response.Outcome != "success" || stderr != gatewayWarningOutput(response.Warnings) {
						t.Errorf("calls = %d, response = %+v, stderr = %q", calls, response, stderr)
					}
				})
			}
		})
	}
}

func TestTinyFishFetchContract(t *testing.T) {
	isolateProviderKeys(t)
	const found = "https://example.org/article?a=%2F#section"
	const final = "https://www.example.org/final"
	const content = "# 汉🙂é字\n"
	var searchCalls, fetchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case "/exa/search":
			searchCalls.Add(1)
			fmt.Fprintf(w, `{"results":[{"url":%q}]}`, found)
		case "/tinyfish/":
			fetchCalls.Add(1)
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
				r.Header.Get("X-API-Key") != gatewayKey || r.Header.Get("Authorization") != "" {
				t.Error("unexpected fetch method, content type or authentication")
			}
			assertGatewayRequestJSON(t, r.Body, fmt.Sprintf(`{"urls":[%q],"format":"markdown"}`, found))
			fmt.Fprintf(w, `{"results":[{"url":"https://elsewhere.example","text":"other body"},
				{"url":%q,"final_url":%q,"title":"Article","text":%q}],"errors":[
				{"url":"https://elsewhere.example","error":"bot_blocked"}]}`, found, final, content)
		default:
			t.Errorf("unexpected endpoint %q", r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	path := writeCLIConfig(t, fmt.Sprintf(`[providers.exa]
api_key = %q
base_url = %q
[providers.tinyfish]
api_key = %q
base_url = %q
`, gatewayKey, server.URL+"/exa", gatewayKey, server.URL+"/tinyfish"))
	app := newApp(server.Client())
	search, _, _ := runGatewayCLI(t, app, 0, "search", "fixture", "--providers", "exa", "--config", path)
	if len(search.Results) != 1 || search.Results[0].URL != found {
		t.Fatalf("search results = %+v", search.Results)
	}
	args := []string{"fetch", search.Results[0].URL, "--providers", "tinyfish", "--config", path}
	title, returnedURL, finalURL := "Article", found, final
	want := retrieval.Document{
		RequestedURL: found, ReturnedURL: &returnedURL, FinalURL: &finalURL, Provider: "tinyfish", Title: &title,
		Content: retrieval.Content{Text: content, Provider: "tinyfish", Format: "markdown", Freshness: "unknown"},
	}
	response, _, stderr := runGatewayCLI(t, app, 0, args...)
	assertGatewayExecutions(t, response, gatewayExecution{provider: "tinyfish", status: "success"})
	if response.Outcome != "success" || !reflect.DeepEqual(response.Document, &want) ||
		response.Results == nil || len(response.Results) != 0 || stderr != "" {
		t.Errorf("response = %+v, document = %+v, stderr = %q", response, response.Document, stderr)
	}
	want.Content.Text, want.Content.LocalTruncated = "# 汉🙂é", true
	response, _, _ = runGatewayCLI(t, app, 0, append(args, "--max-chars", "6")...)
	if !reflect.DeepEqual(response.Document, &want) {
		t.Errorf("truncated document = %+v, want %+v", response.Document, want)
	}
	var textOut, textErr bytes.Buffer
	if code := app.Run(context.Background(), append(args, "--output", "text"), &textOut, &textErr); code != 0 ||
		textOut.String() != content || textErr.String() != "tinyfish: success\n" {
		t.Errorf("text exit = %d, stdout = %q, stderr = %q", code, textOut.String(), textErr.String())
	}
	if searchCalls.Load() != 1 || fetchCalls.Load() != 3 {
		t.Errorf("requests: search = %d, fetch = %d", searchCalls.Load(), fetchCalls.Load())
	}
}

func TestTinyFishFetchResponses(t *testing.T) {
	isolateProviderKeys(t)
	const target = `"url":"https://example.org/article"`
	const match = `{` + target + `,"text":"discarded body"}`
	const rawError = "private upstream message"
	envelope := func(results, failures string) string {
		return `{"results":` + results + `,"errors":` + failures + `}`
	}
	for _, tc := range []struct {
		name, body, status, issue string
	}{
		{"missing final URL", tinyFishDocument, "success", ""},
		{"invalid final URL", envelope(`[{`+target+`,"final_url":"/relative","text":"fixture body"}]`, "[]"), "success", ""},
		{"null text", envelope(`[{`+target+`,"text":null}]`, "[]"), "empty", ""},
		{"blank text", envelope(`[{`+target+`,"text":" \t\n"}]`, "[]"), "empty", ""},
		{"missing text", envelope(`[{`+target+`}]`, "[]"), "error", "invalid_response"},
		{"non-string text", envelope(`[{`+target+`,"text":{"value":"text"}}]`, "[]"), "error", "invalid_response"},
		{"missing results", `{"errors":[]}`, "error", "invalid_response"},
		{"missing errors", `{"results":[` + match + `]}`, "error", "invalid_response"},
		{"invalid results", envelope("{}", "[]"), "error", "invalid_response"},
		{"invalid results with target failure", envelope("{}", `[{`+target+`,"error":"bot_blocked"}]`), "error", "invalid_response"},
		{"invalid errors", envelope("["+match+"]", "null"), "error", "invalid_response"},
		{"empty arrays", envelope("[]", "[]"), "error", "invalid_response"},
		{"unrelated URL", envelope(`[{"url":"https://example.org/else","text":"other"}]`,
			`[{"url":"https://example.org/else","error":"bot_blocked"}]`), "error", "invalid_response"},
		{"duplicate matches", envelope("["+match+","+match+"]", "[]"), "error", "invalid_response"},
		{"blocked", envelope("["+match+"]", `[{`+target+`,"error":"bot_blocked"}]`), "error", "content_blocked"},
		{"empty target error code", envelope("["+match+"]", `[{`+target+`,"error":""}]`), "error", "invalid_response"},
		{"blank target error code", envelope("["+match+"]", `[{`+target+`,"error":" \t"}]`), "error", "invalid_response"},
		{"login", envelope("[]", `[{`+target+`,"error":"login_required"}]`), "error", "content_blocked"},
		{"target 401", envelope("[]", `[{`+target+`,"error":"target_http_error","status":401}]`), "error", "content_blocked"},
		{"target 403", envelope("[]", `[{`+target+`,"error":"target_http_error","status":403}]`), "error", "content_blocked"},
		{"target 429", envelope("[]", `[{`+target+`,"error":"target_http_error","status":429}]`), "error", "rate_limited"},
		{"target 503", envelope("[]", `[{`+target+`,"error":"target_http_error","status":503}]`), "error", "upstream_error"},
		{"target timeout", envelope("[]", `[{`+target+`,"error":"timeout"}]`), "timeout", "provider_timeout"},
		{"empty content", envelope("["+match+"]", `[{`+target+`,"error":"empty_content"}]`), "empty", ""},
		{"other error", envelope("[]", fmt.Sprintf(`[{%s,"error":"proxy_error","message":%q}]`, target, rawError+" "+gatewayKey)), "error", "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			exit, outcome := 1, "error"
			if tc.status == "success" {
				exit, outcome = 0, "success"
			}
			response, stdout, stderr := runGatewayCLI(t, newApp(server.Client()), exit, "fetch", gatewayArticle,
				"--providers", "tinyfish", "--config", tinyFishConfig(t, server.URL))
			want := gatewayExecution{provider: "tinyfish", status: tc.status, code: tc.issue}
			if tc.status == "timeout" {
				want.reason = tc.issue
			}
			assertGatewayExecutions(t, response, want)
			if calls.Load() != 1 || response.Outcome != outcome || stderr != "" ||
				strings.Contains(stdout, rawError) || strings.Contains(stdout, gatewayKey) {
				t.Errorf("calls = %d or unexpected response and streams", calls.Load())
			}
			if exit == 0 {
				if response.Document == nil || response.Document.FinalURL != nil || response.Document.Content.Text != "fixture body" {
					t.Errorf("unexpected document: %+v", response.Document)
				}
			} else if response.Document != nil {
				t.Errorf("failed fetch returned a document: %+v", response.Document)
			}
		})
	}
}

func TestTinyFishHTTPFailures(t *testing.T) {
	testProviderHTTPFailures(t, "tinyfish", tinyFishOperations, tinyFishConfig)
}

func TestTinyFishRejectsOversizedResponses(t *testing.T) {
	isolateProviderKeys(t)
	const maxBytes = 16 << 20
	padding := strings.Repeat(" ", maxBytes)
	for _, op := range tinyFishOperations {
		t.Run(op.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.WriteString(w, op.response)
				io.WriteString(w, padding[:maxBytes-len(op.response)+1])
			}))
			defer server.Close()
			response, _, _ := runGatewayCLI(t, newApp(server.Client()), 1, op.name, op.arg,
				"--providers", "tinyfish", "--config", tinyFishConfig(t, server.URL))
			assertGatewayExecutions(t, response, gatewayExecution{provider: "tinyfish", status: "error", code: "invalid_response"})
			if calls.Load() != 1 || response.Outcome != "error" || len(response.Results) != 0 || response.Document != nil {
				t.Errorf("calls = %d, unexpected response: %+v", calls.Load(), response)
			}
		})
	}
}

func TestTinyFishFetchUsesRemainingBudget(t *testing.T) {
	testFetchUsesRemainingBudget(t, "tinyfish", "/")
}

func TestTinyFishSearchMergeCancellation(t *testing.T) {
	isolateProviderKeys(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var calls atomic.Int32
	cancelled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/tavily/search":
			io.WriteString(w, tinyFishHit)
		case "/tinyfish/":
			assertTinyFishSearchRequest(t, r, "fixture")
			cancel(retrieval.ErrUserCancelled)
			select {
			case <-r.Context().Done():
				cancelled <- struct{}{}
			case <-time.After(3 * time.Second):
				t.Error("in-flight GET did not respond to cancellation")
			}
		default:
			t.Errorf("unexpected request path: %q", r.URL.Path)
		}
	}))
	defer server.Close()
	var config strings.Builder
	config.WriteString("[search]\nmax_concurrency = 1\n")
	for _, provider := range []string{"tavily", "tinyfish", "exa"} {
		fmt.Fprintf(&config, "[providers.%s]\napi_key = %q\nbase_url = %q\n", provider, gatewayKey, server.URL+"/"+provider)
	}
	response, _, stderr := runGatewayCLIContext(t, ctx, newApp(server.Client()), 130, "search", "fixture",
		"--config", writeCLIConfig(t, config.String()), "--providers", "tavily,tinyfish,exa", "--strategy", "merge", "--timeout", "5s")
	assertGatewayExecutions(t, response,
		gatewayExecution{provider: "tavily", status: "success"},
		gatewayExecution{"tinyfish", "cancelled", "user_cancelled", "user_cancelled"},
		gatewayExecution{provider: "exa", status: "not_started", reason: "user_cancelled"})
	if response.Outcome != "cancelled" || response.Error == nil || response.Error.Code != "user_cancelled" ||
		len(response.Results) != 1 || response.Results[0].URL != gatewayArticle || stderr != gatewayWarningOutput(response.Warnings) {
		t.Errorf("response = %+v, stderr = %q", response, stderr)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Error("server did not observe request cancellation")
	}
	if calls.Load() != 2 {
		t.Errorf("requests = %d, want 2", calls.Load())
	}
}
