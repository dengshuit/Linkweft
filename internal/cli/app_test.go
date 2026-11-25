package cli_test

import (
	"context"
	"testing"

	"linkweft/internal/cli"
)

func TestVersionDoesNotHideUnknownCommand(t *testing.T) {
	app := cli.App{Version: "0.1.0-local.1"}
	stdout, stderr := run(t, context.Background(), app, 0, "--version")
	if stdout != app.Version+"\n" || stderr != "" {
		t.Fatalf("version stdout = %q, stderr = %q", stdout, stderr)
	}

	stdout, stderr = run(t, context.Background(), app, 2, "--version", "unknown-command")
	response := envelope(t, stdout, nil, "error")
	objectCode(t, response["error"], "invalid_argument")
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}
