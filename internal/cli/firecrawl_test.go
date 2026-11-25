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

const firecrawlKey = "test-only-firecrawl-key"
const firecrawlBody = `{"success":true,"data":{"markdown":"# 汉🙂é字"}}`
const firecrawlEmpty = `{"success":true,"data":{}}`
const firecrawlFailureContent = "failed page fixture"

func addFirecrawl(t *testing.T, app *cli.App, key string, limit int, search, scrape http.HandlerFunc) (*atomic.Int32, *atomic.Int32) {
	t.Helper()
	var searches, scrapes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected a JSON POST request")
		}
		if values := r.Header.Values("Authorization"); len(values) != 1 || values[0] != "Bearer "+key || r.Header.Get("X-API-Key") != "" {
			t.Error("unexpected Firecrawl authentication headers")
		}
		switch r.URL.RequestURI() {
		case "/v2/search":
			searches.Add(1)
			assertJSON(t, r.Body, fmt.Sprintf(`{"query":%q,"limit":%d,"sources":["web"],"highlights":false}`, query, limit))
			if search != nil {
				search(w, r)
				return
			}
		case "/v2/scrape":
			scrapes.Add(1)
			assertJSON(t, r.Body, fmt.Sprintf(`{"url":%q,"formats":["markdown"],"onlyMainContent":true}`, fetchURL))
			if scrape != nil {
				scrape(w, r)
				return
			}
		}
		t.Error("unexpected Firecrawl request")
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	app.NewFirecrawl = func(key, _ string) retrieval.SearchProvider {
		return provider.NewFirecrawl(key, server.Client(), server.URL)
	}
	app.NewFirecrawlFetch = func(key, _ string) retrieval.FetchProvider {
		return provider.NewFirecrawlFetch(key, server.Client(), server.URL)
	}
	return &searches, &scrapes
}

func TestFirecrawlSearchContract(t *testing.T) {
	isolateConfig(t)
	t.Setenv("FIRECRAWL_API_KEY", firecrawlKey)
	const mixed = `{"success":true,"data":{"web":[
		{"url":"ftp://example.org/ignored","position":1},
		{"url":"https://example.org/one","position":7,"title":"Title","description":"Snippet","markdown":"Unused"},
		{"url":33,"position":2},
		{"url":"https://example.org/two","position":0,"title":"","description":"  "},
		{"url":"https://example.org/three","position":-2},
		{"url":"https://example.org/four","position":2.5},
		{"url":"https://example.org/five","position":"3"},
		{"url":"https://example.org/six"}
	]}}`
	t.Run("cap, rank and optional fields", func(t *testing.T) {
		var app cli.App
		searches, scrapes := addFirecrawl(t, &app, firecrawlKey, 100, bodyResponse(mixed), nil)
		stdout, stderr := run(t, context.Background(), app, 0, "search", query, "--providers", "firecrawl", "--limit", "101")
		obj := envelope(t, stdout, "search", "success")
		assertExecutions(t, obj, executionWant{provider: "firecrawl", status: "success"})
		urls := []string{"https://example.org/one", "https://example.org/two", "https://example.org/three",
			"https://example.org/four", "https://example.org/five", "https://example.org/six"}
		ranks := []float64{7, 4, 5, 6, 7, 8}
		assertResultURLs(t, obj, urls...)
		for i, result := range obj["results"].([]any) {
			var title, snippet any
			if i == 0 {
				title, snippet = "Title", "Snippet"
			}
			want := map[string]any{
				"url": urls[i], "title": title, "snippet": snippet, "content": nil,
				"sources": []any{map[string]any{"provider": "firecrawl", "url": urls[i], "rank": ranks[i]}},
			}
			if !reflect.DeepEqual(result, want) {
				t.Errorf("result = %#v, want %#v", result, want)
			}
		}
		assertFirecrawlWarnings(t, obj, stderr, "upstream_limit_applied", "invalid_results_dropped")
		assertCalls(t, searches, 1)
		assertCalls(t, scrapes, 0)
	})

	for _, tc := range []struct {
		name, body, code string
		discarded        bool
	}{
		{"empty array", `{"success":true,"data":{"web":[]}}`, "", false},
		{"omitted web", firecrawlEmpty, "", false},
		{"missing success", `{"data":{"web":[]}}`, "invalid_response", false},
		{"null success", `{"success":null,"data":{"web":[]}}`, "invalid_response", false},
		{"wrong success type", `{"success":"true","data":{"web":[]}}`, "invalid_response", false},
		{"unsuccessful", `{"success":false,"data":{"web":[{"url":"https://example.org/ignored"}]}}`, "upstream_error", false},
		{"missing data", `{"success":true}`, "invalid_response", false},
		{"null data", `{"success":true,"data":null}`, "invalid_response", false},
		{"wrong data type", `{"success":true,"data":[]}`, "invalid_response", false},
		{"null web", `{"success":true,"data":{"web":null}}`, "invalid_response", false},
		{"wrong web type", `{"success":true,"data":{"web":{}}}`, "invalid_response", false},
		{"all entries invalid", `{"success":true,"data":{"web":[null,{},{"url":"https://example.org:65536/page"}]}}`, "invalid_response", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var app cli.App
			searches, scrapes := addFirecrawl(t, &app, firecrawlKey, 10, bodyResponse(tc.body), nil)
			outcome, exit := "empty", 0
			if tc.code != "" {
				outcome, exit = "error", 1
			}
			stdout, stderr := run(t, context.Background(), app, exit, "search", query, "--providers", "firecrawl")
			obj := envelope(t, stdout, "search", outcome)
			assertResultURLs(t, obj)
			assertExecutions(t, obj, executionWant{provider: "firecrawl", status: outcome, code: tc.code})
			if exit != 0 {
				objectCode(t, obj["error"], "search_failed")
			}
			var warnings []string
			if tc.discarded {
				warnings = append(warnings, "invalid_results_dropped")
			}
			assertFirecrawlWarnings(t, obj, stderr, warnings...)
			assertCalls(t, searches, 1)
			assertCalls(t, scrapes, 0)
		})
	}
}

