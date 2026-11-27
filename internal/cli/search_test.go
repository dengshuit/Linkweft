package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"linkweft/internal/cli"
	"linkweft/internal/provider"
	"linkweft/internal/retrieval"
)

const (
	query   = "local acceptance query"
	testKey = "test-only-tavily-key"
	fileKey = "test-only-file-key"
)

func isolateConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)
	for _, name := range []string{"TAVILY", "EXA", "KEENABLE", "FIRECRAWL", "ANYSEARCH", "TINYFISH"} {
		t.Setenv(name+"_API_KEY", "")
	}
}

func testConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testApp(t *testing.T, key string, maxResults int, handle http.HandlerFunc) (cli.App, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/search" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("unexpected Authorization header")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected JSON request")
		}
		assertJSON(t, r.Body, fmt.Sprintf(`{
			"query":%q,"max_results":%d,"search_depth":"basic","topic":"general",
			"auto_parameters":false,"include_answer":false,"include_raw_content":false,
			"include_images":false,"include_image_descriptions":false
		}`, query, maxResults))
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	app := cli.App{NewTavily: func(apiKey, _ string) retrieval.SearchProvider {
		return provider.NewTavily(apiKey, server.Client(), server.URL)
	}}
	return app, &calls
}

func searchArgs(options ...string) []string {
	return append([]string{"search", query, "--providers", "tavily"}, options...)
}

func run(t *testing.T, ctx context.Context, app cli.App, code int, args ...string) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if got := app.Run(ctx, args, &stdout, &stderr); got != code {
		t.Fatalf("exit = %d, want %d; stdout = %q; stderr = %q", got, code, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

func decode(t *testing.T, input io.Reader) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(input)
	var obj map[string]any
	if err := decoder.Decode(&obj); err != nil || obj == nil {
		t.Errorf("expected a JSON object: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Errorf("expected end of JSON stream, got %v", err)
	}
	return obj
}

func assertJSON(t *testing.T, input io.Reader, want string) {
	t.Helper()
	got := decode(t, input)
	expected := decode(t, strings.NewReader(want))
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("JSON = %#v, want %#v", got, expected)
	}
}

func envelope(t *testing.T, stdout string, operation any, outcome string) map[string]any {
	t.Helper()
	obj := decode(t, strings.NewReader(stdout))
	if len(obj) != 8 || obj["schema_version"] != float64(1) || obj["operation"] != operation || obj["outcome"] != outcome {
		t.Fatalf("unexpected envelope: %#v", obj)
	}
	if doc, ok := obj["document"]; !ok || doc != nil {
		t.Errorf("expected null document: %#v", obj)
	}
	for _, key := range []string{"results", "executions", "warnings"} {
		if _, ok := obj[key].([]any); !ok {
			t.Fatalf("%s must be an array: %#v", key, obj[key])
		}
	}
	if issue, ok := obj["error"]; !ok || ((outcome == "success" || outcome == "empty") && issue != nil) {
		t.Errorf("unexpected error field: %#v", obj)
	}
	return obj
}

func objectCode(t *testing.T, value any, want string) {
	t.Helper()
	entry, ok := value.(map[string]any)
	message, _ := entry["message"].(string)
	if !ok || entry["code"] != want || message == "" {
		t.Errorf("expected %q with a message, got %#v", want, value)
	}
}

func execution(t *testing.T, obj map[string]any, status string, reason any, errorCode string) {
	t.Helper()
	items := obj["executions"].([]any)
	if len(items) != 1 {
		t.Fatalf("executions = %#v, want one record", items)
	}
	entry, ok := items[0].(map[string]any)
	if !ok || len(entry) != 6 || entry["provider"] != "tavily" || entry["status"] != status || entry["reason"] != reason {
		t.Fatalf("unexpected execution: %#v", items[0])
	}
	if duration, ok := entry["duration_ms"].(float64); !ok || duration < 0 {
		t.Errorf("expected a nonnegative duration: %#v", entry)
	}
	if budget, ok := entry["effective_timeout_ms"].(float64); !ok || budget < 0 {
		t.Errorf("expected a nonnegative effective timeout: %#v", entry)
	}
	if errorCode != "" {
		objectCode(t, entry["error"], errorCode)
	} else if entry["error"] != nil {
		t.Errorf("expected null execution error: %#v", entry)
	}
}

func assertCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	if got := calls.Load(); got != want {
		t.Errorf("HTTP requests = %d, want %d", got, want)
	}
}

