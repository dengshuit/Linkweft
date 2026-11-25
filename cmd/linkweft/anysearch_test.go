package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	anySearchHit      = `{"code":0,"data":{"results":[{"url":"https://example.org/article"}]}}`
	anySearchEmpty    = `{"code":0,"data":{"results":[]}}`
	anySearchDocument = `{"code":0,"data":{"url":"https://example.org/article","content":"fixture body"}}`
)

type gatewayFixture struct{ name, arg, uri, response string }

var anySearchOperations = []gatewayFixture{
	{"search", "fixture", "/v1/search", anySearchHit},
	{"fetch", gatewayArticle, "/v1/extract", anySearchDocument},
}

type gatewayExecution struct{ provider, status, reason, code string }

func assertGatewayExecutions(t *testing.T, response retrieval.Response, want ...gatewayExecution) {
	t.Helper()
	if response.Executions == nil || len(response.Executions) != len(want) {
		t.Fatalf("executions = %+v, want %v", response.Executions, want)
	}
	for i, got := range response.Executions {
		var reason, code string
		if got.Reason != nil {
			reason = *got.Reason
		}
		if got.Error != nil {
			code = got.Error.Code
		}
		if actual := (gatewayExecution{got.Provider, got.Status, reason, code}); actual != want[i] {
			t.Errorf("execution %d = %v, want %v", i, actual, want[i])
		}
		uncalled := got.Status == "skipped" || got.Status == "not_started"
		if (got.DurationMS == nil) != uncalled || (got.DurationMS != nil && *got.DurationMS < 0) {
			t.Errorf("unexpected duration for %s: %+v", got.Provider, got)
		}
	}
}

func anySearchConfig(t *testing.T, baseURL string) string {
	t.Helper()
	return writeCLIConfig(t, fmt.Sprintf("[providers.anysearch]\napi_key = %q\nbase_url = %q\n", gatewayKey, baseURL))
}

func TestAnySearchSearchContract(t *testing.T) {
	isolateProviderKeys(t)
	const mixed = `{"code":0,"data":{"results":[
		{"url":"ftp://example.org/ignored"},
		{"url":"https://example.org/article","title":"Title","snippet":"Summary","content":"  Body\n"},
		{"url":7},
		{"url":"https://example.org/next","title":"","content":" \t"}
	]}}`
	for _, tc := range []struct {
		name, body, outcome, status, issue string
		exit                               int
		drops                              bool
	}{
		{"mixed results", mixed, "success", "success", "", 0, true},
		{"empty results", anySearchEmpty, "empty", "empty", "", 0, false},
		{"invalid URLs", `{"code":0,"data":{"results":[{"url":"/relative"},{"url":12}]}}`, "error", "error", "invalid_response", 1, true},
		{"no results field", `{"code":0,"data":{}}`, "error", "error", "invalid_response", 1, false},
		{"null results", `{"code":0,"data":{"results":null}}`, "error", "error", "invalid_response", 1, false},
		{"nonarray results", `{"code":0,"data":{"results":{}}}`, "error", "error", "invalid_response", 1, false},
		{"missing data", `{"code":0}`, "error", "error", "invalid_response", 1, false},
		{"null data", `{"code":0,"data":null}`, "error", "error", "invalid_response", 1, false},
		{"nonobject data", `{"code":0,"data":[]}`, "error", "error", "invalid_response", 1, false},
		{"missing code", `{"data":{"results":[]}}`, "error", "error", "invalid_response", 1, false},
		{"null code", `{"code":null,"data":{"results":[]}}`, "error", "error", "invalid_response", 1, false},
		{"string code", `{"code":"0","data":{"results":[]}}`, "error", "error", "invalid_response", 1, false},
		{"noninteger code", `{"code":0.0,"data":{"results":[]}}`, "error", "error", "invalid_response", 1, false},
		{"failure with results", `{"code":-1,"data":{"results":[{"url":"https://example.org/article"}]}}`, "error", "error", "upstream_error", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.RequestURI() != "/v1/search" {
					t.Errorf("unexpected search path: %s", r.URL.RequestURI())
				}
				assertGatewayRequestJSON(t, r.Body, `{"query":"fixture","max_results":10}`)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			response, _, stderr := runGatewayCLI(t, newApp(server.Client()), tc.exit,
				"search", "fixture", "--providers", "anysearch", "--limit", "11", "--config", anySearchConfig(t, server.URL))
			assertGatewayExecutions(t, response, gatewayExecution{provider: "anysearch", status: tc.status, code: tc.issue})
			if calls.Load() != 1 || response.Outcome != tc.outcome || response.Results == nil || response.Document != nil {
				t.Fatalf("calls = %d, response = %+v", calls.Load(), response)
			}
			warnings := []string{"upstream_limit_applied"}
			if tc.drops {
				warnings = append(warnings, "invalid_results_dropped")
			}
			if len(response.Warnings) != len(warnings) {
				t.Fatalf("warnings = %+v, want %v", response.Warnings, warnings)
			}
			for i, want := range warnings {
				warning := response.Warnings[i]
				if warning.Code != want || warning.Provider == nil || *warning.Provider != "anysearch" || !strings.Contains(stderr, want) {
					t.Errorf("warning = %+v, stderr = %q", warning, stderr)
				}
			}
			if tc.outcome != "success" {
				if len(response.Results) != 0 {
					t.Errorf("unexpected results: %+v", response.Results)
				}
				return
			}
			title, snippet := "Title", "Summary"
			want := []retrieval.Result{
				{URL: gatewayArticle, Title: &title, Snippet: &snippet,
					Content: &retrieval.Content{Text: "  Body\n", Provider: "anysearch", Format: "unknown", Freshness: "unknown"},
					Sources: []retrieval.Source{{Provider: "anysearch", URL: gatewayArticle, Rank: 2}}},
				{URL: "https://example.org/next", Sources: []retrieval.Source{{Provider: "anysearch", URL: "https://example.org/next", Rank: 4}}},
			}
			if !reflect.DeepEqual(response.Results, want) {
				t.Errorf("results = %+v, want %+v", response.Results, want)
			}
		})
	}
}

