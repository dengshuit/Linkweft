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

	"linkweft/internal/retrieval"
)

const offlineCLIEnv = "LINKWEFT_TEST_CLI"
const offlineBinaryEnv = "LINKWEFT_TEST_BINARY"

func TestOfflineCLIEntry(t *testing.T) {
	if os.Getenv(offlineCLIEnv) == "" {
		t.Skip("CLI subprocess entry")
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Fatal("missing CLI argument separator")
	}
	os.Args = append([]string{os.Args[0]}, os.Args[separator+1:]...)
	main()
}

func TestCLIProcessStreams(t *testing.T) {
	const url = "https://example.org/result"
	isolateProviderKeys(t)
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
			config := writeCLIConfig(t, "[providers.tavily]\napi_key = 'offline-fixture'\nbase_url = '"+server.URL+"/tavily/'\n"+`
[providers.anysearch]
base_url = "https://gateway.example/anysearch"
[providers.tinyfish]
search_base_url = "https://gateway.example/tinyfish-search"
fetch_base_url = "https://gateway.example/tinyfish-fetch"
`)
			args := []string{"search", "fixture", "--providers", "tavily", "--config", config}
			if tc.format != "" {
				args = append(args, "--output", tc.format)
			}
			stdout, stderr := runCLIProcess(t, tc.exit, args...)
			if tc.format == "text" {
				if stdout != tc.stdout || !strings.Contains(stderr, tc.diagnostic) {
					t.Fatalf("stdout=%q stderr=%q, want stdout=%q and stderr containing %q", stdout, stderr, tc.stdout, tc.diagnostic)
				}
				return
			}
			var response retrieval.Response
			if err := json.Unmarshal([]byte(stdout), &response); err != nil || stderr != "" {
				t.Fatalf("stdout=%q stderr=%q, want one JSON response and no diagnostics: %v", stdout, stderr, err)
			}
			if response.SchemaVersion != 1 || response.Operation == nil || *response.Operation != "search" ||
				response.Results == nil || response.Executions == nil || response.Warnings == nil || response.Document != nil {
				t.Fatalf("unexpected response: %s", stdout)
			}
			if tc.exit == 0 {
				if response.Outcome != "success" || len(response.Results) != 1 || response.Results[0].URL != url || response.Error != nil {
					t.Fatalf("unexpected success response: %s", stdout)
				}
			} else if response.Outcome != "error" || len(response.Results) != 0 || response.Error == nil || response.Error.Code != "search_failed" {
				t.Fatalf("unexpected failure response: %s", stdout)
			}
		})
	}
}

func runCLIProcess(t *testing.T, wantExit int, args ...string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	binary := os.Getenv(offlineBinaryEnv)
	if launcher := os.Getenv("LINKWEFT_TEST_NODE_LAUNCHER"); launcher != "" {
		binary = "node"
		args = append([]string{launcher}, args...)
	} else if binary == "" {
		binary = os.Args[0]
		args = append([]string{"-test.run=^TestOfflineCLIEntry$", "--"}, args...)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), offlineCLIEnv+"=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != wantExit {
		t.Fatalf("process = %v, err = %v, want exit %d; stdout=%q stderr=%q", cmd.ProcessState, err, wantExit, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

func writeCLIConfig(t *testing.T, contents string) string {
	t.Helper()
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return config
}
