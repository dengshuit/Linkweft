# Linkweft

**为终端、脚本与 AI Agent 提供网页搜索和正文获取能力。**

[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/dengshuit/linkweft)](https://github.com/dengshuit/linkweft/releases)
[![npm version](https://img.shields.io/npm/v/@panden/linkweft)](https://www.npmjs.com/package/@panden/linkweft)
[![Go](https://img.shields.io/github/go-mod/go-version/dengshuit/linkweft)](go.mod)
[![CI](https://github.com/dengshuit/linkweft/actions/workflows/ci.yml/badge.svg)](https://github.com/dengshuit/linkweft/actions/workflows/ci.yml)

[English](README.md) | **简体中文**

[快速开始](#快速开始) · [支持的服务商](#支持的服务商) · [常用命令](#常用命令) · [配置](#配置) · [输出与排错](#输出与排错) · [许可证](#许可证)

## Linkweft 是什么？

Linkweft 是一个网页搜索与正文获取命令行工具。它把多家服务商的搜索和内容提取能力整合到统一命令中，适合在终端查资料、编写检索脚本，或为 AI Agent 提供联网检索能力。

## 主要能力

- **搜索网页**：通过 `search` 获取候选网页的链接、标题和摘要；具体可用内容取决于服务商。
- **获取正文**：通过 `fetch` 读取一个已知 URL 的页面正文，并按需限制返回字符数。
- **多源回退**：默认按顺序尝试已配置的服务商，搜索拿到有效结果或正文获取成功后停止。
- **多源融合**：搜索支持 `merge` 策略，并发查询多家服务商，按 URL 去重、融合排序，保留结果来源。
- **终端与自动化输出**：默认输出 JSON，也可切换为便于阅读的文本；提供服务商执行记录、错误信息和超时控制。
- **AI Agent Skill**：仓库附带 `linkweft` Skill，指导支持 Skills 的 AI Agent 调用 CLI、筛选来源和读取正文。

## 快速开始

可以手动完成安装配置，也可以交给 AI Agent 执行。

### 方式一：手动安装

#### 1. 安装 CLI

推荐通过 npm 安装，需要 **Node.js 24 或更高版本**。

```bash
npm install -g @panden/linkweft
linkweft --version
linkweft --help
```

npm 会自动安装匹配当前平台的可执行文件。支持 Linux、macOS、Windows 的 x64 和 ARM64 架构，无需安装 Go。包信息见 [npm](https://www.npmjs.com/package/@panden/linkweft)。

<details>
<summary>其他安装方式：独立可执行文件或源码</summary>

**独立可执行文件**

从 [GitHub Releases](https://github.com/dengshuit/linkweft/releases/latest) 下载对应平台的文件。独立可执行文件运行时无需 Node.js、npm 或 Go。

| 系统 | x64 文件名中的平台标识 | ARM64 文件名中的平台标识 |
| --- | --- | --- |
| Linux | `linux-amd64` | `linux-arm64` |
| macOS | `darwin-amd64` | `darwin-arm64` |
| Windows | `windows-amd64.exe` | `windows-arm64.exe` |

将下载的文件重命名为 `linkweft`（Windows 为 `linkweft.exe`），放入 `PATH` 包含的目录。Linux 和 macOS 还需为文件添加执行权限：

```bash
chmod +x linkweft
```

安装后运行 `linkweft --version` 检查是否可用。

**从源码安装**

已安装 Go 的用户也可以在本地构建安装，Go 版本要求见 [go.mod](go.mod)：

```bash
git clone https://github.com/dengshuit/linkweft.git
cd linkweft
go install ./cmd/linkweft
```

确保 Go 的安装目录已加入 `PATH`：设置了 `GOBIN` 时使用该目录，否则通常为 `$(go env GOPATH)/bin`。这种安装方式下，`linkweft --version` 显示 `dev`。

</details>

#### 2. 配置服务商

```bash
linkweft init
```

命令会输出配置文件路径。打开该文件，在所选服务商的配置表中取消 `api_key` 的注释并填写密钥。例如，使用 Exa：

```toml
[providers.exa]
api_key = "替换为你的 Exa API Key"
```

生成的模板已经包含各家服务商的配置表，直接编辑对应位置即可。重复运行 `linkweft init` 会保留已有文件。

也可以通过环境变量提供密钥。使用环境变量时，无需先创建配置文件。Bash / Zsh 示例：

```bash
export EXA_API_KEY="替换为你的 Exa API Key"
```

PowerShell 示例：

```powershell
$env:EXA_API_KEY = "替换为你的 Exa API Key"
```

非空环境变量优先于配置文件中的 `api_key`。其他服务商的变量见[支持的服务商](#支持的服务商)。

#### 3. 安装 Skill

完成 CLI 安装和密钥配置后，在目标项目中通过 [Skills CLI](https://github.com/vercel-labs/skills) 安装 [`linkweft` Skill](skills/linkweft/SKILL.md)：

```bash
npx skills add dengshuit/linkweft --skill linkweft
```

按提示完成安装，让 AI Agent 重新加载 Skills，并确保其命令环境能访问 `linkweft` 和服务商密钥。

#### 4. 验证安装

搜索网页并获取一个已知页面的正文：

```bash
linkweft search --output text -- "Go HTTP 客户端超时设置"
linkweft fetch --output text -- "https://go.dev/doc/"
```

省略 `--output text` 时返回 JSON，适合脚本和 AI Agent 处理。请求通过所选服务商的 API 完成，使用前需在相应服务商处申请密钥，额度与计费以服务商为准。

### 方式二：让 AI Agent 安装

将下面的提示词交给 AI Agent，由它完成 CLI 和 Skill 的安装配置：

```text
请按 https://github.com/dengshuit/linkweft 的 README，为当前 AI Agent 安装配置 Linkweft CLI 和 Skill。复用已有配置和 API Key，缺少密钥时提示我在本地填写，不回显密钥；完成后验证搜索和正文获取。
```

## 支持的服务商

支持以下六家服务商，均可用于搜索和正文获取。至少配置一家服务商的 API Key 即可使用。

| 服务商 | CLI 名称 | 密钥环境变量 |
| --- | --- | --- |
| Tavily | `tavily` | `TAVILY_API_KEY` |
| Exa | `exa` | `EXA_API_KEY` |
| Keenable | `keenable` | `KEENABLE_API_KEY` |
| Firecrawl | `firecrawl` | `FIRECRAWL_API_KEY` |
| AnySearch | `anysearch` | `ANYSEARCH_API_KEY` |
| TinyFish | `tinyfish` | `TINYFISH_API_KEY` |

默认回退优先级如下，从左到右依次尝试：

| 命令 | 服务商默认优先级 |
| --- | --- |
| `search`（`fallback`） | Exa → Keenable → TinyFish → AnySearch → Firecrawl → Tavily |
| `fetch` | TinyFish → Keenable → Exa → Tavily → AnySearch → Firecrawl |

未配置密钥的服务商不参与。可通过配置中的 `priority` 调整顺序，或用 `--providers` 指定本次使用的服务商及顺序。

## 常用命令

### 控制搜索范围与策略

```bash
# 最多返回 5 条结果
linkweft search --limit 5 -- "Go HTTP timeout"

# 仅使用 Exa；需要已配置 Exa 的密钥
linkweft search --providers exa -- "Go HTTP timeout"

# 按指定顺序回退；需要同时配置 Tavily 和 Exa 的密钥
linkweft search --providers tavily,exa -- "Go HTTP timeout"

# 并发查询并融合两家结果
linkweft search --strategy merge --providers tavily,exa --limit 10 -- "Go HTTP timeout"
```

默认策略为 `fallback`：某家服务商出现错误、超时或没有有效结果时，在剩余总时限内继续尝试下一家；第一家返回至少一个有效结果 URL 后停止。它不会为了凑满 `--limit` 而继续查询。

`merge` 会查询候选服务商并融合结果。如果部分服务商失败，但仍获得可用结果，整体返回 `partial`，退出码为 `0`。

### 控制正文长度与超时

```bash
# 最多返回 12000 个 Unicode 字符的正文
linkweft fetch --max-chars 12000 -- "https://go.dev/doc/"

# 设置本次检索的总时限
linkweft fetch --timeout 30s --output text -- "https://go.dev/doc/"
```

`fetch` 每次接受一个完整的 HTTP(S) URL，按候选顺序获取正文，成功后停止。`--max-chars` 只限制正文 `document.content.text`，不限制整个 JSON 大小或 token 数，也不适用于 `search`。

### 常用参数

| 参数 | 适用命令 | 作用与默认值 |
| --- | --- | --- |
| `--providers` | `search`、`fetch` | 逗号分隔的服务商名称；省略时按配置选择已提供密钥的服务商 |
| `--output` | `search`、`fetch` | `json`（默认）或 `text` |
| `--config` | `init`、`search`、`fetch` | 指定 TOML 配置文件路径 |
| `--timeout` | `search`、`fetch` | 总时限，如 `30s`、`2m`；搜索默认 `45s`，正文获取默认 `90s` |
| `--strategy` | `search` | `fallback`（默认）或 `merge` |
| `--limit` | `search` | 结果数量上限，默认 `10`；服务商自身限制可能使返回数量更少 |
| `--max-chars` | `fetch` | 正文 Unicode 字符上限，默认 `50000` |

显式指定 `--providers` 时，只使用列出的服务商，且每家都必须配置密钥。更多帮助可运行 `linkweft search --help` 或 `linkweft fetch --help`。

## 配置

默认配置文件的位置遵循操作系统的用户配置目录：

| 系统 | 默认路径 |
| --- | --- |
| Linux | `$XDG_CONFIG_HOME/linkweft/config.toml`，未设置时为 `~/.config/linkweft/config.toml` |
| macOS | `~/Library/Application Support/linkweft/config.toml` |
| Windows | `%AppData%\linkweft\config.toml` |

也可以创建和使用另一份配置：

```bash
linkweft init --config ./linkweft.toml
linkweft search --config ./linkweft.toml -- "Go HTTP timeout"
```

CLI 参数覆盖配置文件中的对应选项。以下示例展示如何调整服务商顺序、结果数量和超时；在已有文件的对应配置表中修改即可：

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
api_key = "替换为你的 Exa API Key"
search_timeout = "15s"
fetch_timeout = "45s"
search_snippets = true

[providers.tavily]
api_key = "替换为你的 Tavily API Key"
```

- `priority` 调整顺序，未列出但已配置密钥的服务商仍会补到末尾。需要限定本次使用哪些服务商时，使用 `--providers`。
- `max_concurrency` 控制 `merge` 搜索的最大并发数，默认 `3`。
- 每家服务商的 `search_timeout` 默认 `15s`，`fetch_timeout` 默认 `45s`。一次请求同时受服务商超时和剩余总时限约束，以较短者为准。
- Exa 默认请求搜索摘要，可通过 `search_snippets = false` 关闭。
- 使用兼容 API 网关或代理地址时，可在对应服务商下设置 `base_url`，或分别设置 `search_base_url`、`fetch_base_url`。填写基础地址，CLI 会追加接口路径；各操作的地址优先于共用的 `base_url`。

## 输出与排错

`search` 和 `fetch` 默认将 JSON 写入 stdout，警告写入 stderr。JSON 中可重点查看：

| 字段 | 用途 |
| --- | --- |
| `outcome` | 本次命令的整体结果 |
| `results` | 搜索结果及其 `sources` 来源信息 |
| `document.content.text` | 获取到的页面正文 |
| `executions` | 各服务商的状态、耗时、跳过原因和错误 |
| `error`、`warnings` | 整体错误与警告 |

| `outcome` | 含义 | 退出码 |
| --- | --- | --- |
| `success` | 获得搜索结果或正文 | `0` |
| `empty` | 搜索完成，但没有结果 | `0` |
| `partial` | 融合搜索获得结果，但部分服务商未完成或失败 | `0` |
| `error` | 检索失败，或参数、配置有误 | 检索失败为 `1`，参数、配置或凭据问题为 `2` |
| `cancelled` | 用户中断检索 | `130` |

脚本应同时检查退出码和 `outcome`。命令以非零状态退出时也可能输出 JSON，可分开保存结果与诊断信息；安装了 `jq` 后，可按字段读取：

```bash
linkweft search -- "Go HTTP timeout" > result.json 2> diagnostics.log
jq '.outcome, .error, .executions' result.json
jq -r '.results[].url' result.json
```

常见问题：

- **`no_provider_available`**：尚未配置可用服务商，检查配置文件路径和密钥。
- **`missing_credentials`**：`--providers` 显式指定的某家服务商缺少密钥，补齐配置或调整列表。
- **`invalid_config`**：检查 TOML 语法、配置项拼写和取值。显式指定的配置文件必须存在；配置项名称区分大小写，使用小写。
- **`search_failed` / `fetch_failed`**：查看 `executions[].error`，确认具体服务商的鉴权、限流、上游错误或超时原因。
- **正文不完整**：检查 `document.content.local_truncated`，为 `true` 时可增大 `--max-chars`；`upstream_truncated` 为 `true` 表示上游已截断，为 `null` 表示未知。

页面可能返回登录提示、验证页面或错误正文。即使 `outcome` 为 `success`，也应确认内容符合预期；需要引用网页时，优先使用 `document.final_url`，缺失时使用 `document.requested_url`。

## 许可证

本项目采用 [MIT License](LICENSE)。