func TestAnySearchSearchLimitBelowCap(t *testing.T) {
	isolateProviderKeys(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assertGatewayRequestJSON(t, r.Body, `{"query":"fixture","max_results":3}`)
		io.WriteString(w, anySearchHit)
	}))
	defer server.Close()
	response, _, stderr := runGatewayCLI(t, newApp(server.Client()), 0,
		"search", "fixture", "--providers", "anysearch", "--limit", "3", "--config", anySearchConfig(t, server.URL))
	assertGatewayExecutions(t, response, gatewayExecution{provider: "anysearch", status: "success"})
	if calls.Load() != 1 || response.Outcome != "success" || len(response.Warnings) != 0 || stderr != "" {
		t.Errorf("calls = %d, response = %+v, stderr = %q", calls.Load(), response, stderr)
	}
}

func TestAnySearchCredentialsAndPreflight(t *testing.T) {
	isolateProviderKeys(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)
	const envKey = "offline-anysearch-env-key"
	for _, op := range anySearchOperations {
		t.Run(op.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, key, config, issue string
			}{
				{"environment overrides file", envKey, anySearchConfig(t, "https://api.anysearch.com"), ""},
				{"default config absent", envKey, "", ""},
				{"explicit config absent", envKey, filepath.Join(dir, "absent.toml"), "invalid_config"},
				{"missing credentials", "", "", "missing_credentials"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Setenv("ANYSEARCH_API_KEY", tc.key)
					calls := 0
					client := loopbackClient(t, func(r *http.Request, _ http.RoundTripper) (*http.Response, error) {
						defer r.Body.Close()
						calls++
						if r.URL.String() != "https://api.anysearch.com"+op.uri || r.Header.Get("Authorization") != "Bearer "+envKey {
							t.Error("unexpected default URL or effective credential")
						}
						w := httptest.NewRecorder()
						io.WriteString(w, op.response)
						return w.Result(), nil
					})
					args := []string{op.name, op.arg, "--providers", "anysearch"}
					if tc.config != "" {
						args = append(args, "--config", tc.config)
					}
					if tc.issue != "" {
						response, _, stderr := runGatewayCLI(t, newApp(client), 2, args...)
						assertGatewayExecutions(t, response)
						if calls != 0 || response.Outcome != "error" || response.Error == nil || response.Error.Code != tc.issue || stderr != "" {
							t.Errorf("calls = %d, response = %+v, stderr = %q", calls, response, stderr)
						}
						return
					}
					response, _, stderr := runGatewayCLI(t, newApp(client), 0, args...)
					assertGatewayExecutions(t, response, gatewayExecution{provider: "anysearch", status: "success"})
					if calls != 1 || response.Outcome != "success" || stderr != "" {
						t.Errorf("calls = %d, response = %+v, stderr = %q", calls, response, stderr)
					}
				})
			}
		})
	}
}