func TestSearchPreflight(t *testing.T) {
	isolateConfig(t)
	app, calls := testApp(t, testKey, 10, func(_ http.ResponseWriter, _ *http.Request) {})
	t.Run("AC06 missing credentials", func(t *testing.T) {
		stdout, _ := run(t, context.Background(), app, 2, searchArgs()...)
		response := envelope(t, stdout, "search", "error")
		objectCode(t, response["error"], "missing_credentials")
		if len(response["executions"].([]any)) != 0 {
			t.Errorf("unexpected executions: %#v", response["executions"])
		}
		assertCalls(t, calls, 0)
	})
	t.Setenv("TAVILY_API_KEY", testKey)
	tests := []struct {
		name string
		args []string
		op   any
		code string
	}{
		{"AC36 missing explicit config", searchArgs("--config", filepath.Join(t.TempDir(), "absent.toml")), "search", "invalid_config"},
		{"empty config path", searchArgs("--config="), "search", "invalid_config"},
		{"invalid TOML", searchArgs("--config", testConfig(t, "[providers.tavily]\napi_key = "+fileKey)), "search", "invalid_config"},
		{"unknown field", searchArgs("--config", testConfig(t, "[providers.tavily]\napi_ky = '"+fileKey+"'")), "search", "invalid_config"},
		{"unknown configured provider", searchArgs("--config", testConfig(t, "[providers.unknown]\napi_key = '"+fileKey+"'")), "search", "invalid_config"},
		{"duplicate priority", searchArgs("--config", testConfig(t, "[search]\npriority = ['tavily', 'tavily']")), "search", "invalid_config"},
		{"zero config limit", searchArgs("--config", testConfig(t, "[search]\nlimit = 0")), "search", "invalid_config"},
		{"uppercase TOML limit", searchArgs("--config", testConfig(t, "[search]\nLIMIT = 1")), "search", "invalid_config"},
		{"TOML limit alias overrides zero", searchArgs("--config", testConfig(t, "[search]\nlimit = 0\nLIMIT = 1")), "search", "invalid_config"},
		{"uppercase TOML section", searchArgs("--config", testConfig(t, "[Search]\nlimit = 1")), "search", "invalid_config"},
		{"uppercase TOML inline key", searchArgs("--config", testConfig(t, "[providers]\ntavily = { API_KEY = '"+fileKey+"' }")), "search", "invalid_config"},
		{"invalid provider timeout", searchArgs("--config", testConfig(t, "[providers.tavily]\nsearch_timeout = '0s'")), "search", "invalid_config"},
		{"AC37 blank query", []string{"search", " \t"}, "search", "invalid_argument"},
		{"missing query", []string{"search"}, "search", "invalid_argument"},
		{"extra query", searchArgs("extra"), "search", "invalid_argument"},
		{"duplicate provider", []string{"search", query, "--providers", "tavily, tavily"}, "search", "invalid_argument"},
		{"empty provider", []string{"search", query, "--providers", "tavily, "}, "search", "invalid_argument"},
		{"unknown provider", []string{"search", query, "--providers", "unknown"}, "search", "invalid_argument"},
		{"unsupported explicit provider", []string{"search", query, "--providers", "tavily, tinyfish"}, "search", "unsupported_capability"},
		{"invalid strategy", searchArgs("--strategy", "single"), "search", "invalid_argument"},
		{"invalid output", searchArgs("--output", "xml"), "search", "invalid_argument"},
		{"unknown option", searchArgs("--unknown"), "search", "invalid_argument"},
		{"wrong operation option", searchArgs("--max-chars", "10"), "search", "invalid_argument"},
		{"nonpositive timeout", searchArgs("--timeout", "0s"), "search", "invalid_argument"},
		{"missing timeout unit", searchArgs("--timeout", "12"), "search", "invalid_argument"},
		{"nonpositive limit", searchArgs("--limit", "-1"), "search", "invalid_argument"},
		{"fractional limit", searchArgs("--limit", "1.5"), "search", "invalid_argument"},
		{"unknown operation", []string{"unknown"}, nil, "invalid_argument"},
		{"unknown help topic", []string{"help", fileKey}, nil, "invalid_argument"},
		{"hidden shell completion", []string{"__complete", "search", "--unknown=probe-argument", ""}, nil, "invalid_argument"},
		{"hidden shell completion alias", []string{"__completeNoDesc", "search", "--unknown=probe-argument", ""}, nil, "invalid_argument"},
		{"completion spelling as a query", []string{"search", "__complete", "--config", ""}, "search", "invalid_config"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			stdout, stderr := run(t, context.Background(), app, 2, tc.args...)
			response := envelope(t, stdout, tc.op, "error")
			objectCode(t, response["error"], tc.code)
			for _, key := range []string{"results", "executions", "warnings"} {
				if len(response[key].([]any)) != 0 {
					t.Errorf("expected empty %s, got %#v", key, response[key])
				}
			}
			if strings.Contains(stdout+stderr, fileKey) {
				t.Error("configuration error exposed a test credential")
			}
			if strings.Contains(stdout+stderr, "probe-argument") {
				t.Error("output echoed the unknown argument")
			}
			assertCalls(t, calls, before)
		})
	}
}