func assertFirecrawlWarnings(t *testing.T, obj map[string]any, stderr string, codes ...string) {
	t.Helper()
	warnings := obj["warnings"].([]any)
	if len(warnings) != len(codes) {
		t.Fatalf("warnings = %#v, want %v", warnings, codes)
	}
	for i, code := range codes {
		warning := warnings[i].(map[string]any)
		if warning["code"] != code || warning["provider"] != "firecrawl" || !strings.Contains(stderr, code+":") {
			t.Errorf("warning = %#v, stderr = %q", warning, stderr)
		}
	}
}

func TestFirecrawlFetchContract(t *testing.T) {
	isolateConfig(t)
	t.Setenv("FIRECRAWL_API_KEY", firecrawlKey)
	for _, tc := range []struct {
		name, metadata, freshness string
		address, title            any
	}{
		{"cache hit and redirected URL", fmt.Sprintf(`,"metadata":{"url":%q,"title":"Article","cacheState":"hit","statusCode":200}`, returnedURL), "cached", returnedURL, "Article"},
		{"cache miss and source URL only", fmt.Sprintf(`,"metadata":{"sourceURL":%q,"cacheState":"miss"}`, fetchURL), "live", nil, nil},
		{"missing metadata", "", "unknown", nil, nil},
		{"unknown cache state and invalid URL", `,"metadata":{"url":"https://example.org:65536/page","cacheState":"other","title":""}`, "unknown", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var app cli.App
			body := `{"success":true,"data":{"markdown":"# 汉🙂é字"` + tc.metadata + "}}"
			searches, scrapes := addFirecrawl(t, &app, firecrawlKey, 10, nil, bodyResponse(body))
			args := fetchArgs("--providers", "firecrawl", "--max-chars", "4")
			stdout, stderr := run(t, context.Background(), app, 0, args...)
			obj := fetchResponse(t, stdout, "success", true)
			assertExecutions(t, obj, executionWant{provider: "firecrawl", status: "success"})
			want := map[string]any{
				"requested_url": fetchURL, "returned_url": tc.address, "final_url": tc.address, "provider": "firecrawl", "title": tc.title,
				"content": map[string]any{"format": "markdown", "text": "# 汉🙂", "provider": "firecrawl",
					"local_truncated": true, "upstream_truncated": nil, "freshness": tc.freshness},
			}
			if !reflect.DeepEqual(obj["document"], want) || stderr != "" {
				t.Errorf("document = %#v, want %#v, stderr = %q", obj["document"], want, stderr)
			}
			assertCalls(t, scrapes, 1)
			stdout, stderr = run(t, context.Background(), app, 0, append(args, "--output", "text")...)
			if stdout != "# 汉🙂\n" || stderr != "firecrawl: success\n" {
				t.Errorf("unexpected text stdout = %q, stderr = %q", stdout, stderr)
			}
			assertCalls(t, scrapes, 2)
			assertCalls(t, searches, 0)
		})
	}
}