func TestAnySearchFetchContract(t *testing.T) {
	isolateProviderKeys(t)
	const found = "https://example.org/article?a=%2F#section"
	const returned = "https://example.org/gateway/article"
	const content = "  汉🙂é字\n[Content truncated]\n"
	var paths []string
	client := loopbackClient(t, func(r *http.Request, network http.RoundTripper) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		return network.RoundTrip(r)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case "/search":
			fmt.Fprintf(w, `{"results":[{"url":%q}]}`, found)
		case "/v1/extract":
			assertGatewayRequestJSON(t, r.Body, fmt.Sprintf(`{"url":%q}`, found))
			fmt.Fprintf(w, `{"code":0,"data":{"url":%q,"title":"Article","content":%q}}`, returned, content)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	path := writeCLIConfig(t, fmt.Sprintf(`[providers.exa]
api_key = %q
base_url = %q
[providers.anysearch]
api_key = %q
base_url = %q
`, gatewayKey, server.URL, gatewayKey, server.URL))
	app := newApp(client)
	search, _, _ := runGatewayCLI(t, app, 0, "search", "fixture", "--providers", "exa", "--config", path)
	if len(search.Results) != 1 || search.Results[0].URL != found {
		t.Fatalf("search result = %+v", search.Results)
	}
	args := []string{"fetch", search.Results[0].URL, "--providers", "anysearch", "--config", path}
	title, returnedURL := "Article", returned
	want := retrieval.Document{
		RequestedURL: found, ReturnedURL: &returnedURL, Provider: "anysearch", Title: &title,
		Content: retrieval.Content{Text: content, Provider: "anysearch", Format: "unknown", Freshness: "unknown"},
	}
	response, _, stderr := runGatewayCLI(t, app, 0, args...)
	assertGatewayExecutions(t, response, gatewayExecution{provider: "anysearch", status: "success"})
	if response.Outcome != "success" || !reflect.DeepEqual(response.Document, &want) ||
		response.Results == nil || len(response.Results) != 0 || stderr != "" {
		t.Errorf("response = %+v, document = %+v, stderr = %q", response, response.Document, stderr)
	}
	want.Content.Text = "  汉🙂é"
	want.Content.LocalTruncated = true
	response, _, _ = runGatewayCLI(t, app, 0, append(args, "--max-chars", "6")...)
	if !reflect.DeepEqual(response.Document, &want) {
		t.Errorf("truncated document = %+v, want %+v", response.Document, want)
	}
	var textOut, textErr bytes.Buffer
	if code := app.Run(context.Background(), append(args, "--output", "text"), &textOut, &textErr); code != 0 ||
		textOut.String() != content || textErr.String() != "anysearch: success\n" {
		t.Errorf("text exit = %d, stdout = %q, stderr = %q", code, textOut.String(), textErr.String())
	}
	if want := []string{"/search", "/v1/extract", "/v1/extract", "/v1/extract"}; !slices.Equal(paths, want) {
		t.Errorf("requests = %v, want %v", paths, want)
	}
}

func TestAnySearchFetchResponses(t *testing.T) {
	isolateProviderKeys(t)
	for _, tc := range []struct{ name, body, status, issue string }{
		{"missing content", `{"code":0,"data":{}}`, "empty", ""},
		{"blank content", `{"code":0,"data":{"content":" \t\n\u3000"}}`, "empty", ""},
		{"null content", `{"code":0,"data":{"content":null}}`, "error", "invalid_response"},
		{"wrong content type", `{"code":0,"data":{"content":{}}}`, "error", "invalid_response"},
		{"nonzero code wins", `{"code":-1,"message":"private fixture text","data":{"content":"failure body"}}`, "error", "upstream_error"},
		{"extract failed wins", `{"code":0,"error_code":"extract_failed","message":"private fixture text","data":{"content":"failure body"}}`, "error", "upstream_error"},
		{"missing returned URL", `{"code":0,"data":{"content":"fixture body"}}`, "success", ""},
		{"invalid returned URL", `{"code":0,"data":{"url":"/relative","content":"fixture body"}}`, "success", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/extract" {
					t.Errorf("wrong fetch path: %s", r.URL.Path)
				}
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			exit := 1
			if tc.status == "success" {
				exit = 0
			}
			response, stdout, stderr := runGatewayCLI(t, newApp(server.Client()), exit, "fetch", gatewayArticle,
				"--providers", "anysearch", "--config", anySearchConfig(t, server.URL))
			assertGatewayExecutions(t, response, gatewayExecution{provider: "anysearch", status: tc.status, code: tc.issue})
			if calls.Load() != 1 {
				t.Errorf("fetch calls = %d, want 1", calls.Load())
			}
			if tc.status == "success" {
				doc := response.Document
				if response.Outcome != "success" || doc == nil || doc.RequestedURL != gatewayArticle || doc.ReturnedURL != nil ||
					doc.FinalURL != nil || !strings.Contains(stdout, `"returned_url":null`) {
					t.Errorf("unexpected success response: %s", stdout)
				}
				return
			}
			if response.Outcome != "error" || response.Document != nil ||
				response.Error == nil || response.Error.Code != "fetch_failed" || strings.Contains(stdout+stderr, "private fixture text") ||
				strings.Contains(stdout+stderr, "failure body") {
				t.Errorf("unexpected failure response: stdout = %q, stderr = %q", stdout, stderr)
			}
		})
	}
}