func TestSearchResults(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	tests := []struct {
		name, body, outcome, status, issue string
		options                            []string
		code, maxResults                   int
		results, warning                   string
	}{
		{
			name: "AC09 AC36 empty success with no default config", body: `{"results":[]}`,
			outcome: "empty", status: "empty", maxResults: 10, results: "[]",
		},
		{
			name: "AC11 AC13 AC28 mixed results preserve rank and nulls",
			body: `{"results":[{"url":"ftp://example.com/invalid"},null,{"url":"https://example.com/a"},
				{"url":"http://example.org/b","title":"Title","content":"Snippet","raw_content":"not requested"}]}`,
			outcome: "success", status: "success", maxResults: 10,
			results: `[
				{"url":"https://example.com/a","title":null,"snippet":null,"content":null,
				"sources":[{"provider":"tavily","url":"https://example.com/a","rank":3}]},
				{"url":"http://example.org/b","title":"Title","snippet":"Snippet","content":null,
				"sources":[{"provider":"tavily","url":"http://example.org/b","rank":4}]}
			]`, warning: "invalid_results_dropped",
		},
		{
			name: "AC12 all URLs invalid", body: `{"results":[{"url":"http://:80"}, {"url":"/relative"}, {"url":"https://broken%zz"}]}`,
			outcome: "error", status: "error", issue: "invalid_response", code: 1, maxResults: 10,
			results: "[]", warning: "invalid_results_dropped",
		},
		{
			name: "malformed optional fields retain valid URLs and ranks",
			body: `{"results":[{"url":"/invalid"},{"url":"https://example.com/a","title":123,"content":"Snippet"},
				{"url":"https://example.org/b","title":"Title","content":{"text":"wrong type"}}]}`,
			outcome: "success", status: "success", maxResults: 10,
			results: `[
				{"url":"https://example.com/a","title":null,"snippet":"Snippet","content":null,
				"sources":[{"provider":"tavily","url":"https://example.com/a","rank":2}]},
				{"url":"https://example.org/b","title":"Title","snippet":null,"content":null,
				"sources":[{"provider":"tavily","url":"https://example.org/b","rank":3}]}
			]`, warning: "invalid_results_dropped",
		},
		{
			name: "AC16 cap is disclosed", body: `{"results":[]}`,
			options: []string{"--limit", "70"}, outcome: "empty", status: "empty", maxResults: 20,
			results: "[]", warning: "upstream_limit_applied",
		},
		{
			name: "local limit counts only valid entries", body: `{"results":[{},{"url":"https://example.com/a"},{"url":"https://example.com/b"}]}`,
			options: []string{"--limit", "1"}, outcome: "success", status: "success", maxResults: 1,
			results: `[{"url":"https://example.com/a","title":null,"snippet":null,"content":null,
				"sources":[{"provider":"tavily","url":"https://example.com/a","rank":2}]}]`, warning: "invalid_results_dropped",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, calls := testApp(t, testKey, tc.maxResults, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.body)
			})
			stdout, _ := run(t, context.Background(), app, tc.code, searchArgs(tc.options...)...)
			response := envelope(t, stdout, "search", tc.outcome)
			execution(t, response, tc.status, nil, tc.issue)
			if tc.issue != "" {
				objectCode(t, response["error"], "search_failed")
			}
			var expected []any
			if err := json.Unmarshal([]byte(tc.results), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(response["results"], expected) {
				t.Errorf("results = %#v, want %#v", response["results"], expected)
			}
			warnings := response["warnings"].([]any)
			if tc.warning == "" && len(warnings) != 0 || tc.warning != "" && len(warnings) != 1 {
				t.Fatalf("unexpected warnings: %#v", warnings)
			}
			if tc.warning != "" {
				objectCode(t, warnings[0], tc.warning)
				if warnings[0].(map[string]any)["provider"] != "tavily" {
					t.Errorf("missing warning provider: %#v", warnings[0])
				}
			}
			assertCalls(t, calls, 1)
		})
	}
}

