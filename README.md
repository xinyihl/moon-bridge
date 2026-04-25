# Moon Bridge

Moon Bridge 是一个 OpenAI Responses 兼容转发层，对外提供 `/v1/responses`，对内调用 Anthropic Messages 兼容 Provider API。

## 配置

复制示例配置，并填入真实 Provider 信息：

```bash
cp config.example.yml config.yml
```

`config.yml` 包含 Provider API Key，已被 `.gitignore` 忽略，不要提交。
`provider.models` 是 Transform 模式的模型映射表，也是 Codex 启动脚本生成临时模型上下文配置的来源。建议保留 `provider.default_model` 指向的模型别名，Codex 脚本和 E2E 会优先使用它。
`provider.user_agent` 是 Transform 模式发往 Anthropic Messages 上游的可选 `User-Agent`，可填入 `CaptureAnthropic` trace 中抓到的 Claude Code / Provider 客户端 UA。
`provider.web_search.max_uses` 控制 Codex `web_search` 转成 Anthropic server-side `web_search_20250305` 时允许的最大搜索次数。
所有模式都使用 `server.addr` 监听，默认端口为 `38440`。

需要排查转发细节时，可以打开 trace：

```yaml
trace_requests: true
```

启用后，trace 会按模式和协议拆开写入：

- `CaptureResponse`：`trace/Capture/Response/{session_id}/{request_number}.json`
- `CaptureAnthropic`：`trace/Capture/Anthropic/{session_id}/{request_number}.json`
- `Transform` OpenAI Responses 侧：`trace/Transform/{session_id}/Response/{request_number}.json`
- `Transform` Anthropic Messages 侧：`trace/Transform/{session_id}/Anthropic/{request_number}.json`

除 API Key 会脱敏外，其余内容保留，方便 debug。`trace/` 已被 `.gitignore` 忽略。

## 运行

```bash
go run ./cmd/moonbridge
```

指定配置文件或临时覆盖监听地址：

```bash
go run ./cmd/moonbridge --config ./config.yml --addr 127.0.0.1:38440
```

## 调用

```bash
curl -sS http://localhost:38440/v1/responses \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-test","input":"Hello"}'
```

## 接入 Codex CLI

Moon Bridge 兼容 Codex CLI 使用的 OpenAI Responses 请求形态，包括 `/responses` 路径、`local_shell` 工具、函数工具、工具结果回传、`web_search` 内置工具和常见 Codex 元数据字段。Transform 模式会把 Codex `web_search` 声明转成 Anthropic `web_search_20250305` server tool，并把 Anthropic `server_tool_use` 回映射为 Codex 历史里的 `web_search_call`；Codex 抓包里的 `open_page` / `find_in_page` 属于 OpenAI Responses 的 `web_search_call.action` 历史项，Transform 目前不会在本地合成网页抓取，只依赖上游 Anthropic web search 能力。

示例 `~/.codex/config.toml`：

```toml
model = "moonbridge"
model_provider = "moonbridge"

[model_providers.moonbridge]
name = "Moon Bridge"
base_url = "http://localhost:38440/v1"
wire_api = "responses"
env_key = "MOONBRIDGE_CLIENT_API_KEY"

[model_providers.moonbridge.models.moonbridge]
name = "Moon Bridge"
```

本地转发层当前不校验客户端 API key，可随便给一个占位值：

```bash
export MOONBRIDGE_CLIENT_API_KEY="local-dev"
```

再启动 Moon Bridge：

```bash
go run ./cmd/moonbridge
```

## 测试

```bash
CGO_ENABLED=0 GOCACHE="$(pwd)/.cache/go-build" go test ./...
```

## E2E 测试

真实 Provider 配置读取 `config.yml`，该文件已被 `.gitignore` 忽略。测试会优先使用 `provider.models.e2e-model`，没有时使用 `provider.models.moonbridge`。

运行真实 Provider E2E：

```bash
CGO_ENABLED=0 GOCACHE="$(pwd)/.cache/go-build" go test -tags=e2e ./internal/e2e
```

缓存 E2E 会产生额外 token 成本，默认跳过；需要时设置：

