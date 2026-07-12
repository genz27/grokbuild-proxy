# grokbuild-proxy

**简体中文** | [English](README_EN.md)

[![CI](https://github.com/GreyGunG/grokbuild-proxy/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/GreyGunG/grokbuild-proxy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/GreyGunG/grokbuild-proxy)](https://github.com/GreyGunG/grokbuild-proxy/releases)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26.5-00ADD8?logo=go)](go.mod)

`grokbuild-proxy` 是一个本地、自托管的协议兼容代理，用于将使用者本人
合法持有的 Grok Build 账号接入 Claude Code、Anthropic SDK 和
OpenAI 兼容客户端。

项目将 Anthropic Messages 请求转换到 Grok Build Responses 后端，支持
流式输出、客户端工具、结构化输出、思考强度、CPA 风格 Thinking Block、
加密推理回放和 Grok 内置 Web Search。

> [!CAUTION]
> 本项目是非官方、社区维护的技术学习与协议互操作研究项目，与 xAI、
> Grok、Anthropic、OpenAI 及其关联公司无关，也未获得其授权、赞助或
> 认可。只能使用你本人合法持有并获准自动化操作的账号。使用本项目可能
> 违反相关服务条款或导致账号限制，全部风险由使用者自行承担。使用前请
> 阅读完整的[免责声明](DISCLAIMER.md)。

## 功能

- Anthropic 兼容的 `POST /v1/messages`
- OpenAI 兼容接口：
  - `POST /v1/responses`
  - `POST /v1/chat/completions`
  - `GET /v1/models`
- 增量 SSE 转换，不缓冲完整响应
- 客户端函数工具和并行工具调用
- Anthropic `web_search_*` 映射到 Grok 内置 Web Search
- Anthropic JSON Schema 映射到 Responses `text.format`
- Adaptive / Manual Thinking 与思考强度兼容
- Summarized / Omitted Thinking Block
- 工具轮次间的加密 Reasoning 回放
- 多账号选择、会话粘滞、冷却和故障切换
- Grok CLI 凭据导入和浏览器 OAuth Device Login
- 带文件锁、原子写入和备份恢复的本地 JSON 存储
- 内嵌 Admin Web UI
- 健康检查、Readiness、Prometheus 指标、Request ID 和结构化日志
- 多平台归档、校验和、SBOM 与 GHCR 容器镜像

## 架构

```text
Claude Code / Anthropic SDK       OpenAI SDK / 兼容客户端
              |                              |
              +-------------+----------------+
                            |
                    grokbuild-proxy
                /v1/messages | /v1/responses
                            |
              凭据池 / OAuth 刷新 / 重试切换
                            |
                cli-chat-proxy.grok.com/v1
```

组件边界和协议决策见 [DESIGN.md](DESIGN.md)。

## 环境要求

- Go 1.26.5 或更高版本，或者 Docker
- 使用者本人合法持有的 Grok CLI / Grok Build 账号
- 可信的本机或私有网络环境

## 一键安装

Linux / macOS：

```bash
curl -fsSL \
  https://raw.githubusercontent.com/GreyGunG/grokbuild-proxy/main/scripts/install.sh \
  | sh
```

Windows PowerShell：

```powershell
irm https://raw.githubusercontent.com/GreyGunG/grokbuild-proxy/main/scripts/install.ps1 | iex
```

安装脚本会自动识别系统与架构、下载最新 Release、验证 SHA-256、安装
二进制并生成本地配置。可以通过 `GROKBUILD_VERSION=v0.1.0` 固定版本。

## 源码运行

```bash
git clone https://github.com/GreyGunG/grokbuild-proxy.git
cd grokbuild-proxy

cp config.example.yaml config.yaml
go run ./cmd/grokbuild-proxy
```

默认监听 `127.0.0.1:8080`。

`api_key` 和 `admin_key` 留空时，首次启动会将随机密钥写入
`data/meta.json`。该文件包含敏感信息，禁止提交或分享。

```bash
jq -r .api_key data/meta.json
jq -r .admin_key data/meta.json
```

打开 Admin UI：

```text
http://127.0.0.1:8080/admin
```

在 Admin UI 中完成浏览器登录、导入 Grok CLI 凭据、管理账号池，以及
创建或撤销客户端密钥。

建议优先使用代理自己的浏览器 Device Login。直接导入
`~/.grok/auth.json` 可能复制一份已经旋转或撤销的 Refresh Token。

## Claude Code

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN="$(jq -r .api_key data/meta.json)"
export ANTHROPIC_MODEL=grok-4.5

claude --effort high
```

也可以使用配置过的 Claude 模型别名：

```bash
export ANTHROPIC_MODEL=claude-sonnet-5
```

### Claude Code 1M 上下文

Claude Code 对模型 id 的默认上下文是 **200k**。只有 id 以 `[1m]` 结尾时，客户端才会按 **1M** 上下文做预算与自动压缩（这是 Claude Code 本地约定，不是上游真实模型名）。

代理会：

1. 在 `GET /v1/models` 为每个 Claude 别名额外暴露 `claude-...[1m]`（`context_window=1000000`）
2. 在 `ResolveModel` 里去掉 `[1m]` 后再做别名映射，因此上游仍收到普通 Grok 模型名

推荐长会话用法：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN="$(jq -r .api_key data/meta.json)"
export ANTHROPIC_MODEL='claude-opus-4-6[1m]'

claude --effort high
```

也可在 Claude Code 的模型选择器里选带 `(1M context)` 的条目。

> 注意：`[1m]` 只改变 Claude Code 侧的上下文预算；实际上游窗口仍受 Grok 模型与代理 `anthropic.soft_input_tokens` / `max_input_tokens` 限制。长会话请保持 `anthropic.auto_compact: true`。

通过 `anthropic.model_aliases` 可以把 Claude 模型映射到指定 Grok 模型。

## OpenAI 兼容客户端

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8080/v1
export OPENAI_API_KEY="$(jq -r .api_key data/meta.json)"
```

```bash
curl --fail --silent --show-error \
  http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer ${OPENAI_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5",
    "input": "Reply with exactly: ok",
    "max_output_tokens": 16
  }'
```

## Docker Compose

```bash
cp config.example.yaml config.yaml
docker compose up --build -d
docker compose exec grokbuild-proxy sh -c 'cat /app/data/meta.json'
```

Compose 只在宿主机发布 `127.0.0.1:8080`，运行状态保存在命名卷中。

预构建镜像：

```text
ghcr.io/greygung/grokbuild-proxy
```

## 配置

以 [config.example.yaml](config.example.yaml) 为起点。

| 配置项 | 用途 |
|---|---|
| `listen` | HTTP 监听地址，默认仅 Loopback |
| `allow_public_listen` | 非 Loopback 监听必须显式开启 |
| `data_dir` | 凭据、客户端密钥和启动密钥目录 |
| `api_key` | 客户端 API 鉴权；留空自动生成 |
| `admin_key` | Admin API/UI 鉴权；留空自动生成 |
| `upstream.*` | Grok CLI 上游地址与客户端请求头 |
| `oauth.*` | xAI OAuth Issuer、Client、Scope 和回调 |
| `anthropic.model_aliases` | Claude 模型到 Grok 模型的映射 |
| `lb.*` | 凭据选择、会话粘滞、刷新和冷却策略 |
| `limits.*` | Body、超时和并发限制 |
| `logging.level` | `debug`、`info`、`warn` 或 `error` |

未知 YAML 字段会导致启动失败。

## 探针与指标

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
curl http://127.0.0.1:8080/metrics
```

- `/healthz`：进程存活
- `/readyz`：存储可用且至少存在一个可用凭据
- `/metrics`：Prometheus 文本格式指标

## 兼容性与已知限制

本项目只实现文档中列出的 Anthropic / OpenAI 兼容子集。

- Anthropic `count_tokens` 尚未实现，返回 404。
- Thinking Signature 仅限本代理和原模型/账号路径回放。
- 部分 Anthropic 推理控制只能近似映射。
- `top_k` 和 `stop_sequences` 不会转发给 Grok Reasoning 模型。
- Anthropic Server Tool 的富结果与 Citation UI 尚未完整复刻。
- 目前只有 Server Web Search 做了专门映射。
- OAuth 刷新由请求触发，尚无后台预刷新调度器。
- 上游 CLI 协议并不稳定，可能随时变化。
- 项目面向可信单一操作者，不是多租户 SaaS。

完整矩阵见 [COMPATIBILITY.md](COMPATIBILITY.md)。

## 文档

- [构建与运行指南](docs/build-and-run.md)
- [设计文档](DESIGN.md)
- [兼容性矩阵](COMPATIBILITY.md)
- [运维指南](docs/operations.md)
- [安全策略](SECURITY.md)
- [免责声明](DISCLAIMER.md)
- [贡献指南](CONTRIBUTING.md)

## 构建与测试

```bash
make build
make check
make release-snapshot

# 或直接使用 Go
gofmt -w ./cmd ./internal
go vet ./...
go test ./...
go test -race ./...
go build ./cmd/grokbuild-proxy
```

跨平台编译、Docker、GoReleaser、Live Probe 和故障排查见
[构建与运行指南](docs/build-and-run.md)。

## 社区

友情链接：[LINUX DO](https://linux.do)

## 参考与致谢

项目在设计和协议研究过程中参考了以下开源项目：

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)：协议转换器、
  Executor 设计和 CPA 风格 Thinking 兼容
- [open-grok-build](https://github.com/kenryu42/open-grok-build)：Grok CLI
  OAuth、请求规范化、模型和 Billing 行为
- [pi-grok-cli](https://github.com/kenryu42/pi-grok-cli)：Grok CLI 端点、
  请求头、鉴权和模型行为
- [kiro.rs](https://github.com/hank9999/kiro.rs)：凭据池和紧凑型自托管
  Admin 设计
- [Sub2API](https://github.com/Wei-Shaw/sub2api)：多账号运维和 Admin
  工作流参考

感谢上述项目公开的实现、文档和协议研究。它们均为独立项目，不代表其
作者认可、赞助或支持本仓库。

## 免责声明摘要

- 仅使用你本人合法持有并获准操作的账号和凭据。
- 禁止用于违法活动、未授权访问、账号共享、凭据转售、支付或配额绕过、
  限制规避、恶意自动化及其他滥用。
- 使用者自行负责遵守法律法规和所有相关服务条款。
- 使用本项目可能导致额度消耗、服务中断、账号限制、暂停或封禁。
- 作者和贡献者不对账号、数据、额度、业务、利润或其他直接/间接损失
  承担责任。
- 本项目按“现状”提供，不承诺可用性、稳定性、兼容性或安全性。
- 第三方名称与商标归其各自权利人所有。
- MIT 许可证只覆盖仓库代码，不授予第三方服务、账号、API、额度或商标
  权利。

使用前请阅读[完整免责声明](DISCLAIMER.md)。

## 贡献

请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。协议行为变更应同时补充测试
并更新 [COMPATIBILITY.md](COMPATIBILITY.md)。

## 许可证

本项目使用 [MIT License](LICENSE)。
