package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"

	"linkweft/internal/cli"
	"linkweft/internal/provider"
	"linkweft/internal/retrieval"
)

var version = "dev"

func main() {
	os.Exit(run(newApp(nil), os.Args[1:]))
}

func newApp(client *http.Client) cli.App {
	return cli.App{
		Version: version,
		NewTavily: func(key, baseURL string) retrieval.SearchProvider {
			return provider.NewTavily(key, client, baseURL)
		},
		NewExa: func(key, baseURL string) retrieval.SearchProvider {
			return provider.NewExa(key, client, baseURL)
		},
		NewKeenable: func(key, baseURL string) retrieval.SearchProvider {
			return provider.NewKeenable(key, client, baseURL)
		},
		NewFirecrawl: func(key, baseURL string) retrieval.SearchProvider {
			return provider.NewFirecrawl(key, client, baseURL)
		},
		NewAnySearch: func(key, baseURL string) retrieval.SearchProvider {
			return provider.NewAnySearch(key, client, baseURL)
		},
		NewTinyFish: func(key, baseURL string) retrieval.SearchProvider {
			return provider.NewTinyFish(key, client, baseURL)
		},
		NewTavilyFetch: func(key, baseURL string) retrieval.FetchProvider {
			return provider.NewTavilyFetch(key, client, baseURL)
		},
		NewExaFetch: func(key, baseURL string) retrieval.FetchProvider {
			return provider.NewExaFetch(key, client, baseURL)
		},
		NewKeenableFetch: func(key, baseURL string) retrieval.FetchProvider {
			return provider.NewKeenableFetch(key, client, baseURL)
		},
		NewFirecrawlFetch: func(key, baseURL string) retrieval.FetchProvider {
			return provider.NewFirecrawlFetch(key, client, baseURL)
		},
		NewAnySearchFetch: func(key, baseURL string) retrieval.FetchProvider {
			return provider.NewAnySearchFetch(key, client, baseURL)
		},
		NewTinyFishFetch: func(key, baseURL string) retrieval.FetchProvider {
			return provider.NewTinyFishFetch(key, client, baseURL)
		},
	}
}

func run(app cli.App, args []string) int {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	go func() {
		select {
		case <-interrupts:
			cancel(retrieval.ErrUserCancelled)
		case <-ctx.Done():
		}
	}()
	return app.Run(ctx, args, os.Stdout, os.Stderr)
}
