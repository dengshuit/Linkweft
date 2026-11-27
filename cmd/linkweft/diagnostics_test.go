package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExaSearchSnippets(t *testing.T) {
	isolateProviderKeys(t)
	for _, tc := range []struct {
		name, setting, highlights, snippet string
		requested                          bool
	}{
		{"default", "", `["First excerpt", "Second excerpt"]`, "First excerpt\nSecond excerpt", true},
		{"enabled", "search_snippets = true", `["Excerpt"]`, "Excerpt", true},
		{"disabled", "search_snippets = false", `["Unrequested excerpt"]`, "", false},
		{"missing", "", `null`, "", true},
		{"empty", "", `["", "  "]`, "", true},
		{"malformed", "", `42`, "", true},
		{"credential filtered", "", `["` + gatewayKey + `"]`, "", true},
	} {
		for _, strategy := range []string{"fallback", "merge"} {
			t.Run(tc.name+"/"+strategy, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/search" {
						t.Errorf("unexpected request path %q", r.URL.Path)
					}
					assertGatewayRequestJSON(t, r.Body, fmt.Sprintf(`{"query":"fixture","numResults":10,"type":"auto","contents":{"text":false,"highlights":%t}}`, tc.requested))
					fmt.Fprintf(w, `{"results":[{"url":%q,"highlights":%s,"text":"Full text"}]}`, gatewayArticle, tc.highlights)
				}))
				defer server.Close()
				path := writeCLIConfig(t, fmt.Sprintf("[providers.exa]\napi_key = %q\nbase_url = %q\n%s\n", gatewayKey, server.URL, tc.setting))
				response, _, _ := runGatewayCLI(t, newApp(nil), 0, "search", "fixture", "--providers", "exa", "--strategy", strategy, "--config", path)
				if len(response.Results) != 1 || response.Results[0].URL != gatewayArticle || calls.Load() != 1 {
					t.Fatalf("unexpected results or call count: %+v, calls=%d", response.Results, calls.Load())
				}
				snippet := response.Results[0].Snippet
				if tc.snippet == "" {
					if snippet != nil {
						t.Fatalf("expected null snippet, got %q", *snippet)
					}
				} else if snippet == nil || *snippet != tc.snippet {
					t.Fatalf("snippet = %v, want %q", snippet, tc.snippet)
				}
				if response.Results[0].Content != nil {
					t.Fatal("unexpected full content in search result")
				}
			})
		}
	}
}

func TestExaSnippetConfigValidation(t *testing.T) {
	isolateProviderKeys(t)
	for _, settings := range []string{
		"[providers.exa]\nsearch_snippets = 'false'",
		"[providers.tavily]\nsearch_snippets = false",
	} {
		path := writeCLIConfig(t, settings)
		for _, op := range []struct{ name, arg string }{{"search", "fixture"}, {"fetch", gatewayArticle}} {
			response, _, _ := runGatewayCLI(t, newApp(nil), 2, op.name, op.arg, "--config", path)
			if response.Error == nil || response.Error.Code != "invalid_config" || len(response.Executions) != 0 {
				t.Fatalf("unexpected config validation response: %+v", response)
			}
		}
	}
}

func TestCLITargetHTTPDiagnostics(t *testing.T) {
	isolateProviderKeys(t)
	for _, tc := range []struct{ provider, body string }{
		{"exa", fmt.Sprintf(`{"results":[{"text":"blocked"}],"statuses":[{"id":%q,"status":"error","error":{"httpStatusCode":403}}]}`, gatewayArticle)},
		{"firecrawl", `{"success":true,"data":{"markdown":"blocked","metadata":{"statusCode":403}}}`},
		{"tinyfish", fmt.Sprintf(`{"results":[],"errors":[{"url":%q,"error":"target_http_error","status":403}]}`, gatewayArticle)},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			path := writeCLIConfig(t, fmt.Sprintf("[providers.%s]\napi_key = %q\nbase_url = %q\n", tc.provider, gatewayKey, server.URL))
			response, _, _ := runGatewayCLI(t, newApp(nil), 1, "fetch", gatewayArticle, "--providers", tc.provider, "--config", path)
			issue := response.Executions[0].Error
			if issue == nil || issue.Code != "content_blocked" || issue.HTTPStatus != nil || issue.TargetHTTPStatus == nil || *issue.TargetHTTPStatus != 403 {
				t.Fatalf("unexpected target-page diagnostics: %+v", issue)
			}
		})
	}
}

func TestCLIProviderHTTPErrorMapping(t *testing.T) {
	isolateProviderKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"code":"SAFE_MODE_BLOCKED"}`)
	}))
	defer server.Close()
	path := writeCLIConfig(t, fmt.Sprintf("[providers.firecrawl]\napi_key = %q\nbase_url = %q\n", gatewayKey, server.URL))
	response, _, _ := runGatewayCLI(t, newApp(nil), 1, "fetch", gatewayArticle, "--providers", "firecrawl", "--config", path)
	issue := response.Executions[0].Error
	if issue == nil || issue.Code != "content_blocked" || issue.HTTPStatus == nil || *issue.HTTPStatus != 403 || issue.RetryAfterMS == nil || *issue.RetryAfterMS != 7000 {
		t.Fatalf("unexpected provider-specific HTTP diagnostics: %+v", issue)
	}
}

func TestCLIHTTPDiagnostics(t *testing.T) {
	isolateProviderKeys(t)
	for _, op := range []struct{ name, argument, timeout string }{
		{"search", "fixture", "200ms"}, {"fetch", gatewayArticle, "300ms"},
	} {
		for _, tc := range []struct {
			name, retryAfter, code string
			status                 int
			retryMS                any
		}{
			{"unavailable", "7", "upstream_error", 503, float64(7000)},
			{"limited", "0", "rate_limited", 429, float64(0)},
			{"invalid retry", "invalid", "upstream_error", 503, nil},
			{"unauthorized", "", "auth_failed", 401, nil},
		} {
			t.Run(op.name+"/"+tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", tc.retryAfter)
					w.WriteHeader(tc.status)
					io.WriteString(w, gatewayKey)
				}))
				defer server.Close()
				path := writeCLIConfig(t, fmt.Sprintf("[providers.tavily]\napi_key = %q\nbase_url = %q\n%s_timeout = %q\n", gatewayKey, server.URL, op.name, op.timeout))
				_, stdout, _ := runGatewayCLI(t, newApp(nil), 1, op.name, op.argument, "--providers", "tavily", "--timeout", "5s", "--config", path)
				var response struct {
					Executions []struct {
						EffectiveTimeoutMS *int64         `json:"effective_timeout_ms"`
						Error              map[string]any `json:"error"`
					} `json:"executions"`
				}
				if err := json.Unmarshal([]byte(stdout), &response); err != nil || len(response.Executions) != 1 {
					t.Fatalf("unexpected response %q: %v", stdout, err)
				}
				execution := response.Executions[0]
				wantTimeout := int64(200)
				if op.name == "fetch" {
					wantTimeout = 300
				}
				if execution.EffectiveTimeoutMS == nil || *execution.EffectiveTimeoutMS != wantTimeout {
					t.Errorf("effective timeout = %v, want %d", execution.EffectiveTimeoutMS, wantTimeout)
				}
				if execution.Error["code"] != tc.code || execution.Error["http_status"] != float64(tc.status) || execution.Error["retry_after_ms"] != tc.retryMS {
					t.Errorf("unexpected HTTP diagnostics: %+v", execution.Error)
				}
				if strings.Contains(stdout, gatewayKey) {
					t.Fatal("response contains a credential")
				}
			})
		}
	}
}
