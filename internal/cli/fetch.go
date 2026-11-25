package cli

import (
	"strconv"

	"github.com/spf13/cobra"

	"linkweft/internal/retrieval"
)

type fetchFlags struct {
	commonFlags
	maxChars string
}

func (f *fetchFlags) bind(cmd *cobra.Command) {
	f.commonFlags.bind(cmd)
	cmd.Flags().StringVar(&f.maxChars, "max-chars", "", "maximum number of Unicode characters")
}

func (a App) fetch(cmd *cobra.Command, address string, opts fetchFlags) (retrieval.Response, int, error) {
	var response retrieval.Response
	if !retrieval.ValidFetchURL(address) {
		return response, 2, argError("fetch requires an absolute HTTP(S) URL with a hostname")
	}
	var maxChars int
	if cmd.Flags().Changed("max-chars") {
		value, err := strconv.Atoi(opts.maxChars)
		if err != nil || value <= 0 {
			return response, 2, argError("max-chars must be a positive integer")
		}
		maxChars = value
	}
	cfg, names, timeout, err := opts.load(cmd)
	if err != nil {
		return response, 2, err
	}
	explicit := names != nil
	if !explicit {
		names = cfg.ConfiguredFetchProviders
	}
	if maxChars == 0 {
		maxChars = cfg.Fetch.MaxChars
	}
	if timeout == 0 {
		timeout = cfg.Fetch.Timeout
	}
	factories := map[string]func(string) retrieval.FetchProvider{
		"tavily": a.NewTavilyFetch, "exa": a.NewExaFetch, "keenable": a.NewKeenableFetch, "firecrawl": a.NewFirecrawlFetch,
	}
	registry := make(map[string]retrieval.FetchProvider, len(names))
	for _, name := range names {
		factory := factories[name]
		if factory == nil {
			continue
		}
		var entry retrieval.FetchProvider
		if settings := cfg.Providers[name]; settings.APIKey != "" {
			entry = factory(settings.APIKey)
			entry.Timeout = settings.FetchTimeout
		}
		registry[name] = entry
	}
	options := retrieval.FetchOptions{Timeout: timeout, MaxChars: maxChars}
	response, exitCode := retrieval.Fetch(cmd.Context(), address, options, names, explicit, registry, cfg.APIKeys)
	return response, exitCode, nil
}
