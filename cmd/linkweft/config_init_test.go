package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"linkweft/internal/config"
	"linkweft/internal/retrieval"
)

func isolatedConfigPath(t *testing.T) string {
	t.Helper()
	isolateProviderKeys(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	// Keep unexpected requests from reaching real provider hosts.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "linkweft", "config.toml")
}

func TestCLIConfigInitAC40(t *testing.T) {
	path := isolatedConfigPath(t)
	providers := []struct{ name, address string }{
		{"tavily", `# base_url = "https://api.tavily.com"`},
		{"exa", `# base_url = "https://api.exa.ai"`},
		{"keenable", `# base_url = "https://api.keenable.ai"`},
		{"firecrawl", `# base_url = "https://api.firecrawl.dev"`},
		{"anysearch", `# base_url = "https://api.anysearch.com"`},
		{"tinyfish", "# search_base_url = \"https://api.search.tinyfish.ai\"\n# fetch_base_url = \"https://api.fetch.tinyfish.ai\""},
	}
	for _, provider := range providers {
		t.Setenv(strings.ToUpper(provider.name)+"_API_KEY", "offline-init-"+provider.name)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration directory should be absent before initialization: %v", err)
	}
	stdout, stderr := runCLIProcess(t, 0, "init")
	data := readConfigFixture(t, path)
	contents := string(data)
	if strings.Contains(contents+stdout+stderr, "offline-init-") {
		t.Fatal("init copied a credential from the environment")
	}
	if stdout != path+"\n" || stderr != "" {
		t.Fatalf("init stdout = %q, stderr = %q, want only the configuration path on stdout", stdout, stderr)
	}
	tables := []string{"[search]", "[fetch]"}
	for _, provider := range providers {
		table := "[providers." + provider.name + "]"
		tables = append(tables, table)
		want := fmt.Sprintf("%s\n# 环境变量 %s_API_KEY\n# api_key = \"\"\n", table, strings.ToUpper(provider.name))
		_, section, _ := strings.Cut(contents, want)
		section, _, _ = strings.Cut(section, "\n\n")
		if !strings.Contains(section, provider.address) || !strings.Contains(section, `# search_timeout = "15s"`) ||
			!strings.Contains(section, `# fetch_timeout = "45s"`) {
			t.Errorf("missing commented settings for %s", provider.name)
		}
	}
	var active []string
	for _, line := range strings.Split(contents, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			active = append(active, line)
		}
	}
	if !slices.Equal(active, tables) {
		t.Error("template must contain only the two operation tables and six provider tables as active lines")
	}
	for _, comment := range []string{
		`# priority = ["tavily", "exa"]`, `# priority = ["exa", "tavily"]`,
		`# timeout = "45s"`, `# limit = 10`, `# max_concurrency = 3`, `# timeout = "90s"`, `# max_chars = 50000`,
		"非空环境变量优先", "可用 search_base_url 或 fetch_base_url",
	} {
		if !strings.Contains(contents, comment) {
			t.Errorf("missing template comment %q", comment)
		}
	}
	if strings.Count(contents, "未列出的适用且有凭据的服务商按内置顺序补到末尾") != 2 {
		t.Error("missing priority completion instructions for search and fetch")
	}
	isolateProviderKeys(t)
	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("production loader rejected the generated template: %v", err)
	}
	if len(cfg.APIKeys) != 0 || len(cfg.ConfiguredSearchProviders) != 0 || len(cfg.ConfiguredFetchProviders) != 0 {
		t.Fatal("unmodified template configured a provider")
	}
	modified := []byte(strings.Replace(contents, "# limit = 10", "limit = 7", 1) + "\n# Local edit\n")
	if err := os.WriteFile(path, modified, 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr = runCLIProcess(t, 0, "init")
	if !strings.Contains(stdout, "already exists") || !strings.Contains(stdout, path) || stderr != "" ||
		!bytes.Equal(readConfigFixture(t, path), modified) {
		t.Fatal("repeating init must report the existing path and preserve the edited file byte for byte")
	}
}