func TestSearchProtectsCredentialsInSuccessfulResults(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	app, calls := testApp(t, testKey, 10, func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		encodedKey := strings.ReplaceAll(key, "-", "%2D")
		fmt.Fprintf(w, `{"results":[
			{"url":%q},
			{"url":%q},
			{"url":"https://example.org/safe","title":%q,"content":%q}
		]}`, "https://example.org/unsafe?key="+key, "https://example.org/unsafe?key="+encodedKey, "title "+key, "snippet "+key)
	})
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), searchArgs("--output", format), &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if strings.Contains(stdout.String()+stderr.String(), testKey) || strings.Contains(stdout.String(), "/unsafe") {
				t.Fatal("successful response exposed a credential or its URL")
			}
			if !strings.Contains(stdout.String(), "https://example.org/safe") {
				t.Error("response lost the safe URL")
			}
			if format == "json" {
				response := envelope(t, stdout.String(), "search", "success")
				results := response["results"].([]any)
				if len(results) != 1 {
					t.Fatalf("got %d results, want 1", len(results))
				}
				want := map[string]any{
					"url": "https://example.org/safe", "title": nil, "snippet": nil, "content": nil,
					"sources": []any{map[string]any{"provider": "tavily", "url": "https://example.org/safe", "rank": float64(3)}},
				}
				if !reflect.DeepEqual(results[0], want) {
					t.Error("expected the original safe URL and rank with null title and snippet")
				}
			}
		})
	}
	assertCalls(t, calls, 2)
}

func TestSearchDoesNotFetchResultURL(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	app, calls := testApp(t, testKey, 10, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"results":[{"url":%q}]}`, "http://"+r.Host+"/article")
	})
	stdout, _ := run(t, context.Background(), app, 0, searchArgs()...)
	response := envelope(t, stdout, "search", "success")
	execution(t, response, "success", nil, "")
	assertCalls(t, calls, 1)
}

func TestSearchConfigurationPrecedence(t *testing.T) {
	isolateConfig(t)
	path := testConfig(t, `[search]
priority = ["tinyfish"]
limit = 3
timeout = "4s"

[providers.tinyfish]
api_key = "test-only-tinyfish-key"

[providers.tavily]
api_key = "`+fileKey+`"
search_timeout = "2s"
`)
	t.Run("use file configuration and skip unsupported automatic candidate", func(t *testing.T) {
		app, calls := testApp(t, fileKey, 3, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"results":[]}`)
		})
		stdout, _ := run(t, context.Background(), app, 0, "search", query, "--config", path)
		response := envelope(t, stdout, "search", "empty")
		items := response["executions"].([]any)
		wantSkip := map[string]any{"provider": "tinyfish", "status": "skipped", "duration_ms": nil, "reason": "unsupported_capability", "error": nil}
		if len(items) != 2 || !reflect.DeepEqual(items[0], wantSkip) {
			t.Fatalf("unexpected executions: %#v", items)
		}
		response["executions"] = items[1:]
		execution(t, response, "empty", nil, "")
		assertCalls(t, calls, 1)
	})
	t.Run("AC29 environment key and explicit limit win", func(t *testing.T) {
		t.Setenv("TAVILY_API_KEY", testKey)
		app, calls := testApp(t, testKey, 5, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, "untrusted error with %s and %s", testKey, fileKey)
		})
		stdout, stderr := run(t, context.Background(), app, 1, searchArgs("--config", path, "--limit", "5")...)
		response := envelope(t, stdout, "search", "error")
		execution(t, response, "error", nil, "auth_failed")
		objectCode(t, response["error"], "search_failed")
		for _, secret := range []string{testKey, fileKey, "untrusted error"} {
			if strings.Contains(stdout+stderr, secret) {
				t.Error("output exposed upstream error content")
			}
		}
		assertCalls(t, calls, 1)
	})
	t.Run("blank environment key does not fall back to the file", func(t *testing.T) {
		t.Setenv("TAVILY_API_KEY", " ")
		app, calls := testApp(t, fileKey, 3, func(_ http.ResponseWriter, _ *http.Request) {})
		stdout, _ := run(t, context.Background(), app, 2, searchArgs("--config", path)...)
		response := envelope(t, stdout, "search", "error")
		objectCode(t, response["error"], "missing_credentials")
		assertCalls(t, calls, 0)
	})
}