func TestFirecrawlCredentialsAndPreflight(t *testing.T) {
	isolateConfig(t)
	config := testConfig(t, "[providers.firecrawl]\napi_key = '"+fileKey+"'")
	for _, tc := range []struct{ name, env, path, key string }{
		{"file only", "", config, fileKey},
		{"environment overrides file", firecrawlKey, config, firecrawlKey},
		{"environment with absent default config", firecrawlKey, "", firecrawlKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FIRECRAWL_API_KEY", tc.env)
			var app cli.App
			searches, scrapes := addFirecrawl(t, &app, tc.key, 10, bodyResponse(firecrawlEmpty), bodyResponse(firecrawlBody))
			opts := []string{"--providers", "firecrawl"}
			if tc.path != "" {
				opts = append(opts, "--config", tc.path)
			}
			stdout, _ := run(t, context.Background(), app, 0, append([]string{"search", query}, opts...)...)
			assertExecutions(t, envelope(t, stdout, "search", "empty"), executionWant{provider: "firecrawl", status: "empty"})
			stdout, _ = run(t, context.Background(), app, 0, fetchArgs(opts...)...)
			assertExecutions(t, fetchResponse(t, stdout, "success", true), executionWant{provider: "firecrawl", status: "success"})
			assertCalls(t, searches, 1)
			assertCalls(t, scrapes, 1)
		})
	}

	configureFourProviders(t)
	app, searchCalls, fetchCalls := fourProviderApp(t, [4]http.HandlerFunc{}, [4]http.HandlerFunc{})
	for _, tc := range []struct {
		name, key, code string
		options         []string
	}{
		{"missing credentials", "", "missing_credentials", nil},
		{"missing explicit config", firecrawlKey, "invalid_config", []string{"--config", filepath.Join(t.TempDir(), "absent.toml")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FIRECRAWL_API_KEY", tc.key)
			for _, command := range [][]string{{"search", query}, {"fetch", fetchURL}} {
				args := append(command, "--providers", "tavily,exa,keenable,firecrawl")
				stdout, _ := run(t, context.Background(), app, 2, append(args, tc.options...)...)
				obj := envelope(t, stdout, command[0], "error")
				objectCode(t, obj["error"], tc.code)
				assertExecutions(t, obj)
			}
			searchCalls.assert(t, [4]int32{})
			fetchCalls.assert(t, [4]int32{})
		})
	}
}

func assertFirecrawlOutputControlled(t *testing.T, stdout, stderr string) {
	t.Helper()
	for _, private := range []string{untrustedError, firecrawlKey, fileKey, firecrawlFailureContent} {
		if strings.Contains(stdout, private) || strings.Contains(stderr, private) {
			t.Error("output exposed upstream error data, failed page content, or a fixture credential")
		}
	}
}

