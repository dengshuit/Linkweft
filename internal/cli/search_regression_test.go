package cli_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"linkweft/internal/provider"
	"linkweft/internal/retrieval"
)

func TestSearchProtectsOtherProviderCredentials(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	path := testConfig(t, "[providers.tavily]\napi_key = '"+fileKey+"'")
	encodedKey := strings.ReplaceAll(testKey, "-", "%2D")
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			app, tavilyCalls := testApp(t, testKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			exa, exaCalls := exaApp(t, exaKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"results":[{"url":%q},{"url":%q},
					{"url":"https://example.org/safe","title":%q},
					{"url":"https://example.org/another","title":%q}]}`,
					"https://example.org/unsafe?key="+testKey, "https://example.org/unsafe?key="+encodedKey,
					"title "+encodedKey, "title "+fileKey)
			})
			app.NewExa = exa.NewExa
			stdout, stderr := run(t, context.Background(), app, 0, "search", query, "--config", path, "--output", format)
			for _, value := range []string{testKey, fileKey, encodedKey, "/unsafe"} {
				if strings.Contains(stdout+stderr, value) {
					t.Fatal("CLI output contains a credential or its URL")
				}
			}
			if format == "json" {
				response := envelope(t, stdout, "search", "success")
				assertResultURLs(t, response, "https://example.org/safe", "https://example.org/another")
				for _, result := range response["results"].([]any) {
					if result.(map[string]any)["title"] != nil {
						t.Error("expected null title after credential filtering")
					}
				}
			} else if stdout != "https://example.org/safe\n\nhttps://example.org/another\n\n" {
				t.Error("expected only safe URLs in text output")
			}
			assertCalls(t, tavilyCalls, 1)
			assertCalls(t, exaCalls, 1)
		})
	}
}

func TestSearchProtectsEncodedCredentialsWithMalformedQuery(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	const protectedKey = "test-only-exa key"
	t.Setenv("EXA_API_KEY", protectedKey)
	encodedKey := strings.NewReplacer("-", "%2D", " ", "%20").Replace(protectedKey)
	queryEncodedKey := strings.ReplaceAll(encodedKey, "%20", "+")
	const safeURL = "https://example.org/safe?broken=%zz"
	for _, strategy := range []string{"fallback", "merge"} {
		t.Run(strategy, func(t *testing.T) {
			app, tavilyCalls := testApp(t, testKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"results":[{"url":%q},{"url":%q},{"url":%q},{"url":%q}]}`,
					"https://example.org/unsafe?broken=%zz&key="+encodedKey,
					"https://example.org/unsafe?key=%zz"+encodedKey,
					"https://example.org/unsafe?broken=%zz&key="+queryEncodedKey, safeURL)
			})
			exa, exaCalls := exaApp(t, protectedKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"results":[{"url":%q}]}`, safeURL)
			})
			app.NewExa = exa.NewExa
			stdout, stderr := run(t, context.Background(), app, 0, "search", query, "--strategy", strategy)
			response := envelope(t, stdout, "search", "success")
			results := response["results"].([]any)
			for _, value := range results {
				result := value.(map[string]any)
				if result["url"] != safeURL {
					t.Error("unexpected result URL after credential filtering")
				}
				for _, source := range result["sources"].([]any) {
					if source.(map[string]any)["url"] != safeURL {
						t.Error("unexpected source URL after credential filtering")
					}
				}
			}
			if len(results) != 1 || strings.Contains(stdout+stderr, protectedKey) || strings.Contains(stdout+stderr, encodedKey) ||
				strings.Contains(stdout+stderr, queryEncodedKey) {
				t.Fatal("expected only the safe result and no credential in CLI output")
			}
			warnings := response["warnings"].([]any)
			if len(warnings) != 1 {
				t.Fatalf("warnings = %d, want one diagnostic", len(warnings))
			}
			objectCode(t, warnings[0], "invalid_results_dropped")
			assertCalls(t, tavilyCalls, 1)
			var wantExaCalls int32
			if strategy == "merge" {
				wantExaCalls = 1
			}
			assertCalls(t, exaCalls, wantExaCalls)
		})
	}
}

func TestSearchCredentialURLDoesNotSatisfyFallback(t *testing.T) {
	isolateConfig(t)
	const unselectedKey = "test-only-unselected-key"
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	t.Setenv("KEENABLE_API_KEY", unselectedKey)
	app, tavilyCalls := testApp(t, testKey, 10, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"results":[{"url":"https://example.org/tavily","content":%q}]}`,
			"snippet https://example.org/?key="+strings.ReplaceAll(exaKey, "-", "%2D"))
	})
	exa, exaCalls := exaApp(t, exaKey, 10, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"results":[{"url":%q}]}`, "https://example.org/unsafe/"+unselectedKey)
	})
	app.NewExa = exa.NewExa
	stdout, stderr := run(t, context.Background(), app, 0, "search", query, "--providers", "exa,tavily")
	if strings.Contains(stdout+stderr, unselectedKey) || strings.Contains(stdout+stderr, exaKey) {
		t.Fatal("CLI output contains another provider's credential")
	}
	response := envelope(t, stdout, "search", "success")
	assertResultURLs(t, response, "https://example.org/tavily")
	results := response["results"].([]any)
	if len(results) == 1 && results[0].(map[string]any)["snippet"] != nil {
		t.Error("expected null snippet after credential filtering")
	}
	assertExecutions(t, response,
		executionWant{provider: "exa", status: "error", code: "invalid_response"},
		executionWant{provider: "tavily", status: "success"},
	)
	assertCalls(t, exaCalls, 1)
	assertCalls(t, tavilyCalls, 1)
}

func TestSearchHTTPTimeoutPermitsFallback(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	for _, tc := range []struct {
		name  string
		setup func(*testing.T) (string, *http.Client, *atomic.Int32)
	}{
		{"TLS handshake", stalledTLSHandshake},
		{"response body", stalledResponseBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseURL, client, exaAttempts := tc.setup(t)
			app, tavilyCalls := testApp(t, testKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"results":[{"url":"https://example.org/tavily"}]}`)
			})
			app.NewExa = func(key, _ string) retrieval.SearchProvider {
				return provider.NewExa(key, client, baseURL)
			}
			stdout, _ := run(t, context.Background(), app, 0, "search", query, "--providers", "exa,tavily", "--timeout", "3s")
			response := envelope(t, stdout, "search", "success")
			assertExecutions(t, response,
				executionWant{"exa", "timeout", "provider_timeout", "provider_timeout"},
				executionWant{provider: "tavily", status: "success"},
			)
			assertResultURLs(t, response, "https://example.org/tavily")
			assertCalls(t, exaAttempts, 1)
			assertCalls(t, tavilyCalls, 1)
		})
	}
}

