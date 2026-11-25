package main

import (
	"context"
	"os"
	"os/signal"

	"linkweft/internal/cli"
	"linkweft/internal/provider"
	"linkweft/internal/retrieval"
)

func main() {
	app := cli.App{
		NewTavily: func(key string) retrieval.SearchProvider {
			return provider.NewTavily(key, nil, "")
		},
		NewExa: func(key string) retrieval.SearchProvider {
			return provider.NewExa(key, nil, "")
		},
		NewTavilyFetch: func(key string) retrieval.FetchProvider {
			return provider.NewTavilyFetch(key, nil, "")
		},
		NewExaFetch: func(key string) retrieval.FetchProvider {
			return provider.NewExaFetch(key, nil, "")
		},
	}
	os.Exit(run(app, os.Args[1:]))
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
