package cli

import (
	"strings"
	"time"

	"github.com/spf13/cobra"

	"linkweft/internal/config"
	"linkweft/internal/retrieval"
)

type commonFlags struct {
	providers string
	config    string
	timeout   string
	output    string
}

func (f *commonFlags) bind(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVar(&f.providers, "providers", "", "comma-separated providers")
	flags.StringVar(&f.config, "config", "", "path to the TOML configuration file")
	flags.StringVar(&f.timeout, "timeout", "", "command timeout (for example 45s)")
	flags.StringVar(&f.output, "output", "json", "output format: json or text")
}

func (f commonFlags) load(cmd *cobra.Command) (config.Config, []string, time.Duration, error) {
	var cfg config.Config
	if f.output != "json" && f.output != "text" {
		return cfg, nil, 0, argError("output must be json or text")
	}
	flags := cmd.Flags()
	var names []string
	if flags.Changed("providers") {
		names = strings.Split(f.providers, ",")
		for i := range names {
			names[i] = strings.TrimSpace(names[i])
		}
		if !config.ValidProviders(names) {
			return cfg, nil, 0, argError("providers must be distinct known names")
		}
	}
	var timeout time.Duration
	if flags.Changed("timeout") {
		value, err := time.ParseDuration(f.timeout)
		if err != nil || value <= 0 {
			return cfg, nil, 0, argError("timeout must be a positive duration")
		}
		timeout = value
	}
	var path *string
	if flags.Changed("config") {
		path = &f.config
	}
	cfg, err := config.Load(path)
	if err != nil {
		return cfg, nil, 0, &retrieval.Error{Code: "invalid_config", Message: err.Error()}
	}
	return cfg, names, timeout, nil
}
