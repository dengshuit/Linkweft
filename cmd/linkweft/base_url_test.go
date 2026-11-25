package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"linkweft/internal/cli"
	"linkweft/internal/retrieval"
)

const gatewayKey = "offline-gateway-key"
const gatewayArticle = "https://example.org/article"

type gatewayOperation struct {
	path, request, response string
}

func TestProviderBaseURLPrecedence(t *testing.T) {
	isolateProviderKeys(t)
	for _, provider := range []struct {
		name, origin, authorization, apiKey string
		search, fetch                       gatewayOperation
	}{
		{"tavily", "https://api.tavily.com", "Bearer " + gatewayKey, "",
			gatewayOperation{"/search", `{"query":"fixture","max_results":10,"search_depth":"basic","topic":"general","auto_parameters":false,"include_answer":false,"include_raw_content":false,"include_images":false,"include_image_descriptions":false}`, `{"results":[{"url":"https://example.org/article"}]}`},
			gatewayOperation{"/extract", `{"urls":["https://example.org/article"],"format":"markdown","extract_depth":"basic"}`, `{"results":[{"raw_content":"fixture body"}]}`}},
		{"exa", "https://api.exa.ai", "", gatewayKey,
			gatewayOperation{"/search", `{"query":"fixture","numResults":10,"type":"auto","contents":{"text":false,"highlights":false}}`, `{"results":[{"url":"https://example.org/article"}]}`},
			gatewayOperation{"/contents", `{"urls":["https://example.org/article"],"text":true}`, `{"results":[{"text":"fixture body"}]}`}},
		{"keenable", "https://api.keenable.ai", "", gatewayKey,
			gatewayOperation{"/v1/search", `{"query":"fixture","max_results":10,"disable_cards":true}`, `{"results":[{"url":"https://example.org/article"}]}`},
			gatewayOperation{"/v1/fetch", `{"urls":["https://example.org/article"]}`, `{"results":[{"content":"fixture body"}]}`}},
		{"firecrawl", "https://api.firecrawl.dev", "Bearer " + gatewayKey, "",
			gatewayOperation{"/v2/search", `{"query":"fixture","limit":10,"sources":["web"],"highlights":false}`, `{"success":true,"data":{"web":[{"url":"https://example.org/article"}]}}`},
			gatewayOperation{"/v2/scrape", `{"url":"https://example.org/article","formats":["markdown"],"onlyMainContent":true}`, `{"success":true,"data":{"markdown":"fixture body"}}`}},
	} {
		t.Run(provider.name, func(t *testing.T) {
			serve := func(w http.ResponseWriter, r *http.Request, prefix string) {
				var op gatewayOperation
				switch r.URL.RequestURI() {
				case prefix + provider.search.path:
					op = provider.search
				case prefix + provider.fetch.path:
					op = provider.fetch
				default:
					t.Errorf("unexpected request path: %q", r.URL.RequestURI())
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
					r.Header.Get("Authorization") != provider.authorization || r.Header.Get("X-API-Key") != provider.apiKey {
					t.Error("unexpected method, content type or authentication")
				}
				var body, want map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil || json.Unmarshal([]byte(op.request), &want) != nil ||
					!reflect.DeepEqual(body, want) {
					t.Errorf("request body = %#v, want %s", body, op.request)
				}
				io.WriteString(w, op.response)
			}
			const (
				builtIn = iota
				shared
				search
				fetch
			)
			for _, tc := range []struct {
				name   string
				fields []int
				want   [2]int
			}{
				{"omitted", nil, [2]int{builtIn, builtIn}},
				{"shared", []int{shared}, [2]int{shared, shared}},
				{"search-only", []int{search}, [2]int{search, builtIn}},
				{"fetch-only", []int{fetch}, [2]int{builtIn, fetch}},
				{"search-over-shared", []int{shared, search}, [2]int{search, shared}},
				{"fetch-over-shared", []int{shared, fetch}, [2]int{shared, fetch}},
				{"both", []int{search, fetch}, [2]int{search, fetch}},
				{"both-over-shared", []int{shared, search, fetch}, [2]int{search, fetch}},
			} {
				for _, variant := range []struct{ name, prefix, ending string }{
					{"host", "", ""},
					{"host-with-slash", "", "/"},
					{"prefix", "/gateway%2Ftenant", ""},
					{"prefix-with-slash", "/gateway%2Ftenant", "/"},
				} {
					if len(tc.fields) == 0 && variant.name != "host" {
						continue
					}
					t.Run(tc.name+"/"+variant.name, func(t *testing.T) {
						bases := [4]string{builtIn: provider.origin}
						var config strings.Builder
						fmt.Fprintf(&config, "[providers.%s]\napi_key = %q\n", provider.name, gatewayKey)
						fields := [4]string{shared: "base_url", search: "search_base_url", fetch: "fetch_base_url"}
						for _, field := range tc.fields {
							server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								serve(w, r, variant.prefix)
							}))
							t.Cleanup(server.Close)
							bases[field] = server.URL + variant.prefix
							fmt.Fprintf(&config, "%s = %q\n", fields[field], bases[field]+variant.ending)
						}
						path := writeCLIConfig(t, config.String())
						var requested []string
						client := loopbackClient(t, func(r *http.Request, network http.RoundTripper) (*http.Response, error) {
							requested = append(requested, r.URL.String())
							if r.URL.Scheme+"://"+r.URL.Host == provider.origin {
								defer r.Body.Close()
								recorder := httptest.NewRecorder()
								serve(recorder, r, "")
								return recorder.Result(), nil
							}
							return network.RoundTrip(r)
						})
						app := newApp(client)
						for _, op := range []struct{ name, arg string }{{"search", "fixture"}, {"fetch", gatewayArticle}} {
							response, _, _ := runGatewayCLI(t, app, 0, op.name, op.arg, "--providers", provider.name, "--config", path)
							if response.Outcome != "success" || len(response.Executions) != 1 ||
								response.Executions[0].Provider != provider.name || response.Executions[0].Status != "success" {
								t.Fatalf("unexpected %s response: %+v", op.name, response)
							}
						}
						want := []string{bases[tc.want[0]] + provider.search.path, bases[tc.want[1]] + provider.fetch.path}
						if !slices.Equal(requested, want) {
							t.Errorf("request URLs = %v, want %v", requested, want)
						}
					})
				}
			}
		})
	}
}

