package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"linkweft/internal/retrieval"
)

func writeResponse(stdout, stderr io.Writer, response retrieval.Response, format string) error {
	for _, warning := range response.Warnings {
		fmt.Fprintf(stderr, "%s: %s\n", warning.Code, warning.Message)
	}
	if format != "text" {
		return json.NewEncoder(stdout).Encode(response)
	}
	if response.Error != nil {
		fmt.Fprintf(stderr, "%s: %s\n", response.Error.Code, response.Error.Message)
	}
	for _, execution := range response.Executions {
		fmt.Fprintf(stderr, "%s: %s", execution.Provider, execution.Status)
		if execution.Reason != nil {
			fmt.Fprintf(stderr, ": %s", *execution.Reason)
		} else if execution.Error != nil {
			fmt.Fprintf(stderr, ": %s", execution.Error.Code)
		}
		if execution.Error != nil && execution.Error.Code != "user_cancelled" {
			fmt.Fprintf(stderr, ": %s", execution.Error.Message)
		}
		fmt.Fprintln(stderr)
	}
	var text strings.Builder
	if response.Outcome == "empty" {
		fmt.Fprintln(&text, "No results.")
	}
	if response.Document != nil {
		text.WriteString(response.Document.Content.Text)
		if !strings.HasSuffix(response.Document.Content.Text, "\n") {
			text.WriteByte('\n')
		}
	}
	for _, result := range response.Results {
		if result.Title != nil {
			fmt.Fprintln(&text, *result.Title)
		}
		fmt.Fprintln(&text, result.URL)
		if result.Snippet != nil {
			fmt.Fprintln(&text, *result.Snippet)
		}
		fmt.Fprintln(&text)
	}
	_, err := io.WriteString(stdout, text.String())
	return err
}