func TestFirecrawlFetchFailures(t *testing.T) {
	isolateConfig(t)
	configureFourProviders(t)
	message := untrustedError + firecrawlKey + " " + fileKey
	config := testConfig(t, "[providers.firecrawl]\napi_key = '"+fileKey+"'")
	targetError := func(status int, detail string) string {
		return fmt.Sprintf(`{"success":true,"data":{"markdown":%q,"metadata":{"statusCode":%d,"error":%q}}}`,
			firecrawlFailureContent, status, detail)
	}
	apiError := func(code string) string {
		return fmt.Sprintf(`{"success":false,"code":%q,"error":%q,"data":{"markdown":%q}}`, code, message, firecrawlFailureContent)
	}
	for _, tc := range []struct {
		name, body, code string
		status           int
		fallback         bool
	}{
		{"missing markdown", firecrawlEmpty, "", 200, false},
		{"blank markdown", `{"success":true,"data":{"markdown":" \t\n\u00a0"}}`, "", 200, true},
		{"unsuccessful response", apiError("OTHER"), "upstream_error", 200, true},
		{"nested error with successful page status", targetError(200, message), "upstream_error", 200, false},
		{"target requires login", targetError(401, ""), "content_blocked", 200, false},
		{"target forbidden", targetError(403, message), "content_blocked", 200, true},
		{"target rate limited", targetError(429, ""), "rate_limited", 200, true},
		{"target unavailable", targetError(503, message), "upstream_error", 200, true},
		{"missing success", `{"data":{"markdown":"fixture"}}`, "invalid_response", 200, false},
		{"null success", `{"success":null,"data":{"markdown":"fixture"}}`, "invalid_response", 200, false},
		{"wrong success type", `{"success":1,"data":{"markdown":"fixture"}}`, "invalid_response", 200, false},
		{"missing data", `{"success":true}`, "invalid_response", 200, false},
		{"null data", `{"success":true,"data":null}`, "invalid_response", 200, false},
		{"wrong data type", `{"success":true,"data":[]}`, "invalid_response", 200, false},
		{"null markdown", `{"success":true,"data":{"markdown":null}}`, "invalid_response", 200, false},
		{"wrong markdown type", `{"success":true,"data":{"markdown":[]}}`, "invalid_response", 200, false},
		{"wrong metadata type", `{"success":true,"data":{"markdown":"fixture","metadata":[]}}`, "invalid_response", 200, false},
		{"wrong metadata field type", `{"success":true,"data":{"markdown":"fixture","metadata":{"statusCode":"403"}}}`, "invalid_response", 200, false},
		{"null target status", fmt.Sprintf(`{"success":true,"data":{"markdown":%q,"metadata":{"statusCode":null}}}`, firecrawlFailureContent), "invalid_response", 200, true},
		{"null target error", fmt.Sprintf(`{"success":true,"data":{"markdown":%q,"metadata":{"error":null}}}`, firecrawlFailureContent), "invalid_response", 200, true},
		{"API unauthorized", apiError("UNAUTHORIZED"), "auth_failed", 401, false},
		{"API forbidden", apiError("FORBIDDEN"), "auth_failed", 403, true},
		{"API forbidden with non-JSON body", message, "auth_failed", 403, false},
		{"API unsupported site", apiError("UNSUPPORTED_SITE"), "content_blocked", 403, true},
		{"API safe mode blocked", apiError("SAFE_MODE_BLOCKED"), "content_blocked", 403, false},
		{"API insufficient credits", apiError("PAYMENT_REQUIRED"), "upstream_error", 402, false},
		{"API timeout", apiError("SCRAPE_TIMEOUT"), "provider_timeout", 408, true},
		{"API rate limited", apiError("RATE_LIMITED"), "rate_limited", 429, true},
		{"API unavailable", apiError("UNAVAILABLE"), "upstream_error", 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, searches, fetches := fourProviderApp(t, [4]http.HandlerFunc{}, [4]http.HandlerFunc{
				bodyResponse(tavilyBody), nil, nil, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				},
			})
			opts, outcome, exit := []string{"--config", config}, "success", 0
			wantCalls := [4]int32{1, 0, 0, 1}
			if !tc.fallback {
				opts = append(opts, "--providers", "firecrawl")
				outcome, exit, wantCalls[0] = "error", 1, 0
			}
			status, reason := "error", ""
			switch tc.code {
			case "":
				status = "empty"
			case "provider_timeout":
				status, reason = "timeout", tc.code
			}
			want := []executionWant{{"firecrawl", status, reason, tc.code}}
			if tc.fallback {
				want = append(want, executionWant{provider: "tavily", status: "success"},
					executionWant{"exa", "not_started", "fallback_satisfied", ""},
					executionWant{"keenable", "not_started", "fallback_satisfied", ""})
			}
			args := fetchArgs(opts...)
			stdout, stderr := run(t, context.Background(), app, exit, args...)
			obj := fetchResponse(t, stdout, outcome, tc.fallback)
			assertFirecrawlOutputControlled(t, stdout, stderr)
			fetches.assert(t, wantCalls)
			if tc.fallback {
				doc := obj["document"].(map[string]any)
				content := doc["content"].(map[string]any)
				if doc["provider"] != "tavily" || content["text"] != "# 汉🙂é字" {
					t.Error("expected the Tavily fallback document")
				}
			} else {
				objectCode(t, obj["error"], "fetch_failed")
			}
			assertExecutions(t, obj, want...)

			stdout, stderr = run(t, context.Background(), app, exit, append(args, "--output", "text")...)
			diagnostic, wantText := "firecrawl: "+status, ""
			if tc.code != "" {
				diagnostic += ": " + tc.code + ":"
			} else {
				diagnostic += "\n"
			}
			if tc.fallback {
				wantText = "# 汉🙂é字\n"
			}
			if stdout != wantText || !strings.Contains(stderr, diagnostic) {
				t.Errorf("unexpected text stdout = %q, stderr = %q", stdout, stderr)
			}
			assertFirecrawlOutputControlled(t, stdout, stderr)
			for i := range wantCalls {
				wantCalls[i] *= 2
			}
			fetches.assert(t, wantCalls)
			searches.assert(t, [4]int32{})
		})
	}
}

