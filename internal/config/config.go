package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"

	"linkweft/internal/httpurl"
)

var searchOrder = []string{"exa", "keenable", "tinyfish", "anysearch", "firecrawl", "tavily"}
var fetchOrder = []string{"tinyfish", "keenable", "exa", "tavily", "anysearch", "firecrawl"}

type Config struct {
	Search                    Search
	Fetch                     Fetch
	Providers                 map[string]Provider
	ConfiguredSearchProviders []string
	ConfiguredFetchProviders  []string
	APIKeys                   []string
}

type Search struct {
	Limit          int
	Timeout        time.Duration
	MaxConcurrency int
}

type Fetch struct {
	Timeout  time.Duration
	MaxChars int
}

type Provider struct {
	APIKey         string
	SearchSnippets bool
	SearchTimeout  time.Duration
	FetchTimeout   time.Duration
	SearchBaseURL  string
	FetchBaseURL   string
}

type fileConfig struct {
	Search struct {
		Priority       []string `toml:"priority"`
		Timeout        *string  `toml:"timeout"`
		Limit          *int     `toml:"limit"`
		MaxConcurrency *int     `toml:"max_concurrency"`
	} `toml:"search"`
	Fetch struct {
		Priority []string `toml:"priority"`
		Timeout  *string  `toml:"timeout"`
		MaxChars *int     `toml:"max_chars"`
	} `toml:"fetch"`
	Providers map[string]struct {
		APIKey         string  `toml:"api_key"`
		SearchSnippets *bool   `toml:"search_snippets"`
		SearchTimeout  *string `toml:"search_timeout"`
		FetchTimeout   *string `toml:"fetch_timeout"`
		BaseURL        *string `toml:"base_url"`
		SearchBaseURL  *string `toml:"search_base_url"`
		FetchBaseURL   *string `toml:"fetch_base_url"`
	} `toml:"providers"`
}

func Load(path *string) (Config, error) {
	file, err := readFile(path)
	if err != nil {
		return Config{}, err
	}
	return resolve(file)
}

func configPath(path *string) (string, error) {
	if path != nil {
		return *path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("cannot locate the user configuration directory")
	}
	return filepath.Join(dir, "linkweft", "config.toml"), nil
}

func readFile(path *string) (fileConfig, error) {
	var cfg fileConfig
	filename, err := configPath(path)
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(filename)
	if path == nil && errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, errors.New("cannot read the configuration file")
	}
	// The schema uses lowercase keys, while TOML struct decoding also accepts case-folded aliases.
	var fields map[string]any
	if err := toml.Unmarshal(data, &fields); err != nil {
		return cfg, errors.New("cannot decode the TOML configuration file")
	}
	if !lowercaseKeys(fields) {
		return cfg, errors.New("configuration keys must use their exact lowercase spelling")
	}
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&cfg); err != nil {
		return cfg, errors.New("cannot decode the TOML configuration file")
	}
	return cfg, nil
}

func lowercaseKeys(fields map[string]any) bool {
	for key, value := range fields {
		if key != strings.ToLower(key) {
			return false
		}
		if table, ok := value.(map[string]any); ok && !lowercaseKeys(table) {
			return false
		}
	}
	return true
}

