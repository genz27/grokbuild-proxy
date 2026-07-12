# grokbuild-proxy

[简体中文](README.md) | **English**

[![CI](https://github.com/GreyGunG/grokbuild-proxy/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GreyGunG/grokbuild-proxy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GreyGunG/grokbuild-proxy)](https://github.com/GreyGunG/grokbuild-proxy/releases)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26.5-00ADD8?logo=go)](go.mod)

`grokbuild-proxy` is a local, self-hosted compatibility proxy for using an
operator-owned Grok Build account with Anthropic Messages and OpenAI-compatible
clients.

It translates Claude Code requests to the Grok Build Responses backend,
including SSE, client tools, structured output, reasoning effort, CPA-style
thinking blocks, encrypted reasoning replay, and Grok-hosted web search.

> [!CAUTION]
> This is an unofficial project for technical learning and interoperability
> research. It is not affiliated with xAI, Grok, Anthropic, or OpenAI. Use only
> accounts you own and are authorized to automate. Usage may violate an
> upstream provider's terms or result in account restrictions. You assume all
> risk. Read the full [Disclaimer](DISCLAIMER.md) before use.

## Features

- Anthropic-compatible `POST /v1/messages`
- OpenAI-compatible Responses, Chat Completions, and Models endpoints
- Incremental SSE translation
- Client function tools and parallel calls
- Anthropic hosted web search mapped to Grok `web_search`
- JSON Schema output mapped to Responses `text.format`
- Adaptive/manual reasoning-effort compatibility
- Summarized/omitted thinking and encrypted reasoning replay
- Multi-account selection, sticky sessions, cooldown, and failover
- Grok CLI import and browser OAuth device login
- Atomic local JSON storage with locking and backup recovery
- Embedded Admin Web UI
- Health, readiness, Prometheus metrics, request IDs, and structured logs
- Multi-platform archives, checksums, SBOMs, and GHCR images

## One-command installation

Linux / macOS:

```bash
curl -fsSL \
  https://raw.githubusercontent.com/GreyGunG/grokbuild-proxy/main/scripts/install.sh \
  | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/GreyGunG/grokbuild-proxy/main/scripts/install.ps1 | iex
```

The installers detect the OS/architecture, download the latest Release, verify
SHA-256, install the binary, and create a local configuration. Set
`GROKBUILD_VERSION=v0.1.0` to pin a release.

## Run from source

Requirements: Go 1.26.5 or newer, or Docker.

```bash
git clone https://github.com/GreyGunG/grokbuild-proxy.git
cd grokbuild-proxy
cp config.example.yaml config.yaml
go run ./cmd/grokbuild-proxy
```

The proxy listens on `127.0.0.1:8080`. Empty bootstrap keys are generated in
`data/meta.json`.

```bash
jq -r .api_key data/meta.json
jq -r .admin_key data/meta.json
```

Admin UI:

```text
http://127.0.0.1:8080/admin
```

Complete browser device login in the Admin UI. Prefer this over importing a
potentially stale `~/.grok/auth.json` refresh-token snapshot.

## Claude Code

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN="$(jq -r .api_key data/meta.json)"
export ANTHROPIC_MODEL=grok-4.5

claude --effort high
```

Configured Claude aliases can also be mapped to Grok models via `anthropic.model_aliases`.

### Claude Code 1M context

Claude Code defaults model context to **200k**. Only model ids ending in `[1m]` get the **1M** context budget and auto-compact behavior (this is a Claude Code client convention, not a real upstream model name).

This proxy:

1. Exposes a sibling `claude-...[1m]` entry on `GET /v1/models` (`context_window=1000000`) for every Claude alias
2. Strips `[1m]` in `ResolveModel` before alias mapping, so upstream still receives a normal Grok model id

Long-session example:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN="$(jq -r .api_key data/meta.json)"
export ANTHROPIC_MODEL='claude-opus-4-6[1m]'

claude --effort high
```

You can also pick the `(1M context)` entry in Claude Code's model selector.

> Note: `[1m]` only changes Claude Code's local context budget. The real upstream window is still limited by the Grok model and proxy `anthropic.soft_input_tokens` / `max_input_tokens`. Keep `anthropic.auto_compact: true` for long sessions.

## OpenAI-compatible clients

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8080/v1
export OPENAI_API_KEY="$(jq -r .api_key data/meta.json)"
```

```bash
curl --fail --silent --show-error \
  http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer ${OPENAI_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"grok-4.5","input":"Reply with exactly: ok"}'
```

## Docker Compose

```bash
cp config.example.yaml config.yaml
docker compose up --build -d
docker compose exec grokbuild-proxy sh -c 'cat /app/data/meta.json'
```

Published image:

```text
ghcr.io/greygung/grokbuild-proxy
```

## Documentation

- [Build and run guide](docs/build-and-run.md)
- [Design](DESIGN.md)
- [Compatibility matrix](COMPATIBILITY.md)
- [Operations](docs/operations.md)
- [Security policy](SECURITY.md)
- [Disclaimer](DISCLAIMER.md)
- [Contributing](CONTRIBUTING.md)

## Known limitations

- Anthropic `count_tokens` is not implemented.
- Thinking signatures are proxy-scoped and account/model-bound.
- Some Anthropic reasoning controls are approximated.
- `top_k` and `stop_sequences` are not forwarded to Grok reasoning models.
- Rich Anthropic hosted-tool result/citation blocks are reduced.
- Only hosted web search has a dedicated Anthropic server-tool mapping.
- OAuth refresh is request-driven, not scheduled in the background.
- The upstream CLI protocol is unstable and may change.
- This is a trusted-operator tool, not a multi-tenant service.

See [COMPATIBILITY.md](COMPATIBILITY.md) for details.

## Build and test

```bash
make build
make check
make release-snapshot

# Or invoke Go directly
gofmt -w ./cmd ./internal
go vet ./...
go test ./...
go test -race ./...
go build ./cmd/grokbuild-proxy
```

See [docs/build-and-run.md](docs/build-and-run.md) for cross-compilation,
containers, GoReleaser, live probes, and troubleshooting.

## Community

Friendly link: [LINUX DO](https://linux.do)

## Acknowledgements

This project studied or was inspired by:

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) — protocol
  translators, executor patterns, and CPA-style thinking compatibility
- [open-grok-build](https://github.com/kenryu42/open-grok-build) — Grok CLI
  OAuth, request normalization, models, and billing observations
- [pi-grok-cli](https://github.com/kenryu42/pi-grok-cli) — Grok CLI endpoint,
  headers, authentication, and model behavior
- [kiro.rs](https://github.com/hank9999/kiro.rs) — credential-pool and compact
  self-hosted admin patterns
- [Sub2API](https://github.com/Wei-Shaw/sub2api) — multi-account operations and
  admin workflow references

These projects are independent and do not endorse or support this repository.

## License

[MIT](LICENSE). The license covers repository code only and grants no right to
third-party accounts, subscriptions, APIs, quota, trademarks, or services.
