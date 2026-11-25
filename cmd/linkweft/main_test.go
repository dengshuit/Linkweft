package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"linkweft/internal/cli"
	"linkweft/internal/provider"
	"linkweft/internal/retrieval"
)

const offlineEndpointEnv = "LINKWEFT_TEST_LOOPBACK"

func TestOfflineCLIEntry(t *testing.T) {
	endpoint := os.Getenv(offlineEndpointEnv)
	if endpoint == "" {
		t.Skip("CLI subprocess entry")
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Fatal("missing CLI argument separator")
	}
	app := cli.App{
		NewTavily: func(key string) retrieval.SearchProvider {
			return provider.NewTavily(key, nil, endpoint+"/tavily/search")
		},
		NewExa: func(key string) retrieval.SearchProvider {
			return provider.NewExa(key, nil, endpoint+"/exa/search")
		},
	}
	os.Exit(run(app, os.Args[separator+1:]))
}

func TestCLIProcessStreams(t *testing.T) {
	const url = "https://example.org/result"
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte("[providers.tavily]\napi_key = 'offline-fixture'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAVILY_API_KEY", "")
	for _, tc := range []struct {
		name, format, stdout, diagnostic string
		status, exit                     int
	}{
		{name: "default JSON", status: http.StatusOK},
		{name: "text", format: "text", stdout: "Title\n" + url + "\n\n", diagnostic: "tavily: success", status: http.StatusOK},
		{name: "JSON failure", status: http.StatusTooManyRequests, exit: 1},
		{name: "text failure", format: "text", diagnostic: "search_failed", status: http.StatusTooManyRequests, exit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/tavily/search" || r.Header.Get("Authorization") != "Bearer offline-fixture" {
					t.Error("unexpected request path or authorization")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"results":[{"url":"`+url+`","title":"Title"}]}`)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			args := []string{"-test.run=^TestOfflineCLIEntry$", "--", "search", "fixture", "--providers", "tavily", "--config", config}
			if tc.format != "" {
				args = append(args, "--output", tc.format)
			}
			cmd := exec.CommandContext(ctx, os.Args[0], args...)
			cmd.Env = append(os.Environ(), offlineEndpointEnv+"="+server.URL)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.exit {
				t.Fatalf("process = %v, err = %v, want exit %d; stdout=%q stderr=%q", cmd.ProcessState, err, tc.exit, stdout.String(), stderr.String())
			}
			if tc.format == "text" {
				if stdout.String() != tc.stdout || !strings.Contains(stderr.String(), tc.diagnostic) {
					t.Fatalf("stdout=%q stderr=%q, want stdout=%q and stderr containing %q", stdout.String(), stderr.String(), tc.stdout, tc.diagnostic)
				}
				return
			}
			var response retrieval.Response
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || stderr.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q, want one JSON response and no diagnostics: %v", stdout.String(), stderr.String(), err)
			}
			if response.SchemaVersion != 1 || response.Operation == nil || *response.Operation != "search" ||
				response.Results == nil || response.Executions == nil || response.Warnings == nil || response.Document != nil {
				t.Fatalf("unexpected response: %s", stdout.String())
			}
			if tc.exit == 0 {
				if response.Outcome != "success" || len(response.Results) != 1 || response.Results[0].URL != url || response.Error != nil {
					t.Fatalf("unexpected success response: %s", stdout.String())
				}
			} else if response.Outcome != "error" || len(response.Results) != 0 || response.Error == nil || response.Error.Code != "search_failed" {
				t.Fatalf("unexpected failure response: %s", stdout.String())
			}
		})
	}
}
