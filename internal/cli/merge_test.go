package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"linkweft/internal/cli"
	"linkweft/internal/retrieval"
)

func TestSearchMergeCandidateOrder(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	t.Setenv("ANYSEARCH_API_KEY", "test-only-anysearch-key")
	const tavilyURL, exaURL = "HTTPS://EXAMPLE.ORG:443/result", "https://example.org/result"
	for _, tc := range []struct {
		name, priority, selected string
		exaFirst, skip           bool
	}{
		{name: "automatic built-in order", skip: true},
		{name: "automatic configured order", priority: "exa", exaFirst: true, skip: true},
		{name: "explicit order overrides configuration", priority: "tavily", selected: "exa,tavily", exaFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, tavilyCalls := testApp(t, testKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"results":[{"url":%q,"content":"Tavily snippet"}]}`, tavilyURL)
			})
			exa, exaCalls := exaApp(t, exaKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"results":[{"url":%q,"title":"Exa title"}]}`, exaURL)
			})
			app.NewExa = exa.NewExa
			args := []string{"search", query, "--strategy", "merge"}
			if tc.priority != "" {
				args = append(args, "--config", testConfig(t, fmt.Sprintf("[search]\npriority = [%q]", tc.priority)))
			}
			if tc.selected != "" {
				args = append(args, "--providers", tc.selected)
			}
			stdout, _ := run(t, context.Background(), app, 0, args...)
			response := envelope(t, stdout, "search", "success")
			executions := []executionWant{{provider: "tavily", status: "success"}, {provider: "exa", status: "success"}}
			sources := []any{
				map[string]any{"provider": "tavily", "url": tavilyURL, "rank": float64(1)},
				map[string]any{"provider": "exa", "url": exaURL, "rank": float64(1)},
			}
			address := tavilyURL
			if tc.exaFirst {
				address = exaURL
				executions[0], executions[1] = executions[1], executions[0]
				sources[0], sources[1] = sources[1], sources[0]
			}
			if tc.skip {
				executions = append(executions, executionWant{provider: "anysearch", status: "skipped", reason: "unsupported_capability"})
			}
			assertExecutions(t, response, executions...)
			want := []any{map[string]any{"url": address, "title": "Exa title", "snippet": "Tavily snippet", "content": nil, "sources": sources}}
			if !reflect.DeepEqual(response["results"], want) {
				t.Errorf("results = %#v, want %#v", response["results"], want)
			}
			assertCalls(t, tavilyCalls, 1)
			assertCalls(t, exaCalls, 1)
		})
	}
}

func TestSearchMergeCLIConcurrency(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	for _, name := range []string{"default concurrency", "configured concurrency of one"} {
		t.Run(name, func(t *testing.T) {
			one := name == "configured concurrency of one"
			started := make(chan string, 2)
			release := make(chan struct{})
			app, tavilyCalls := testApp(t, testKey, 10, func(w http.ResponseWriter, r *http.Request) {
				started <- "tavily"
				select {
				case <-release:
				case <-r.Context().Done():
				}
				fmt.Fprint(w, `{"results":[]}`)
			})
			exa, exaCalls := exaApp(t, exaKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				started <- "exa"
				fmt.Fprint(w, `{"results":[{"url":"https://example.org/exa"}]}`)
			})
			app.NewExa = exa.NewExa
			args := []string{"search", query, "--strategy", "merge", "--timeout", "3s"}
			if one {
				args = append(args, "--config", testConfig(t, "[search]\nmax_concurrency = 1"))
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stdout, stderr bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- app.Run(ctx, args, &stdout, &stderr) }()
			awaitStart := func() string {
				t.Helper()
				select {
				case name := <-started:
					return name
				case <-time.After(4 * time.Second):
					t.Fatal("timed out waiting for an HTTP request")
					return ""
				}
			}
			first := awaitStart()
			if one {
				if first != "tavily" {
					t.Fatalf("first request = %s, want tavily", first)
				}
				select {
				case next := <-started:
					t.Fatalf("%s started while Tavily still holds the only slot", next)
				case <-time.After(75 * time.Millisecond):
				}
				close(release)
				if next := awaitStart(); next != "exa" {
					t.Fatalf("next request = %s, want exa", next)
				}
			} else {
				if second := awaitStart(); first == second {
					t.Fatalf("repeated request to %s", second)
				}
				close(release)
			}
			select {
			case exit := <-done:
				if exit != 0 {
					t.Fatalf("exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
				}
			case <-time.After(4 * time.Second):
				t.Fatal("CLI did not return")
			}
			response := envelope(t, stdout.String(), "search", "success")
			assertExecutions(t, response, executionWant{provider: "tavily", status: "empty"}, executionWant{provider: "exa", status: "success"})
			assertResultURLs(t, response, "https://example.org/exa")
			assertCalls(t, tavilyCalls, 1)
			assertCalls(t, exaCalls, 1)
		})
	}
}

