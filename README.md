# OpenCode Go CLIProxyAPI Plugin

A native dynamic Go plugin for [CLIProxyAPI](https://help.router-for.me/plugin/development) that exposes OpenCode Go as a single provider (`opencode-go`).

The plugin unifies model discovery, protocol translation, and execution across OpenCode Go's upstream endpoints while leveraging CLIProxyAPI's built-in authentication, scheduling, keys rotation, and cooldown management.

## The Problem

OpenCode Go exposes models across multiple API protocols (OpenAI Chat Completions `/v1/chat/completions`, Anthropic Messages `/v1/messages`, and OpenAI Responses `/v1/responses`).

Without this plugin, using OpenCode Go in CLIProxyAPI requires configuring separate provider blocks for each protocol family. This leads to:
- **Duplicated configuration & keys**: The same API keys must be configured across multiple provider blocks.
- **Fragmented scheduling & rotation**: Keys rotation, rate limits, and cooldowns cannot be shared across protocols—exhausting quota on one protocol does not coordinate with another.
- **Client protocol burden**: Clients must know beforehand which upstream protocol and endpoint each model requires.
- **Fragmented catalog**: Models are split across disjoint provider namespaces instead of a unified model list.

## The Solution

This plugin exposes OpenCode Go as a single provider (`opencode-go`) backed by a shared keys pool:
- **Unified auth pool**: Configure keys once; CLIProxyAPI schedules, rotates, and cools down keys across all protocols.
- **Transparent protocol translation & routing**: Clients request models (e.g. `opencode-go/glm-5.2`, `opencode-go/gpt-5.6-luna`) without needing to know the upstream protocol format.
- **Single model catalog**: All models are discovered and published under the `opencode-go` provider namespace in `/v1/models`.

## Features

- **Single Provider Namespace**: Exposes models under the `opencode-go` provider prefix (e.g. `opencode-go/glm-5.2`, `opencode-go/qwen3.7-max`, `opencode-go/gpt-5.6-luna`).
- **Multi-Protocol Translation**: Translates requests and streaming responses between client formats and upstream endpoints:
  - OpenAI Chat Completions (`/v1/chat/completions`)
  - Anthropic Messages (`/v1/messages`)
  - OpenAI Responses (`/v1/responses`)
- **Thinking & Reasoning Support**: Maps reasoning effort across supported client and upstream formats.
- **Dynamic Catalog Discovery**: Fetches remote model catalogs with local fallback and custom route overrides.
- **Multi-Key Auth Scheduling**: Pools multiple API keys with CLIProxyAPI's native scheduler for rotation, retries, and error cooldowns across all protocols.
- **OpenCode Go Quota Page**: Management Center includes a separate `OpenCode Go Quota` page. Page load lists credentials without contacting OpenCode; each card is refreshed manually and independently, and quota values do not affect routing or CPA's native quota page.

## Requirements

- **CLIProxyAPI**: `v7.2.138+`
- **Go Toolchain**: Go 1.24+ (with CGO enabled for C-shared build mode)

## Build

Build the dynamic shared library for your platform:

### Windows (AMD64)
```powershell
go build -buildmode=c-shared -o plugins/windows/amd64/opencode-go-cliproxyapi.dll .
```

### Linux (AMD64)
```bash
go build -buildmode=c-shared -o plugins/linux/amd64/opencode-go-cliproxyapi.so .
```

### macOS (ARM64)
```bash
go build -buildmode=c-shared -o plugins/darwin/arm64/opencode-go-cliproxyapi.dylib .
```

Place the compiled binary into your CLIProxyAPI plugin directory (e.g. `<cliproxyapi_root>/plugins/<os>/<arch>/`).

## Configuration

Configure the plugin in your CLIProxyAPI `config.yaml` under `plugins.configs.opencode-go-cliproxyapi`:

```yaml
plugins:
  configs:
    opencode-go-cliproxyapi:
      # Upstream base URL (default: "https://opencode.ai/zen/go/v1")
      base-url: "https://opencode.ai/zen/go/v1"

      # Optional catalog endpoint override (default: "{base-url}/models")
      # catalog-url: "https://opencode.ai/zen/go/v1/models"

      # Client-facing model ID prefix configuration
      model-prefix:
        enabled: true           # true -> "opencode-go/<model>", false -> bare "<model>" (default: true)
        value: "opencode-go"    # prefix name (default: "opencode-go")

      # OpenCode Go API keys (at least one required). Supports ${ENV_VAR} expansion.
      api-keys:
        - value: "sk-opencode-key-1"
        - value: "sk-opencode-key-2"
        - value: "${OPENCODE_GO_API_KEY}"

      # Catalog discovery settings
      catalog:
        refresh-interval: "15m"          # discovery refresh cadence, min "1m" (default: "15m")
        stale-while-unavailable: true    # retain last good catalog snapshot on refresh failure (default: true)

      # Protocol enable/disable switches (all default to true)
      protocols:
        chat-completions: true   # enables models routed to /v1/chat/completions
        messages: true           # enables models routed to /v1/messages
        responses: true          # enables models routed to /v1/responses

      # Explicit route overrides per model (takes priority over built-in prefix routing)
      route-overrides:
        "custom-model":
          protocol: "messages"           # "chat-completions" | "messages" | "responses"
          endpoint: "/v1/messages"       # must start with /

      # Optional per-model capability fixes (takes priority over catalog and models.dev)
      model-metadata-overrides:
        "gpt-5.6-luna":
          thinking:
            zero-allowed: true
            levels: [none, low, medium, high, xhigh, max]

      # Execution settings
      request-timeout: "5m"              # upstream request timeout (default: "5m")
      max-response-bytes: 67108864       # max non-streaming response body size in bytes (default: 64 MiB)
      allow-http: false                  # allow http:// scheme for local mock/testing (default: false)
```

### Configuration Options

The configured `catalog-url` alone determines which models are routable. On each successful discovery, the plugin fills missing limits, modalities, and unambiguous reasoning metadata from the `opencode-go` provider in [models.dev](https://models.dev/); if that request fails, the last good metadata is retained. Field priority is: `model-metadata-overrides` > catalog > models.dev > plugin defaults. `toggle` reasoning options are not inferred as effort levels.

| Option | Type | Default | Description |
|---|---|---|---|
| `api-keys` | `[]object` | *(Required)* | List of API keys (`- value: "..."`). Supports `${ENV_VAR}` expansion. Duplicates and empty values are rejected. |
| `base-url` | `string` | `https://opencode.ai/zen/go/v1` | Upstream base URL. Must be valid HTTPS (or HTTP if `allow-http: true`) without query parameters, fragments, or userinfo. |
| `catalog-url` | `string` | `{base-url}/models` | Full URL for catalog discovery. Defaults to `{base-url}/models`. |
| `model-prefix.enabled` | `bool` | `true` | When `true`, client-facing model names use `<prefix>/<model>`. When `false`, uses bare model IDs. |
| `model-prefix.value` | `string` | `opencode-go` | Provider prefix string when prefixing is enabled. |
| `catalog.refresh-interval` | `duration` | `15m` | Interval between catalog polling refreshes (e.g. `15m`, `1h`). Minimum is `1m`. |
| `catalog.stale-while-unavailable` | `bool` | `true` | When `true`, serves the last valid catalog snapshot if an update fails. |
| `protocols.chat-completions` | `bool` | `true` | Protocol switch for Chat Completions endpoints. |
| `protocols.messages` | `bool` | `true` | Protocol switch for Messages endpoints. |
| `protocols.responses` | `bool` | `true` | Protocol switch for Responses endpoints. |
| `route-overrides` | `map` | `{}` | Map of model ID to `{ protocol: "...", endpoint: "..." }` overriding built-in family routing. Valid protocols: `chat-completions`, `messages`, `responses`. |
| `model-metadata-overrides` | `map` | `{}` | Per-model `thinking` (`min`, `max`, `zero-allowed`, `dynamic-allowed`, `levels`), `context-limit`, `output-limit`, `input-modes`, and `output-modes`. Each supplied field overrides the catalog and models.dev; unspecified fields retain their current value. |
| `request-timeout` | `duration` | `5m` | Upstream HTTP request timeout. Must be positive. |
| `max-response-bytes` | `int64` | `67108864` (64 MiB) | Maximum non-streaming response body size in bytes. |
| `allow-http` | `bool` | `false` | When `true`, permits `http://` scheme in `base-url` / `catalog-url` for local testing. |

## Testing

```powershell
# Run all tests
go test ./...

# Run tests with coverage
go test ./... -cover

# Run linter / vetting
go vet ./...
```
