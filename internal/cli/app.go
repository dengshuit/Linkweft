package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"linkweft/internal/retrieval"
)

type App struct {
	NewTavily        func(apiKey string) retrieval.SearchProvider
	NewExa           func(apiKey string) retrieval.SearchProvider
	NewKeenable      func(apiKey string) retrieval.SearchProvider
	NewTavilyFetch   func(apiKey string) retrieval.FetchProvider
	NewExaFetch      func(apiKey string) retrieval.FetchProvider
	NewKeenableFetch func(apiKey string) retrieval.FetchProvider
}

func (a App) Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := &cobra.Command{
		Use:           "linkweft",
		Short:         "Search the web and fetch page content",
		SilenceErrors: true,
		SilenceUsage:  true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}
	root.SetArgs(append([]string{}, args...))
	root.SetOut(stdout)
	root.SetErr(stderr)

	var response retrieval.Response
	var exitCode int
	var searchOpts searchFlags
	search := &cobra.Command{
		Use:   "search QUERY",
		Short: "Search for web pages",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return argError("search requires exactly one nonblank query")
			}
			var err error
			response, exitCode, err = a.search(cmd, args[0], searchOpts)
			return err
		},
	}
	searchOpts.bind(search)
	var fetchOpts fetchFlags
	fetch := &cobra.Command{
		Use:   "fetch URL",
		Short: "Fetch the main content of one web page",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return argError("fetch requires exactly one URL")
			}
			var err error
			response, exitCode, err = a.fetch(cmd, args[0], fetchOpts)
			return err
		},
	}
	fetchOpts.bind(fetch)
	root.AddCommand(search, fetch)
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command]",
		Short: "Show command help",
		RunE: func(_ *cobra.Command, args []string) error {
			switch {
			case len(args) == 0:
				return root.Help()
			case len(args) == 1 && args[0] == "search":
				search.InitDefaultHelpFlag()
				return search.Help()
			case len(args) == 1 && args[0] == "fetch":
				fetch.InitDefaultHelpFlag()
				return fetch.Help()
			default:
				return argError("unknown help topic; use --help for usage")
			}
		},
	})

	// Resolve the public command tree before Cobra can install its hidden completion commands.
	root.InitDefaultHelpCmd()
	executed, _, err := root.Find(args)
	if err == nil {
		executed, err = root.ExecuteContextC(ctx)
	}
	var operation *string
	output := "json"
	switch executed {
	case search:
		name := "search"
		operation, output = &name, searchOpts.output
	case fetch:
		name := "fetch"
		operation, output = &name, fetchOpts.output
	}
	if err != nil {
		var issue *retrieval.Error
		if !errors.As(err, &issue) {
			issue = argError("invalid command or option; use --help for usage")
		}
		response = retrieval.NewResponse(operation)
		response.Outcome, response.Error = "error", issue
		exitCode = 2
	}
	if response.SchemaVersion == 0 {
		return 0
	}
	if err := writeResponse(stdout, stderr, response, output); err != nil {
		fmt.Fprintln(stderr, "cannot write the command response")
		if exitCode == 0 {
			return 1
		}
	}
	return exitCode
}

func argError(message string) *retrieval.Error {
	return &retrieval.Error{Code: "invalid_argument", Message: message}
}