func TestCLIInitCommandR21(t *testing.T) {
	path := isolatedConfigPath(t)
	for _, args := range [][]string{{"--help"}, {"init", "--help"}, {"help", "init"}} {
		stdout, stderr := runCLIProcess(t, 0, args...)
		want := "init [--config PATH]"
		if len(args) == 1 {
			want = "Create a TOML configuration file"
		}
		if !strings.Contains(stdout, want) || stderr != "" {
			t.Fatalf("%v stdout = %q, stderr = %q, want usage containing %q", args, stdout, stderr, want)
		}
		assertMissingConfig(t, path)
	}
	for _, args := range [][]string{
		{"init", "extra"}, {"init", "--config"}, {"init", "--providers", "tavily"},
		{"init", "--timeout", "1s"}, {"init", "--output", "json"}, {"init", "--strategy", "merge"},
		{"init", "--limit", "1"}, {"init", "--max-chars", "100"},
	} {
		stdout, stderr := runCLIProcess(t, 2, args...)
		if stdout != "" || stderr == "" {
			t.Fatalf("%v stdout = %q, stderr = %q, want only a diagnostic on stderr", args, stdout, stderr)
		}
		assertMissingConfig(t, path)
	}
}

func TestCLIPersistentConfig(t *testing.T) {
	defaultPath := isolatedConfigPath(t)
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		header, key := "Authorization", "Bearer "+gatewayKey
		if r.URL.Path == "/default/exa/search" {
			header, key = "X-API-Key", gatewayKey
		}
		if r.Method != http.MethodPost || r.Header.Get(header) != key {
			t.Error("unexpected method or fixture credential")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/default/exa/search", "/default/tavily/extract":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/default/tavily/search", "/explicit/tavily/search":
			fmt.Fprintf(w, `{"results":[{"url":%q}]}`, gatewayArticle)
		case "/default/firecrawl/v2/scrape":
			fmt.Fprint(w, `{"success":true,"data":{"markdown":"default body"}}`)
		case "/explicit/tavily/extract":
			fmt.Fprint(w, `{"results":[{"raw_content":"explicit body"}]}`)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runCLIProcess(t, 0, "init")
	providerSettings := func(name, prefix string) string {
		return fmt.Sprintf("[providers.%s]\napi_key = %q\nbase_url = %q", name, gatewayKey, server.URL+prefix+"/"+name)
	}
	defaultFile := []byte(strings.NewReplacer(
		"[search]", "[search]\npriority = [\"exa\"]",
		"[fetch]", "[fetch]\npriority = [\"tavily\"]",
		"[providers.tavily]", providerSettings("tavily", "/default"),
		"[providers.exa]", providerSettings("exa", "/default"),
		"[providers.firecrawl]", providerSettings("firecrawl", "/default"),
	).Replace(string(readConfigFixture(t, defaultPath))))
	if err := os.WriteFile(defaultPath, defaultFile, 0600); err != nil {
		t.Fatal(err)
	}
	assertRun := func(t *testing.T, args, wantRequests []string, wantExecutions ...gatewayExecution) retrieval.Response {
		t.Helper()
		stdout, stderr := runCLIProcess(t, 0, args...)
		var response retrieval.Response
		if err := json.Unmarshal([]byte(stdout), &response); err != nil || stderr != "" ||
			response.Operation == nil || *response.Operation != args[0] || response.Outcome != "success" || response.Error != nil {
			t.Fatalf("%v stdout = %q, stderr = %q, want a successful JSON response: %v", args, stdout, stderr, err)
		}
		mu.Lock()
		gotRequests := requests
		requests = nil
		mu.Unlock()
		if !slices.Equal(gotRequests, wantRequests) {
			t.Fatalf("%v requests = %v, want %v", args, gotRequests, wantRequests)
		}
		assertGatewayExecutions(t, response, wantExecutions...)
		return response
	}
	assertSearch := func(t *testing.T, r retrieval.Response) {
		t.Helper()
		if len(r.Results) != 1 || r.Results[0].URL != gatewayArticle || len(r.Results[0].Sources) != 1 ||
			r.Results[0].Sources[0].Provider != "tavily" || r.Document != nil {
			t.Fatalf("unexpected search results: %+v", r.Results)
		}
	}
	assertFetch := func(t *testing.T, r retrieval.Response, provider, body string) {
		t.Helper()
		if r.Document == nil || r.Document.Provider != provider || r.Document.RequestedURL != gatewayArticle ||
			r.Document.Content.Text != body || len(r.Results) != 0 {
			t.Fatalf("unexpected fetch document: %+v", r.Document)
		}
	}
	searchArgs, fetchArgs := []string{"search", "fixture"}, []string{"fetch", gatewayArticle}
	defaultSearch := func(t *testing.T) {
		t.Helper()
		assertSearch(t, assertRun(t, searchArgs, []string{"/default/exa/search", "/default/tavily/search"},
			gatewayExecution{"exa", "error", "", "rate_limited"},
			gatewayExecution{"tavily", "success", "", ""},
			gatewayExecution{"firecrawl", "not_started", "fallback_satisfied", ""}))
	}
	defaultFetch := func(t *testing.T) {
		t.Helper()
		assertFetch(t, assertRun(t, fetchArgs, []string{"/default/tavily/extract", "/default/firecrawl/v2/scrape"},
			gatewayExecution{"tavily", "error", "", "rate_limited"},
			gatewayExecution{"firecrawl", "success", "", ""},
			gatewayExecution{"exa", "not_started", "fallback_satisfied", ""}), "firecrawl", "default body")
	}
	t.Run("AC41", func(t *testing.T) {
		defaultSearch(t)
		defaultFetch(t)
	})
	t.Run("AC42", func(t *testing.T) {
		explicitPath := filepath.Join(t.TempDir(), "explicit config", "config.toml")
		stdout, stderr := runCLIProcess(t, 0, "init", "--config", explicitPath)
		if stdout != explicitPath+"\n" || stderr != "" || !bytes.Equal(readConfigFixture(t, defaultPath), defaultFile) {
			t.Fatal("explicit init must print its path and preserve the default file")
		}
		explicitFile := strings.Replace(string(readConfigFixture(t, explicitPath)), "[providers.tavily]", providerSettings("tavily", "/explicit"), 1)
		if err := os.WriteFile(explicitPath, []byte(explicitFile), 0600); err != nil {
			t.Fatal(err)
		}
		defaultSearch(t)
		assertSearch(t, assertRun(t, append(searchArgs, "--config", explicitPath), []string{"/explicit/tavily/search"},
			gatewayExecution{"tavily", "success", "", ""}))
		defaultSearch(t)
		defaultFetch(t)
		assertFetch(t, assertRun(t, append(fetchArgs, "--config", explicitPath), []string{"/explicit/tavily/extract"},
			gatewayExecution{"tavily", "success", "", ""}), "tavily", "explicit body")
		defaultFetch(t)
		stdout, stderr = runCLIProcess(t, 1, "init", "--config", filepath.Dir(explicitPath))
		if stdout != "" || stderr != "cannot write the configuration file\n" {
			t.Fatalf("directory target stdout = %q, stderr = %q, want only a controlled write diagnostic on stderr", stdout, stderr)
		}
		if !bytes.Equal(readConfigFixture(t, defaultPath), defaultFile) {
			t.Fatal("default file changed during explicit path checks")
		}
	})
	t.Run("AC42SymlinkParent", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("requires Unix path resolution through a symlink followed by ..")
		}
		tree := t.TempDir()
		child := filepath.Join(tree, "nested", "child")
		if err := os.MkdirAll(child, 0700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(tree, "link")
		if err := os.Symlink(child, link); err != nil {
			t.Fatal(err)
		}
		// Pass the path verbatim so the filesystem resolves .. after following the link.
		path := link + "/../config-parent/config.toml"
		stdout, stderr := runCLIProcess(t, 0, "init", "--config", path)
		if stdout != path+"\n" || stderr != "" {
			t.Fatalf("init stdout = %q, stderr = %q, want only the requested path on stdout", stdout, stderr)
		}
		assertMissingConfig(t, filepath.Join(tree, "config-parent"))
		resolved := filepath.Join(tree, "nested", "config-parent", "config.toml")
		configured := strings.Replace(string(readConfigFixture(t, resolved)), "[providers.tavily]", providerSettings("tavily", "/explicit"), 1)
		if err := os.WriteFile(resolved, []byte(configured), 0600); err != nil {
			t.Fatal(err)
		}
		assertSearch(t, assertRun(t, append(searchArgs, "--config", path), []string{"/explicit/tavily/search"},
			gatewayExecution{"tavily", "success", "", ""}))
		assertFetch(t, assertRun(t, append(fetchArgs, "--config", path), []string{"/explicit/tavily/extract"},
			gatewayExecution{"tavily", "success", "", ""}), "tavily", "explicit body")
	})
}

func readConfigFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertMissingConfig(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration file should be absent: %v", err)
	}
}