func TestSearchUpstreamErrors(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	tests := []struct {
		name, body, code string
		status           int
	}{
		{"AC10 AC26 rate limit", "untrusted error " + testKey, "rate_limited", http.StatusTooManyRequests},
		{"AC10 AC26 server error", "untrusted error " + testKey, "upstream_error", http.StatusServiceUnavailable},
		{"forbidden", "untrusted error " + testKey, "auth_failed", http.StatusForbidden},
		{"redirect refused", "", "upstream_error", http.StatusTemporaryRedirect},
		{"invalid JSON", "untrusted error " + testKey, "invalid_response", http.StatusOK},
		{"missing array", `{"message":"untrusted error ` + testKey + `"}`, "invalid_response", http.StatusOK},
		{"null array", `{"results":null}`, "invalid_response", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, calls := testApp(t, testKey, 10, func(w http.ResponseWriter, r *http.Request) {
				if tc.status == http.StatusTemporaryRedirect && r.URL.Path == "/search" {
					w.Header().Set("Location", "/redirected")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			stdout, stderr := run(t, context.Background(), app, 1, searchArgs()...)
			response := envelope(t, stdout, "search", "error")
			execution(t, response, "error", nil, tc.code)
			objectCode(t, response["error"], "search_failed")
			if strings.Contains(stdout+stderr, testKey) || strings.Contains(stdout+stderr, "untrusted error") {
				t.Error("output exposed upstream error content")
			}
			assertCalls(t, calls, 1)
		})
	}
}

func TestSearchBudgetsAndCancellation(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	tests := []struct {
		name, flag, command, platform, reason string
	}{
		{"AC26 command deadline overrides file", "200ms", "5s", "4s", "command_timeout"},
		{"AC26 provider deadline precedes command", "4s", "50ms", "200ms", "provider_timeout"},
		{"AC26 command deadline from file", "", "200ms", "4s", "command_timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := testConfig(t, fmt.Sprintf("[search]\ntimeout = %q\n[providers.tavily]\nsearch_timeout = %q", tc.command, tc.platform))
			options := []string{"--config", path}
			if tc.flag != "" {
				options = append(options, "--timeout", tc.flag)
			}
			app, calls := testApp(t, testKey, 10, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"results":`)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
					t.Error("request context was not cancelled")
				}
			})
			stdout, _ := run(t, context.Background(), app, 1, searchArgs(options...)...)
			response := envelope(t, stdout, "search", "error")
			execution(t, response, "timeout", tc.reason, tc.reason)
			objectCode(t, response["error"], "search_failed")
			assertCalls(t, calls, 1)
		})
	}
	t.Run("user cancellation reaches in-flight HTTP request", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		app, calls := testApp(t, testKey, 10, func(_ http.ResponseWriter, r *http.Request) {
			cancel(retrieval.ErrUserCancelled)
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
				t.Error("request context was not cancelled")
			}
		})
		stdout, _ := run(t, ctx, app, 130, searchArgs()...)
		response := envelope(t, stdout, "search", "cancelled")
		execution(t, response, "cancelled", "user_cancelled", "user_cancelled")
		objectCode(t, response["error"], "user_cancelled")
		assertCalls(t, calls, 1)
	})
}

func TestSearchTextExitCodes(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	tests := []struct {
		name, body, text, diagnostic string
		status, exit                 int
		options                      []string
	}{
		{"success", `{"results":[{"url":"https://example.com","title":"Title","content":"Snippet"}]}`, "Title\nhttps://example.com\nSnippet", "", http.StatusOK, 0, nil},
		{"empty", `{"results":[]}`, "No results.", "", http.StatusOK, 0, nil},
		{"error", "", "", "search_failed", http.StatusTooManyRequests, 1, nil},
		{"invalid", "", "", "invalid_argument", http.StatusOK, 2, []string{"--timeout", "0s"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, calls := testApp(t, testKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			args := searchArgs(tc.options...)
			run(t, context.Background(), app, tc.exit, args...)
			stdout, stderr := run(t, context.Background(), app, tc.exit, append(args, "--output", "text")...)
			if tc.text == "" && stdout != "" || tc.text != "" && !strings.Contains(stdout, tc.text) || json.Valid([]byte(stdout)) {
				t.Errorf("unexpected text output: %q", stdout)
			}
			if tc.diagnostic != "" && (!strings.Contains(stderr, tc.diagnostic) || strings.Contains(stdout, tc.diagnostic)) {
				t.Errorf("diagnostic %q should be written to stderr, got stdout=%q stderr=%q", tc.diagnostic, stdout, stderr)
			}
			if tc.exit == 1 && !strings.Contains(stderr, "tavily: error: rate_limited") {
				t.Errorf("missing provider diagnostic in stderr: %q", stderr)
			}
			if tc.exit == 2 {
				assertCalls(t, calls, 0)
			} else {
				assertCalls(t, calls, 2)
			}
		})
	}
}
