# Linkweft

**Web search and page content retrieval for terminals, scripts, and AI Agents.**

[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/dengshuit/linkweft)](https://github.com/dengshuit/linkweft/releases)
[![npm version](https://img.shields.io/npm/v/@panden/linkweft)](https://www.npmjs.com/package/@panden/linkweft)
[![Go](https://img.shields.io/github/go-mod/go-version/dengshuit/linkweft)](go.mod)
[![CI](https://github.com/dengshuit/linkweft/actions/workflows/ci.yml/badge.svg)](https://github.com/dengshuit/linkweft/actions/workflows/ci.yml)

**English** | [简体中文](README.zh-CN.md)

[Quick Start](#quick-start) · [Supported Providers](#supported-providers) · [Common Commands](#common-commands) · [Configuration](#configuration) · [Output and Troubleshooting](#output-and-troubleshooting) · [License](#license)

## What is Linkweft?

Linkweft is a command-line tool for web search and page content retrieval. It brings multiple providers behind a consistent set of commands, so you can look up information in your terminal, build retrieval scripts, or give an AI Agent access to web sources.

## Features

- **Web search**: Use `search` to find page URLs, titles, and snippets. Available content depends on the provider.
- **Page content retrieval**: Use `fetch` to retrieve the main content of a known URL, with an optional character limit.
- **Automatic fallback**: Try configured providers in order, stopping once a search returns valid results or a fetch succeeds.
- **Search fusion**: Use the `merge` strategy to query multiple providers concurrently, deduplicate by URL, combine rankings, and preserve source information.
- **Terminal and automation output**: Get JSON by default or switch to readable text, with provider execution records, error details, and timeout controls.
- **AI Agent Skill**: The included `linkweft` Skill guides AI Agents that support Skills through CLI usage, source selection, and page retrieval.

## Quick Start

Install and configure Linkweft manually, or let an AI Agent do it for you.

### Option 1: Install Manually

#### 1. Install the CLI

The recommended installation method is npm. It requires **Node.js 24 or later**.

```bash
npm install -g @panden/linkweft
linkweft --version
linkweft --help
```

npm automatically installs the executable for your platform. Linux, macOS, and Windows are supported on x64 and ARM64, with no Go installation required. See the [npm package](https://www.npmjs.com/package/@panden/linkweft).

<details>
<summary>Other installation methods: standalone executable or source</summary>

**Standalone executable**

Download the file for your platform from [GitHub Releases](https://github.com/dengshuit/linkweft/releases/latest). Standalone executables run without Node.js, npm, or Go.

| OS | x64 filename suffix | ARM64 filename suffix |
| --- | --- | --- |
| Linux | `linux-amd64` | `linux-arm64` |
| macOS | `darwin-amd64` | `darwin-arm64` |
| Windows | `windows-amd64.exe` | `windows-arm64.exe` |

Rename the downloaded file to `linkweft` (`linkweft.exe` on Windows) and place it in a directory on your `PATH`. On Linux and macOS, also make the file executable:

```bash
chmod +x linkweft
```

Run `linkweft --version` to check the installation.

**Install from source**

If you have Go installed, you can build and install locally. See [go.mod](go.mod) for the required Go version:

```bash
git clone https://github.com/dengshuit/linkweft.git
cd linkweft
go install ./cmd/linkweft
```

Make sure the directory where Go installs executables is on your `PATH`: use `GOBIN` if set, otherwise it is usually `$(go env GOPATH)/bin`. A source installation reports `dev` when you run `linkweft --version`.

</details>

#### 2. Configure a Provider

```bash
linkweft init
```

The command prints the configuration file path. Open that file, uncomment `api_key` in your chosen provider's section, and enter your key. For example, with Exa:

```toml
[providers.exa]
api_key = "your-exa-api-key"
```

The generated template already contains a section for each provider; edit the relevant section directly. Running `linkweft init` again preserves the existing file.

You can also provide the key through an environment variable, without creating a configuration file first. For Bash / Zsh:

```bash
export EXA_API_KEY="your-exa-api-key"
```

For PowerShell:

```powershell
$env:EXA_API_KEY = "your-exa-api-key"
```

A nonempty environment variable takes precedence over `api_key` in the configuration file. See [Supported Providers](#supported-providers) for the other variables.

#### 3. Install the Skill

After installing the CLI and configuring a key, run this in your target project to install the [`linkweft` Skill](skills/linkweft/SKILL.md) with the [Skills CLI](https://github.com/vercel-labs/skills):

```bash
npx skills add dengshuit/linkweft --skill linkweft
```

Follow the prompts, then have your AI Agent reload its Skills. Make sure its shell environment can access `linkweft` and the provider credentials.

#### 4. Verify the Installation

Search the web and retrieve the content of a known page:

```bash
linkweft search --output text -- "Go HTTP client timeout settings"
linkweft fetch --output text -- "https://go.dev/doc/"
```

Omit `--output text` to get JSON for scripts and AI Agents. Requests go through your selected providers' APIs. Obtain a key from the provider before use; quotas and pricing are determined by that provider.

### Option 2: Let an AI Agent Install It

Give your AI Agent this prompt to install and configure both the CLI and the Skill:

```text
Follow the README at https://github.com/dengshuit/linkweft to install and configure the Linkweft CLI and Skill for this AI Agent. Reuse existing settings and API keys; if a key is missing, ask me to set it locally. Never print keys. Verify search and page fetching.
```

## Supported Providers

All six providers below support both search and page content retrieval. Configure an API key for at least one provider to get started.

| Provider | CLI name | API key environment variable |
| --- | --- | --- |
| Tavily | `tavily` | `TAVILY_API_KEY` |
| Exa | `exa` | `EXA_API_KEY` |
| Keenable | `keenable` | `KEENABLE_API_KEY` |
| Firecrawl | `firecrawl` | `FIRECRAWL_API_KEY` |
| AnySearch | `anysearch` | `ANYSEARCH_API_KEY` |
| TinyFish | `tinyfish` | `TINYFISH_API_KEY` |

The default fallback order runs from left to right:

| Command | Default provider priority |
| --- | --- |
| `search` (`fallback`) | Exa → Keenable → TinyFish → AnySearch → Firecrawl → Tavily |
| `fetch` | TinyFish → Keenable → Exa → Tavily → AnySearch → Firecrawl |

Providers without a configured key are excluded. Adjust `priority` in the configuration file to change the order, or use `--providers` to select the providers and their order for a single command.

## Common Commands

### Control Search Providers and Strategy

```bash
# Return up to 5 results
linkweft search --limit 5 -- "Go HTTP timeout"

# Use only Exa; an Exa key must be configured
linkweft search --providers exa -- "Go HTTP timeout"

# Fall back in the specified order; both Tavily and Exa keys must be configured
linkweft search --providers tavily,exa -- "Go HTTP timeout"

# Query both providers concurrently and merge the results
linkweft search --strategy merge --providers tavily,exa --limit 10 -- "Go HTTP timeout"
```

The default strategy is `fallback`: if a provider returns an error, times out, or has no valid results, Linkweft tries the next provider within the remaining total time budget. It stops as soon as one provider returns at least one valid result URL, even if the result count is below `--limit`.

`merge` queries the candidate providers and combines their results. If some providers fail but usable results remain, the command returns `partial` with exit code `0`.

### Control Content Length and Timeouts

```bash
# Return up to 12000 Unicode characters of page content
linkweft fetch --max-chars 12000 -- "https://go.dev/doc/"

# Set the total time budget for this retrieval
linkweft fetch --timeout 30s --output text -- "https://go.dev/doc/"
```

`fetch` accepts one absolute HTTP(S) URL per call. It tries providers in order and stops when one returns accepted page content. `--max-chars` limits only `document.content.text`, not the entire JSON response or token count, and does not apply to `search`.

### Common Options

| Option | Commands | Purpose and default |
| --- | --- | --- |
| `--providers` | `search`, `fetch` | Comma-separated provider names; defaults to configured providers with API keys |
| `--output` | `search`, `fetch` | `json` (default) or `text` |
| `--config` | `init`, `search`, `fetch` | Path to a TOML configuration file |
| `--timeout` | `search`, `fetch` | Total time budget, such as `30s` or `2m`; defaults to `45s` for search and `90s` for fetch |
| `--strategy` | `search` | `fallback` (default) or `merge` |
| `--limit` | `search` | Maximum number of results, default `10`; provider limits may yield fewer results |
| `--max-chars` | `fetch` | Maximum Unicode characters of page content, default `50000` |

When `--providers` is explicit, only the listed providers are used, and each must have a configured key. Run `linkweft search --help` or `linkweft fetch --help` for more help.

## Configuration

The default configuration file follows your operating system's user configuration directory:

| OS | Default path |
| --- | --- |
| Linux | `$XDG_CONFIG_HOME/linkweft/config.toml`, or `~/.config/linkweft/config.toml` when unset |
| macOS | `~/Library/Application Support/linkweft/config.toml` |
| Windows | `%AppData%\linkweft\config.toml` |

You can also create and use a separate configuration file:

```bash
linkweft init --config ./linkweft.toml
linkweft search --config ./linkweft.toml -- "Go HTTP timeout"
```

CLI options override their corresponding configuration values. The example below adjusts provider priority, result limits, and timeouts. Edit the matching sections in your existing file:

```toml
[search]
priority = ["exa", "tavily"]
limit = 10
timeout = "45s"
max_concurrency = 3

[fetch]
priority = ["tavily", "exa"]
max_chars = 50000
timeout = "90s"

[providers.exa]
api_key = "your-exa-api-key"
search_timeout = "15s"
fetch_timeout = "45s"
search_snippets = true

[providers.tavily]
api_key = "your-tavily-api-key"
```

- `priority` changes the order. Other providers with configured keys are still appended. Use `--providers` to restrict the providers for a single command.
- `max_concurrency` sets the maximum concurrent requests for `merge` searches, default `3`.
- Each provider's `search_timeout` defaults to `15s`, and `fetch_timeout` to `45s`. A request is bounded by the shorter of its provider timeout and the remaining total budget.
- Exa requests search snippets by default. Set `search_snippets = false` to disable them.
- For a compatible API gateway or proxy, set `base_url` for the provider, or set `search_base_url` and `fetch_base_url` separately. Supply a base address; the CLI appends the endpoint path. Operation-specific addresses take precedence over the shared `base_url`.

## Output and Troubleshooting

By default, `search` and `fetch` write JSON to stdout and warnings to stderr. Useful JSON fields include:

| Field | Purpose |
| --- | --- |
| `outcome` | Overall command outcome |
| `results` | Search results and their `sources` information |
| `document.content.text` | Retrieved page content |
| `executions` | Provider status, duration, skip reasons, and errors |
| `error`, `warnings` | Overall error and warnings |

| `outcome` | Meaning | Exit code |
| --- | --- | --- |
| `success` | Search results or page content are available | `0` |
| `empty` | Search completed with no results | `0` |
| `partial` | Merged search has results, but some providers failed or did not finish | `0` |
| `error` | Retrieval failed, or arguments or configuration are invalid | `1` for retrieval failure; `2` for argument, configuration, or credential issues |
| `cancelled` | The user interrupted retrieval | `130` |

Scripts should check both the exit code and `outcome`. Commands may still return JSON on a nonzero exit. Save results and diagnostics separately; with `jq` installed, inspect individual fields:

```bash
linkweft search -- "Go HTTP timeout" > result.json 2> diagnostics.log
jq '.outcome, .error, .executions' result.json
jq -r '.results[].url' result.json
```

Common issues:

- **`no_provider_available`**: No usable provider is configured. Check the configuration path and API keys.
- **`missing_credentials`**: A provider explicitly selected with `--providers` has no key. Configure it or adjust the list.
- **`invalid_config`**: Check TOML syntax, option names, and values. An explicitly selected configuration file must exist. Configuration keys are case-sensitive and use lowercase names.
- **`search_failed` / `fetch_failed`**: Inspect `executions[].error` for provider authentication, rate limiting, upstream errors, or timeouts.
- **Incomplete content**: If `document.content.local_truncated` is `true`, increase `--max-chars`. `upstream_truncated: true` means the provider already truncated the content; `null` means the upstream truncation status is unknown.

A page may return a login prompt, verification page, or error content. Even with `outcome: success`, check that the content matches your request. When citing a page, prefer `document.final_url`, falling back to `document.requested_url` when it is absent.

## License

This project is licensed under the [MIT License](LICENSE).