func stalledTLSHandshake(t *testing.T) (string, *http.Client, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	release, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			attempts.Add(1)
			<-release
		}
	}()
	transport := &http.Transport{TLSHandshakeTimeout: 150 * time.Millisecond}
	t.Cleanup(func() {
		close(release)
		listener.Close()
		<-done
		transport.CloseIdleConnections()
	})
	return "https://" + listener.Addr().String(), &http.Client{Transport: transport}, &attempts
}

func stalledResponseBody(t *testing.T) (string, *http.Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"results":`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(4 * time.Second):
			t.Error("timed-out request was not cancelled")
		}
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 200 * time.Millisecond
	return server.URL, client, &calls
}

func TestSearchTextReportsAllExecutions(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	t.Setenv("ANYSEARCH_API_KEY", "test-only-anysearch-key")
	path := testConfig(t, "[search]\npriority = ['anysearch']")
	app, tavilyCalls := testApp(t, testKey, 10, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":[{"url":"https://example.org/tavily"}]}`)
	})
	exa, exaCalls := exaApp(t, exaKey, 10, func(http.ResponseWriter, *http.Request) {})
	app.NewExa = exa.NewExa
	stdout, stderr := run(t, context.Background(), app, 0, "search", query, "--config", path, "--output", "text")
	if stdout != "https://example.org/tavily\n\n" {
		t.Errorf("unexpected text results: %q", stdout)
	}
	want := "anysearch: skipped: unsupported_capability\ntavily: success\nexa: not_started: fallback_satisfied\n"
	if stderr != want {
		t.Errorf("execution summaries = %q, want %q", stderr, want)
	}
	assertCalls(t, tavilyCalls, 1)
	assertCalls(t, exaCalls, 0)
}