```bash
MOONBRIDGE_E2E_CACHE=1 CGO_ENABLED=0 GOCACHE="$(pwd)/.cache/go-build" go test -tags=e2e ./internal/e2e
```

## 启动 Codex

当 `config.yml` 的 `mode` 为 `Transform` 或 `CaptureResponse` 时，使用统一脚本启动 Moon Bridge 和 Codex。`Transform` 用于把 Codex 的 OpenAI Responses 请求转成 Anthropic Messages；`CaptureResponse` 用于透明代理并抓取 Codex 原生 OpenAI Responses 请求。脚本会读取 `server.addr`、Codex 模型和上下文元数据，生成临时 `./FakeHome/Codex/config.toml`，并用临时 `CODEX_HOME=./FakeHome/Codex` 运行 Codex，不修改全局 `~/.codex` 配置。Codex 退出时脚本会清理 Moon Bridge 进程。

模型选择规则：

- `Transform`：使用 `provider.default_model`，并从 `provider.models.<alias>` 读取上下文窗口配置。
- `CaptureResponse`：优先使用 `developer.proxy.response.model`，未配置时回退到 `provider.default_model`。

`CaptureResponse` 抓包时的上游 Provider 和模型示例：

```yaml
mode: "CaptureResponse"

developer:
  proxy:
    response:
      model: "gpt-5.4"
      provider:
        base_url: "https://api.openai.com"
        api_key: "real-upstream-openai-compatible-api-key"
```

启动交互式 Codex TUI：

```bash
./scripts/start_codex_with_moonbridge.sh
```

不要用 `source scripts/start_codex_with_moonbridge.sh`，脚本会拒绝被 source，避免 `CODEX_HOME` 等临时环境变量污染当前终端。

也可以带一个初始任务进入 TUI：

```bash
./scripts/start_codex_with_moonbridge.sh '请运行 CGO_ENABLED=0 GOCACHE="$(pwd)/.cache/go-build" go test ./... 并汇报结果'
```

## 启动 Claude Code

当 `config.yml` 的 `mode` 为 `CaptureAnthropic` 时，使用统一脚本启动 Anthropic Messages 透明代理和 Claude Code。脚本会读取同一个 `server.addr`、`developer.proxy.anthropic.model` 和上游 Provider 配置，设置临时 `CLAUDE_CONFIG_DIR=./FakeHome/ClaudeCode` 和 Claude Code 登录 env，不修改全局 Claude 配置。
脚本会从沙箱外的 `$HOME/.claude/settings.json` 读取非敏感偏好，但不会复制真实 `ANTHROPIC_AUTH_TOKEN` / API Key；它会在 `./FakeHome/ClaudeCode/settings.json` 写入占位 `env.ANTHROPIC_AUTH_TOKEN` 用于跳过登录，同时强制把 `env.ANTHROPIC_BASE_URL` 改成当前 Moon Bridge 地址，默认注入 `env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1"` 和根级 `includeCoAuthoredBy: false`。需要指定其他来源时可设置 `MOONBRIDGE_CLAUDE_SETTINGS=/path/to/settings.json`。

```yaml
developer:
  proxy:
    anthropic:
      model: "" # 可选；留空时使用 Claude Code settings/default model
      provider:
        base_url: "https://provider.example.com"
        api_key: "real-upstream-provider-api-key"
        version: "2023-06-01"
```

```bash
./scripts/start_claude_code_with_moonbridge.sh
```

不要用 `source scripts/start_claude_code_with_moonbridge.sh`，脚本会拒绝被 source，避免 `CLAUDE_CONFIG_DIR` / `ANTHROPIC_BASE_URL` 等临时环境变量污染全局 Claude Code。

也可以带一个初始任务进入 Claude Code：

```bash
./scripts/start_claude_code_with_moonbridge.sh '请读取 README 并总结项目用途'
```

Moon Bridge 默认使用 `server.addr: 127.0.0.1:38440`；`mode` 决定运行 Transform、CaptureAnthropic 还是 CaptureResponse，不再为透明代理分配单独端口。日志会分别写入 `logs/moonbridge-codex.log` 或 `logs/moonbridge-claude-code.log`。`FakeHome/`、`logs/` 和 `trace/` 均已被 `.gitignore` 忽略。