func resolve(file fileConfig) (Config, error) {
	cfg := Config{
		Search:    Search{Limit: 10, Timeout: 45 * time.Second, MaxConcurrency: 3},
		Fetch:     Fetch{Timeout: 90 * time.Second, MaxChars: 50_000},
		Providers: make(map[string]Provider),
	}
	if !validPositiveInt(file.Search.Limit) || !validPositiveInt(file.Search.MaxConcurrency) ||
		!validPositiveInt(file.Fetch.MaxChars) {
		return Config{}, errors.New("configuration counts must be positive integers")
	}
	if !ValidProviders(file.Search.Priority) || !ValidProviders(file.Fetch.Priority) {
		return Config{}, errors.New("configuration priorities must contain distinct known providers")
	}
	var ok bool
	cfg.Search.Timeout, ok = durationOrDefault(file.Search.Timeout, cfg.Search.Timeout)
	if !ok {
		return Config{}, errors.New("search.timeout must be a positive duration")
	}
	cfg.Fetch.Timeout, ok = durationOrDefault(file.Fetch.Timeout, cfg.Fetch.Timeout)
	if !ok {
		return Config{}, errors.New("fetch.timeout must be a positive duration")
	}
	if file.Search.Limit != nil {
		cfg.Search.Limit = *file.Search.Limit
	}
	if file.Search.MaxConcurrency != nil {
		cfg.Search.MaxConcurrency = *file.Search.MaxConcurrency
	}
	if file.Fetch.MaxChars != nil {
		cfg.Fetch.MaxChars = *file.Fetch.MaxChars
	}
	for name := range file.Providers {
		if !slices.Contains(searchOrder, name) {
			return Config{}, errors.New("configuration contains an unknown provider")
		}
	}
	for _, name := range searchOrder {
		entry := file.Providers[name]
		if entry.SearchSnippets != nil && name != "exa" {
			return Config{}, errors.New("providers." + name + ".search_snippets is only supported for exa")
		}
		for _, field := range []struct {
			name  string
			value *string
		}{
			{"base_url", entry.BaseURL},
			{"search_base_url", entry.SearchBaseURL},
			{"fetch_base_url", entry.FetchBaseURL},
		} {
			if field.value != nil && !validBaseURL(*field.value) {
				return Config{}, errors.New("providers." + name + "." + field.name +
					" must be an absolute HTTP(S) URL with a hostname and without user info, query, or fragment")
			}
		}
		baseURL := baseURLOrDefault(entry.BaseURL, "")
		timeout, ok := durationOrDefault(entry.SearchTimeout, 15*time.Second)
		if !ok {
			return Config{}, errors.New("provider search_timeout must be a positive duration")
		}
		fetchTimeout, ok := durationOrDefault(entry.FetchTimeout, 45*time.Second)
		if !ok {
			return Config{}, errors.New("provider fetch_timeout must be a positive duration")
		}
		key := os.Getenv(strings.ToUpper(name) + "_API_KEY")
		if key == "" {
			key = entry.APIKey
		}
		key = strings.TrimSpace(key)
		cfg.Providers[name] = Provider{
			APIKey: key, SearchTimeout: timeout, FetchTimeout: fetchTimeout,
			SearchSnippets: entry.SearchSnippets == nil || *entry.SearchSnippets,
			SearchBaseURL:  baseURLOrDefault(entry.SearchBaseURL, baseURL),
			FetchBaseURL:   baseURLOrDefault(entry.FetchBaseURL, baseURL),
		}
		// A file key overridden by the environment still needs protection in output.
		for _, value := range []string{key, strings.TrimSpace(entry.APIKey)} {
			if value != "" && !slices.Contains(cfg.APIKeys, value) {
				cfg.APIKeys = append(cfg.APIKeys, value)
			}
		}
	}
	cfg.ConfiguredSearchProviders = configuredProviders(file.Search.Priority, searchOrder, cfg.Providers)
	cfg.ConfiguredFetchProviders = configuredProviders(file.Fetch.Priority, fetchOrder, cfg.Providers)
	return cfg, nil
}

func validBaseURL(raw string) bool {
	// Reject the delimiters themselves, including empty queries and fragments.
	if !utf8.ValidString(raw) || strings.ContainsFunc(raw, unicode.IsSpace) || strings.ContainsAny(raw, "?#") {
		return false
	}
	address := httpurl.Parse(raw)
	return address != nil && address.User == nil
}

func baseURLOrDefault(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func configuredProviders(priority, defaults []string, providers map[string]Provider) []string {
	ordered := slices.Clone(priority)
	for _, name := range defaults {
		if !slices.Contains(priority, name) {
			ordered = append(ordered, name)
		}
	}
	return slices.DeleteFunc(ordered, func(name string) bool { return providers[name].APIKey == "" })
}

func ValidProviders(names []string) bool {
	for i, name := range names {
		if !slices.Contains(searchOrder, name) || slices.Contains(names[:i], name) {
			return false
		}
	}
	return true
}

func validPositiveInt(value *int) bool {
	return value == nil || *value > 0
}

func durationOrDefault(value *string, fallback time.Duration) (time.Duration, bool) {
	if value == nil {
		return fallback, true
	}
	duration, err := time.ParseDuration(*value)
	return duration, err == nil && duration > 0
}