func TestProviderBaseURLFallback(t *testing.T) {
	isolateProviderKeys(t)
	for _, status := range []int{http.StatusBadGateway, http.StatusTemporaryRedirect} {
		for _, op := range []struct{ name, arg, firstPath, nextPath, response string }{
			{"search", "fixture", "/selected/search", "/next/search", `{"results":[{"url":"https://example.org/article"}]}`},
			{"fetch", gatewayArticle, "/selected/extract", "/next/contents", `{"results":[{"text":"fixture body"}]}`},
		} {
			t.Run(fmt.Sprintf("%s/%d", op.name, status), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case op.firstPath:
						w.Header().Set("Location", "/redirect-target")
						w.WriteHeader(status)
					case op.nextPath:
						io.WriteString(w, op.response)
					default:
						t.Errorf("unexpected HTTP path: %q", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(server.Close)
				path := writeCLIConfig(t, fmt.Sprintf(`[providers.tavily]
api_key = %q
base_url = %q
%s_base_url = %q
[providers.exa]
api_key = %q
base_url = %q
`, gatewayKey, server.URL+"/covered", op.name, server.URL+"/selected/", gatewayKey, server.URL+"/next"))
				var requested []string
				var deadline time.Time
				client := loopbackClient(t, func(r *http.Request, network http.RoundTripper) (*http.Response, error) {
					requested = append(requested, r.URL.String())
					got, ok := r.Context().Deadline()
					if !ok {
						t.Error("request has no deadline")
					}
					if len(requested) == 1 {
						deadline = got
					} else if !got.Equal(deadline) {
						t.Errorf("fallback reset the command deadline from %v to %v", deadline, got)
					}
					return network.RoundTrip(r)
				})
				response, _, _ := runGatewayCLI(t, newApp(client), 0, op.name, op.arg,
					"--providers", "tavily,exa", "--timeout", "10s", "--config", path)
				if response.Outcome != "success" || len(response.Executions) != 2 {
					t.Fatalf("unexpected fallback response: %+v", response)
				}
				first, next := response.Executions[0], response.Executions[1]
				if first.Provider != "tavily" || first.Status != "error" || first.Error == nil ||
					first.Error.Code != "upstream_error" || next.Provider != "exa" || next.Status != "success" || next.Error != nil {
					t.Errorf("unexpected executions: %+v", response.Executions)
				}
				if want := []string{server.URL + op.firstPath, server.URL + op.nextPath}; !slices.Equal(requested, want) {
					t.Errorf("request URLs = %v, want %v", requested, want)
				}
			})
		}
	}
}

