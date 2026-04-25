# Moon Bridge

Moon Bridge 是一个 OpenAI Responses 兼容转发层，对外提供 `/v1/responses`，对内调用 Anthropic Messages 兼容 Provider API。

## 配置

复制示例配置，并填入真实 Provider 信息：

```bash
cp config.example.yml config.yml
```

`config.yml` 包含 Provider API Key，已被 `.gitignore` 忽略，不要提交。
`provider.models` 是 Transform 模式的模型映射表，也是 Codex 启动脚本生成临时模型上下文配置的来源。建议保留 `provider.default_model` 指向的模型别名，Codex 脚本和 E2E 会优先使用它。
所有模式都使用 `server.addr` 监听，默认端口为 `38440`。

需要排查转发细节时，可以打开 trace：

```yaml
trace_requests: true
```

启用后，当前 `mode` 的请求会写入 `trace/{session_id}/{request_number}.json`。除 API Key 会脱敏外，其余内容保留，方便 debug。`trace/` 已被 `.gitignore` 忽略。

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

Moon Bridge 兼容 Codex CLI 使用的 OpenAI Responses 请求形态，包括 `/responses` 路径、`local_shell` 工具、函数工具、工具结果回传和常见 Codex 元数据字段。

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

当 `config.yml` 的 `mode` 为 `Transform` 时，使用统一脚本启动 Moon Bridge 和 Codex。脚本会读取 `server.addr`、`provider.default_model`、`provider.models.<alias>.context_window` / `max_output_tokens`，生成临时 `./FakeHome/Codex/config.toml`，并用临时 `CODEX_HOME=./FakeHome/Codex` 运行 Codex，不修改全局 `~/.codex` 配置。Codex 退出时脚本会清理 Moon Bridge 进程。

启动交互式 Codex TUI：

```bash
./scripts/start_codex_with_moonbridge.sh
```

也可以带一个初始任务进入 TUI：

```bash
./scripts/start_codex_with_moonbridge.sh '请运行 CGO_ENABLED=0 GOCACHE="$(pwd)/.cache/go-build" go test ./... 并汇报结果'
```

## 启动 Claude Code

当 `config.yml` 的 `mode` 为 `CaptureAnthropic` 时，使用统一脚本启动 Anthropic Messages 透明代理和 Claude Code。脚本会读取同一个 `server.addr`、`developer.proxy.anthropic.model` 和上游 Provider 配置，设置临时 `CLAUDE_CONFIG_DIR=./FakeHome/ClaudeCode`、`ANTHROPIC_BASE_URL`、`ANTHROPIC_API_KEY`，不修改全局 Claude 配置。

```yaml
developer:
  proxy:
    anthropic:
      model: "provider-model-name"
      provider:
        base_url: "https://provider.example.com"
        api_key: "real-upstream-provider-api-key"
        version: "2023-06-01"
```

```bash
./scripts/start_claude_code_with_moonbridge.sh
```

也可以带一个初始任务进入 Claude Code：

```bash
./scripts/start_claude_code_with_moonbridge.sh '请读取 README 并总结项目用途'
```

Moon Bridge 默认使用 `server.addr: 127.0.0.1:38440`；`mode` 决定运行 Transform、CaptureAnthropic 还是 CaptureResponse，不再为透明代理分配单独端口。日志会分别写入 `logs/moonbridge-codex.log` 或 `logs/moonbridge-claude-code.log`。`FakeHome/`、`logs/` 和 `trace/` 均已被 `.gitignore` 忽略。
