package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"linkweft/internal/retrieval"
)

func TestAnySearchRouting(t *testing.T) {
	isolateProviderKeys(t)
	const anySearchError = `{"code":-1,"data":{"results":[{"url":"https://example.org/failed"}]}}`
	const mergeResponse = `{"code":0,"data":{"results":[{"url":"/invalid"},{"url":"https://example.org/article","content":"AnySearch content"}]}}`
	const exaResults = `{"results":[{"url":"https://example.org/article"},{"url":"https://example.org/next"}]}`
	const exaMerge = `{"results":[{"url":"HTTPS://EXAMPLE.ORG:443/article"}]}`
	const exaDocument = `{"results":[{"text":"Exa body"}]}`
	const skip = "unsupported_capability"
	const stop = "fallback_satisfied"
	const searchPriority = "[search]\npriority = ['anysearch']\n"
	for _, tc := range []struct {
		name, operation, arg, priority, selected string
		exit, results                            int
		merge                                    bool
		documentText                             string
		fixtures                                 map[string]string
		requests                                 []string
		executions                               []gatewayExecution
	}{
		{name: "default search", operation: "search", arg: "fixture", results: 1,
			fixtures: map[string]string{"/tavily/search": exaMerge}, requests: []string{"/tavily/search"},
			executions: []gatewayExecution{{provider: "tavily", status: "success"}, {"exa", "not_started", stop, ""},
				{"keenable", "not_started", stop, ""}, {"firecrawl", "not_started", stop, ""},
				{"anysearch", "not_started", stop, ""}, {"tinyfish", "skipped", skip, ""}}},
		{name: "default fetch", operation: "fetch", arg: gatewayArticle,
			fixtures: map[string]string{"/firecrawl/v2/scrape": `{"success":true,"data":{"markdown":"Firecrawl body"}}`},
			requests: []string{"/firecrawl/v2/scrape"}, executions: []gatewayExecution{{provider: "firecrawl", status: "success"},
				{"tavily", "not_started", stop, ""}, {"exa", "not_started", stop, ""}, {"keenable", "not_started", stop, ""},
				{"tinyfish", "skipped", skip, ""}, {"anysearch", "not_started", stop, ""}}},
		{name: "configured search with appended defaults", operation: "search", arg: "fixture", priority: searchPriority, results: 1,
			fixtures: map[string]string{"/anysearch/v1/search": anySearchHit}, requests: []string{"/anysearch/v1/search"},
			executions: []gatewayExecution{{provider: "anysearch", status: "success"}, {"tavily", "not_started", stop, ""},
				{"exa", "not_started", stop, ""}, {"keenable", "not_started", stop, ""}, {"firecrawl", "not_started", stop, ""},
				{"tinyfish", "skipped", skip, ""}}},
		{name: "explicit search overrides configuration", operation: "search", arg: "fixture", priority: searchPriority, selected: "exa,anysearch", results: 2,
			fixtures: map[string]string{"/exa/search": exaResults}, requests: []string{"/exa/search"},
			executions: []gatewayExecution{{provider: "exa", status: "success"}, {"anysearch", "not_started", stop, ""}}},
		{name: "empty search falls back", operation: "search", arg: "fixture", selected: "anysearch,exa", results: 2,
			fixtures: map[string]string{"/anysearch/v1/search": anySearchEmpty, "/exa/search": exaResults}, requests: []string{"/anysearch/v1/search", "/exa/search"},
			executions: []gatewayExecution{{provider: "anysearch", status: "empty"}, {provider: "exa", status: "success"}}},
		{name: "failed search falls back", operation: "search", arg: "fixture", selected: "anysearch,exa", results: 2,
			fixtures: map[string]string{"/anysearch/v1/search": anySearchError, "/exa/search": exaResults}, requests: []string{"/anysearch/v1/search", "/exa/search"},
			executions: []gatewayExecution{{provider: "anysearch", status: "error", code: "upstream_error"}, {provider: "exa", status: "success"}}},
		{name: "failed single selection stays in scope", operation: "search", arg: "fixture", selected: "anysearch", exit: 1,
			fixtures: map[string]string{"/anysearch/v1/search": anySearchError}, requests: []string{"/anysearch/v1/search"},
			executions: []gatewayExecution{{provider: "anysearch", status: "error", code: "upstream_error"}}},
		{name: "blank content falls back in configured fetch order", operation: "fetch", arg: gatewayArticle, priority: "[fetch]\npriority = ['anysearch','exa']\n",
			fixtures: map[string]string{"/anysearch/v1/extract": `{"code":0,"data":{"content":" \t"}}`, "/exa/contents": exaDocument},
			requests: []string{"/anysearch/v1/extract", "/exa/contents"},
			executions: []gatewayExecution{{provider: "anysearch", status: "empty"}, {provider: "exa", status: "success"},
				{"firecrawl", "not_started", stop, ""}, {"tavily", "not_started", stop, ""}, {"keenable", "not_started", stop, ""},
				{"tinyfish", "skipped", skip, ""}}},
		{name: "invalid extract URL falls back", operation: "fetch", arg: gatewayArticle, selected: "anysearch,exa", documentText: "Exa body",
			fixtures: map[string]string{
				"/anysearch/v1/extract": `{"code":0,"error_code":"invalid_extract_url","data":{"content":"failed AnySearch body"}}`,
				"/exa/contents":         exaDocument,
			},
			requests:   []string{"/anysearch/v1/extract", "/exa/contents"},
			executions: []gatewayExecution{{provider: "anysearch", status: "error", code: "upstream_error"}, {provider: "exa", status: "success"}}},
		{name: "merge preserves sources", operation: "search", arg: "fixture", selected: "anysearch,exa", merge: true, results: 1,
			fixtures: map[string]string{"/anysearch/v1/search": mergeResponse, "/exa/search": exaMerge}, requests: []string{"/anysearch/v1/search", "/exa/search"},
			executions: []gatewayExecution{{provider: "anysearch", status: "success"}, {provider: "exa", status: "success"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.URL.RequestURI())
				mu.Unlock()
				body, ok := tc.fixtures[r.URL.RequestURI()]
				if !ok {
					t.Errorf("unexpected request to %s", r.URL.RequestURI())
					w.WriteHeader(http.StatusNotFound)
					return
				}
				io.WriteString(w, body)
			}))
			defer server.Close()
			var config strings.Builder
			config.WriteString(tc.priority)
			for _, name := range []string{"tavily", "exa", "keenable", "firecrawl", "anysearch", "tinyfish"} {
				fmt.Fprintf(&config, "[providers.%s]\napi_key = %q\nbase_url = %q\n", name, gatewayKey, server.URL+"/"+name)
			}
			args := []string{tc.operation, tc.arg, "--config", writeCLIConfig(t, config.String())}
			if tc.selected != "" {
				args = append(args, "--providers", tc.selected)
			}
			if tc.merge {
				args = append(args, "--strategy", "merge")
			}
			client := loopbackClient(t, func(r *http.Request, network http.RoundTripper) (*http.Response, error) {
				return network.RoundTrip(r)
			})
			response, _, _ := runGatewayCLI(t, newApp(client), tc.exit, args...)
			assertGatewayExecutions(t, response, tc.executions...)
			outcome := "success"
			if tc.exit != 0 {
				outcome = "error"
			}
			if response.Outcome != outcome || len(response.Results) != tc.results {
				t.Errorf("response = %+v, want %s with %d results", response, outcome, tc.results)
			}
			if tc.documentText != "" && (response.Document == nil || response.Document.Content.Text != tc.documentText) {
				t.Errorf("document = %+v, want body %q", response.Document, tc.documentText)
			}
			if tc.merge {
				want := []retrieval.Result{{URL: gatewayArticle, Sources: []retrieval.Source{
					{Provider: "anysearch", URL: gatewayArticle, Rank: 2}, {Provider: "exa", URL: "HTTPS://EXAMPLE.ORG:443/article", Rank: 1},
				}, Content: &retrieval.Content{Text: "AnySearch content", Provider: "anysearch", Format: "unknown", Freshness: "unknown"}}}
				if !reflect.DeepEqual(response.Results, want) {
					t.Errorf("merged results = %+v, want %+v", response.Results, want)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.merge {
				slices.Sort(requests)
			}
			if !slices.Equal(requests, tc.requests) {
				t.Errorf("requests = %v, want %v", requests, tc.requests)
			}
		})
	}
}
