package cli

import (
	"strconv"

	"github.com/spf13/cobra"

	"linkweft/internal/retrieval"
)

type searchFlags struct {
	commonFlags
	strategy string
	limit    string
}

func (f *searchFlags) bind(cmd *cobra.Command) {
	f.commonFlags.bind(cmd)
	flags := cmd.Flags()
	flags.StringVar(&f.strategy, "strategy", "fallback", "search strategy: fallback or merge")
	flags.StringVar(&f.limit, "limit", "", "maximum number of results")
}

func (a App) search(cmd *cobra.Command, query string, opts searchFlags) (retrieval.Response, int, error) {
	var response retrieval.Response
	flags := cmd.Flags()
	if opts.strategy != "fallback" && opts.strategy != "merge" {
		return response, 2, argError("strategy must be fallback or merge")
	}
	var limit int
	if flags.Changed("limit") {
		value, err := strconv.Atoi(opts.limit)
		if err != nil || value <= 0 {
			return response, 2, argError("limit must be a positive integer")
		}
		limit = value
	}
	cfg, names, timeout, err := opts.load(cmd)
	if err != nil {
		return response, 2, err
	}
	explicit := names != nil
	if !explicit {
		names = cfg.ConfiguredSearchProviders
	}
	if limit == 0 {
		limit = cfg.Search.Limit
	}
	if timeout == 0 {
		timeout = cfg.Search.Timeout
	}
	factories := map[string]func(string, string) retrieval.SearchProvider{
		"tavily": a.NewTavily, "exa": a.NewExa, "keenable": a.NewKeenable, "firecrawl": a.NewFirecrawl,
		"anysearch": a.NewAnySearch, "tinyfish": a.NewTinyFish,
	}
	registry := make(map[string]retrieval.SearchProvider, len(names))
	for _, name := range names {
		factory := factories[name]
		if factory == nil {
			continue
		}
		var entry retrieval.SearchProvider
		if settings := cfg.Providers[name]; settings.APIKey != "" {
			entry = factory(settings.APIKey, settings.SearchBaseURL)
			entry.Timeout = settings.SearchTimeout
		}
		registry[name] = entry
	}
	request := retrieval.SearchRequest{Query: query, Limit: limit}
	options := retrieval.SearchOptions{Timeout: timeout, Strategy: opts.strategy, MaxConcurrency: cfg.Search.MaxConcurrency}
	response, exitCode := retrieval.Search(cmd.Context(), request, options, names, explicit, registry, cfg.APIKeys)
	return response, exitCode, nil
}