func TestProviderBaseURLInvalidConfig(t *testing.T) {
	isolateProviderKeys(t)
	for _, field := range []string{"base_url", "search_base_url", "fetch_base_url"} {
		for _, tc := range []struct{ name, value string }{
			{"empty", ""},
			{"whitespace", " \t "},
			{"relative", "/private-gateway"},
			{"missing-host", "http://:8080/private-gateway"},
			{"invalid-host", "http://127.0.0.1:1:2/private-gateway"},
			{"out-of-range-port", "http://127.0.0.1:65536/private-gateway"},
			{"non-http", "ftp://private-gateway.example/path"},
			{"userinfo", "http://private-user:private-password@gateway.example"},
			{"query", "http://gateway.example?private-query"},
			{"empty-query", "http://gateway.example?"},
			{"fragment", "http://gateway.example#private-fragment"},
			{"empty-fragment", "http://gateway.example#"},
			{"malformed", "http://gateway.example/%zz"},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				assertInvalidBaseURL(t, "tinyfish", field, fmt.Sprintf("[providers.tinyfish]\n%s = %q\n", field, tc.value))
			})
		}
	}
	t.Run("selected-provider", func(t *testing.T) {
		assertInvalidBaseURL(t, "tavily", "base_url", "base_url = 'http://127.0.0.1:65536/private-gateway'\n")
	})
	t.Run("overridden-shared", func(t *testing.T) {
		assertInvalidBaseURL(t, "tavily", "base_url", `base_url = "http://private-user:private-password@gateway.example"
search_base_url = "http://127.0.0.1:1/search-proxy"
fetch_base_url = "http://127.0.0.1:1/fetch-proxy"
`)
	})
}

func assertInvalidBaseURL(t *testing.T, provider, field, config string) {
	t.Helper()
	path := writeCLIConfig(t, fmt.Sprintf("[providers.exa]\napi_key = %q\n[providers.tavily]\napi_key = %q\n", gatewayKey, gatewayKey)+config)
	message := "providers." + provider + "." + field + " must be an absolute HTTP(S) URL with a hostname and without user info, query, or fragment"
	for _, op := range []struct{ name, arg string }{{"search", "fixture"}, {"fetch", gatewayArticle}} {
		t.Run(op.name, func(t *testing.T) {
			app := newApp(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				r.Body.Close()
				t.Error("config error allowed an HTTP request")
				return nil, errors.New("unexpected HTTP request")
			})})
			args := []string{op.name, op.arg, "--providers", "tavily,exa", "--config", path}
			response, stdout, stderr := runGatewayCLI(t, app, 2, args...)
			if response.Operation == nil || *response.Operation != op.name || response.Outcome != "error" ||
				response.Error == nil || response.Error.Code != "invalid_config" || response.Error.Message != message ||
				response.Executions == nil || len(response.Executions) != 0 || stderr != "" {
				t.Errorf("unexpected preflight response: %+v, stderr = %q", response, stderr)
			}
			wantJSON := fmt.Sprintf(`{"schema_version":1,"operation":%q,"outcome":"error","results":[],"document":null,`+
				`"executions":[],"warnings":[],"error":{"code":"invalid_config","message":%q}}`+"\n", op.name, message)
			if stdout != wantJSON {
				t.Errorf("JSON stdout = %q, want %q", stdout, wantJSON)
			}
			var textStdout, textStderr bytes.Buffer
			if code := app.Run(context.Background(), append(args, "--output", "text"), &textStdout, &textStderr); code != 2 ||
				textStdout.Len() != 0 || textStderr.String() != "invalid_config: "+message+"\n" {
				t.Errorf("text preflight exit = %d, stdout = %q, stderr = %q", code, textStdout.String(), textStderr.String())
			}
		})
	}
}

func isolateProviderKeys(t *testing.T) {
	t.Helper()
	for _, provider := range []string{"TAVILY", "EXA", "KEENABLE", "FIRECRAWL", "ANYSEARCH", "TINYFISH"} {
		t.Setenv(provider+"_API_KEY", "")
	}
}

func runGatewayCLI(t *testing.T, app cli.App, wantCode int, args ...string) (retrieval.Response, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var response retrieval.Response
	if code := app.Run(context.Background(), args, &stdout, &stderr); code != wantCode {
		t.Fatalf("exit = %d, want %d, stdout = %q, stderr = %q", code, wantCode, stdout.String(), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("invalid CLI JSON: %v", err)
	}
	return response, stdout.String(), stderr.String()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func loopbackClient(t *testing.T, handle func(*http.Request, http.RoundTripper) (*http.Response, error)) *http.Client {
	t.Helper()
	network := &http.Transport{}
	t.Cleanup(network.CloseIdleConnections)
	loopback := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !net.ParseIP(r.URL.Hostname()).IsLoopback() {
			r.Body.Close()
			t.Errorf("unexpected outbound request to %s", r.URL.Host)
			return nil, errors.New("test transport only connects to loopback")
		}
		return network.RoundTrip(r)
	})
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return handle(r, loopback)
	})}
}