func TestFirecrawlSearchHTTPFailures(t *testing.T) {
	isolateConfig(t)
	t.Setenv("FIRECRAWL_API_KEY", firecrawlKey)
	for _, tc := range []struct {
		httpStatus           int
		status, reason, code string
	}{
		{401, "error", "", "auth_failed"},
		{403, "error", "", "auth_failed"},
		{402, "error", "", "upstream_error"},
		{408, "timeout", "provider_timeout", "provider_timeout"},
		{429, "error", "", "rate_limited"},
		{503, "error", "", "upstream_error"},
	} {
		t.Run(fmt.Sprint(tc.httpStatus), func(t *testing.T) {
			var app cli.App
			searches, scrapes := addFirecrawl(t, &app, firecrawlKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.httpStatus)
				fmt.Fprintf(w, `{"success":false,"code":"UNSUPPORTED_SITE","error":%q}`, untrustedError+firecrawlKey)
			}, nil)
			stdout, stderr := run(t, context.Background(), app, 1, "search", query, "--providers", "firecrawl")
			obj := envelope(t, stdout, "search", "error")
			assertResultURLs(t, obj)
			objectCode(t, obj["error"], "search_failed")
			assertExecutions(t, obj, executionWant{"firecrawl", tc.status, tc.reason, tc.code})
			assertFirecrawlOutputControlled(t, stdout, stderr)
			assertCalls(t, searches, 1)
			assertCalls(t, scrapes, 0)
		})
	}
}

func TestFirecrawlRejectsRedirects(t *testing.T) {
	isolateConfig(t)
	t.Setenv("FIRECRAWL_API_KEY", firecrawlKey)
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		fmt.Fprint(w, firecrawlBody)
	}))
	t.Cleanup(target.Close)
	redirect := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		fmt.Fprint(w, untrustedError+firecrawlKey)
	}
	var app cli.App
	searches, scrapes := addFirecrawl(t, &app, firecrawlKey, 10, redirect, redirect)
	for _, command := range [][]string{{"search", query}, {"fetch", fetchURL}} {
		stdout, stderr := run(t, context.Background(), app, 1, append(command, "--providers", "firecrawl")...)
		obj := envelope(t, stdout, command[0], "error")
		objectCode(t, obj["error"], command[0]+"_failed")
		assertExecutions(t, obj, executionWant{provider: "firecrawl", status: "error", code: "upstream_error"})
		assertFirecrawlOutputControlled(t, stdout, stderr)
	}
	assertCalls(t, searches, 1)
	assertCalls(t, scrapes, 1)
	assertCalls(t, &redirected, 0)
}