func TestAnySearchHTTPFailures(t *testing.T) {
	testProviderHTTPFailures(t, "anysearch", anySearchOperations, anySearchConfig)
}

func testProviderHTTPFailures(t *testing.T, provider string, operations []gatewayFixture, config func(*testing.T, string) string) {
	t.Helper()
	isolateProviderKeys(t)
	const marker, username, password, apiKey = "private quota response", "fake-generated-account", "fake-generated-password", "fake-generated-api-key"
	for _, op := range operations {
		for _, tc := range []struct {
			status int
			issue  string
		}{
			{http.StatusUnauthorized, "auth_failed"}, {http.StatusForbidden, "auth_failed"},
			{http.StatusPaymentRequired, "upstream_error"}, {http.StatusUnprocessableEntity, "upstream_error"},
			{http.StatusTooManyRequests, "rate_limited"}, {http.StatusBadGateway, "upstream_error"},
			{http.StatusTemporaryRedirect, "upstream_error"},
		} {
			t.Run(fmt.Sprintf("%s/%d", op.name, tc.status), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.RequestURI() != op.uri {
						t.Errorf("unexpected request to %s", r.URL.RequestURI())
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Location", "/redirect-target")
					w.WriteHeader(tc.status)
					if tc.status == http.StatusUnprocessableEntity {
						io.WriteString(w, `{"code":-1,"error_code":"extract_failed"}`)
						return
					}
					fmt.Fprintf(w, `{"code":-1,"message":%q}`, strings.Join([]string{marker, username, password, apiKey, gatewayKey}, " "))
				}))
				defer server.Close()
				app := newApp(server.Client())
				args := []string{op.name, op.arg, "--providers", provider, "--config", config(t, server.URL)}
				response, stdout, stderr := runGatewayCLI(t, app, 1, args...)
				assertGatewayExecutions(t, response, gatewayExecution{provider: provider, status: "error", code: tc.issue})
				if calls.Load() != 1 || response.Outcome != "error" || stderr != gatewayWarningOutput(response.Warnings) {
					t.Errorf("calls = %d, response = %+v, stderr = %q", calls.Load(), response, stderr)
				}
				outputs := stdout + stderr
				if tc.status == http.StatusPaymentRequired {
					var textOut, textErr bytes.Buffer
					if code := app.Run(context.Background(), append(args, "--output", "text"), &textOut, &textErr); code != 1 ||
						calls.Load() != 2 || textOut.Len() != 0 || !strings.Contains(textErr.String(), provider+": error: upstream_error:") {
						t.Errorf("text exit = %d, calls = %d or unexpected streams", code, calls.Load())
					}
					outputs += textOut.String() + textErr.String()
				}
				for _, private := range []string{marker, username, password, apiKey, gatewayKey} {
					if strings.Contains(outputs, private) {
						t.Error("CLI output contains upstream error text or credentials")
					}
				}
			})
		}
	}
}

func TestAnySearchFetchUsesRemainingBudget(t *testing.T) {
	testFetchUsesRemainingBudget(t, "anysearch", "/v1/extract")
}

func testFetchUsesRemainingBudget(t *testing.T, provider, providerPath string) {
	t.Helper()
	isolateProviderKeys(t)
	var calls []string
	client := loopbackClient(t, func(r *http.Request, network http.RoundTripper) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		if r.URL.Path == "/contents" {
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) >= 550*time.Millisecond {
				t.Error("fallback did not inherit the remaining command budget")
			}
		}
		return network.RoundTrip(r)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("request did not end with its context")
		}
	}))
	defer server.Close()
	path := writeCLIConfig(t, fmt.Sprintf(`[fetch]
timeout = '700ms'
[providers.%s]
api_key = %q
base_url = %q
fetch_timeout = '300ms'
[providers.exa]
api_key = %q
base_url = %q
[providers.tavily]
api_key = %q
base_url = %q
`, provider, gatewayKey, server.URL, gatewayKey, server.URL, gatewayKey, server.URL))
	response, _, _ := runGatewayCLI(t, newApp(client), 1, "fetch", gatewayArticle, "--config", path, "--providers", provider+",exa,tavily")
	assertGatewayExecutions(t, response,
		gatewayExecution{provider, "timeout", "provider_timeout", "provider_timeout"},
		gatewayExecution{"exa", "timeout", "command_timeout", "command_timeout"},
		gatewayExecution{provider: "tavily", status: "not_started", reason: "command_timeout"})
	if want := []string{providerPath, "/contents"}; !slices.Equal(calls, want) {
		t.Errorf("requests = %v, want %v", calls, want)
	}
}