func TestSearchMergeCLIOutcomes(t *testing.T) {
	isolateConfig(t)
	t.Setenv("TAVILY_API_KEY", testKey)
	t.Setenv("EXA_API_KEY", exaKey)
	const aURL = "https://example.org/tavily"
	for _, tc := range []struct {
		name, outcome, reason, exaStatus, exaCode string
		found                                     bool
		httpStatus, exit                          int
	}{
		{"AC08 provider timeout", "partial", "provider_timeout", "timeout", "provider_timeout", true, 0, 0},
		{"AC09 both empty", "empty", "", "empty", "", false, http.StatusOK, 0},
		{"AC10 empty and limited", "error", "", "error", "rate_limited", false, http.StatusTooManyRequests, 1},
		{"AC26 result and upstream failure", "partial", "", "error", "upstream_error", true, http.StatusServiceUnavailable, 0},
		{"AC34 user cancellation", "cancelled", "user_cancelled", "cancelled", "user_cancelled", true, 0, 130},
		{"AC35 command timeout with results", "partial", "command_timeout", "timeout", "command_timeout", true, 0, 0},
		{"AC10 command timeout without results", "error", "command_timeout", "timeout", "command_timeout", false, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			app, tavilyCalls := testApp(t, testKey, 10, func(w http.ResponseWriter, _ *http.Request) {
				if tc.found {
					fmt.Fprintf(w, `{"results":[{"url":%q}]}`, aURL)
				} else {
					fmt.Fprint(w, `{"results":[]}`)
				}
			})
			requestDone := make(chan struct{}, 1)
			exa, exaCalls := exaApp(t, exaKey, 10, func(w http.ResponseWriter, r *http.Request) {
				if tc.httpStatus != 0 {
					w.WriteHeader(tc.httpStatus)
					fmt.Fprint(w, `{"results":[]}`)
					return
				}
				fmt.Fprint(w, `{"results":`)
				w.(http.Flusher).Flush()
				if tc.exit == 130 {
					cancel(retrieval.ErrUserCancelled)
				}
				select {
				case <-r.Context().Done():
					requestDone <- struct{}{}
				case <-time.After(4 * time.Second):
					t.Error("Exa request context was not cancelled")
				}
			})
			app.NewExa = exa.NewExa
			command, platform := "3s", "3s"
			switch tc.reason {
			case "provider_timeout":
				platform = "150ms"
			case "command_timeout":
				command = "350ms"
			}
			path := testConfig(t, fmt.Sprintf("[search]\nmax_concurrency = 1\ntimeout = %q\n[providers.exa]\nsearch_timeout = %q", command, platform))
			stdout, _ := run(t, ctx, app, tc.exit, "search", query, "--strategy", "merge", "--config", path)
			response := envelope(t, stdout, "search", tc.outcome)
			tavilyStatus := "empty"
			if tc.found {
				tavilyStatus = "success"
				assertResultURLs(t, response, aURL)
			} else {
				assertResultURLs(t, response)
			}
			assertExecutions(t, response, executionWant{provider: "tavily", status: tavilyStatus}, executionWant{"exa", tc.exaStatus, tc.reason, tc.exaCode})
			switch tc.exit {
			case 0:
				if response["error"] != nil {
					t.Errorf("overall error = %#v, want null", response["error"])
				}
			case 1:
				objectCode(t, response["error"], "search_failed")
			case 130:
				objectCode(t, response["error"], "user_cancelled")
			}
			if tc.httpStatus == 0 {
				select {
				case <-requestDone:
				case <-time.After(4 * time.Second):
					t.Fatal("HTTP request was not cancelled")
				}
			}
			assertCalls(t, tavilyCalls, 1)
			assertCalls(t, exaCalls, 1)
		})
	}
}

func TestSearchMergeHelp(t *testing.T) {
	stdout, _ := run(t, context.Background(), cli.App{}, 0, "search", "--help")
	if !strings.Contains(stdout, "fallback or merge") || !strings.Contains(stdout, `(default "fallback")`) {
		t.Errorf("unexpected search help: %s", stdout)
	}
}
