# Moon Bridge

Moon Bridge 是一个 OpenAI Responses 兼容转发层。你可以像调用 OpenAI 一样使用 `/v1/responses`，而 Moon Bridge 在背后把请求转发到 Anthropic Messages 兼容的 Provider API。

## 快速开始

1. 复制示例配置：

```bash
cp config.example.yml config.yml
```

2. 编辑 `config.yml`，填入 Provider 的 `base_url` 和 `api_key`。

3. 启动服务：

```bash
go run ./cmd/moonbridge
```

默认监听 `127.0.0.1:38440`。启动后即可通过 `http://localhost:38440/v1/responses` 调用。

## 三种工作模式

在 `config.yml` 中通过 `mode` 选择工作方式：

### `Transform`（默认）

把 OpenAI Responses 请求翻译成 Anthropic Messages 调用。适合想让 Codex CLI 等 OpenAI 客户端跑在 Anthropic 兼容模型上的场景。

### `CaptureResponse`

透明代理 OpenAI Responses 流量。适合抓包分析 Codex CLI 等客户端发给原生 OpenAI 的请求内容。

### `CaptureAnthropic`

透明代理 Anthropic Messages 流量。适合抓包分析 Claude Code 等客户端发给 Anthropic 兼容 Provider 的请求内容。

## 配置说明

### 模型映射

`provider.models` 定义模型别名。例如客户端请求 `model: "moonbridge"`，Moon Bridge 会把它映射到 Provider 真实的模型名：

```yaml
provider:
  default_model: "moonbridge"
  models:
    moonbridge:
      name: "claude-sonnet-4-5"
      context_window: 200000
      max_output_tokens: 100000
```

### 模型定价

`provider.models.<alias>.pricing` 是可选的 per-model 价格配置，单位是元（¥）/ 1K tokens。当某个模型配置了价格后，session 结束时会在日志和控制台输出费用统计。

```yaml
provider:
  models:
    moonbridge:
      name: "deepseek-v4-pro"
      pricing:
        input_price: 2        # 无缓存输入 元/M tokens
        output_price: 8       # 模型输出
        cache_write_price: 1  # 缓存写入
        cache_read_price: 0.2  # 缓存读取
```

费用计算方式：`input_tokens × input_price + cache_creation × cache_write_price + cache_read × cache_read_price + output_tokens × output_price`，四项均为独立计费。如果价格配置不全（某项为 0 或未设置），该项不产生费用。


### Prompt 缓存

`cache.mode` 控制 Anthropic prompt caching 策略：

- `off`：不注入缓存标记
- `automatic`：在请求顶层自动注入 `cache_control`
- `explicit`：在工具定义、system 提示、历史消息等稳定块上注入块级缓存断点（默认推荐）
- `hybrid`：同时启用顶层自动缓存和块级断点

`cache.ttl` 支持 `5m`（默认）和 `1h`。

### Web Search 能力

`provider.web_search.support` 控制是否向 Anthropic 上游注入搜索工具：

- `auto`：启动 Transform 时用默认模型发送一次流式轻量探测；只有探测证明可用才注入，否则保守禁用
- `enabled`：跳过探测，始终注入 Anthropic `web_search_20250305`
- `disabled`：不注入搜索工具，Codex 仍可继续使用其他工具
- `injected`：不依赖上游 Provider 是否支持 Anthropic 服务端搜索。桥接器改为向模型注入 `tavily_search` / `firecrawl_fetch` 两个 function-type 工具，并在 Transform 内部通过 Tavily / Firecrawl API 执行搜索。需配置 `tavily_api_key`；`firecrawl_api_key` 可选，不配则不注入 fetch 工具

### DeepSeek V4 扩展

在 `config.yml` 中设置 `provider.deepseek_v4: true` 可启用 DeepSeek V4 专用兼容逻辑，包括 reasoning_content 剥离与重注入、reasoning_effort → thinking 映射、推理输出展示等。详见 [docs/deepseek-v4.md](docs/deepseek-v4.md)。

### 调试抓包

打开 `trace_requests: true` 后，每次请求和响应会按模式写入 `trace/` 目录，方便排查问题。API Key 等敏感信息会自动脱敏。

## 配合 Codex CLI 使用

Moon Bridge 兼容 Codex CLI 的 OpenAI Responses 请求，包括 `local_shell` 工具、函数工具、工具结果回传、`web_search` 等。

### 手动配置

在 `~/.codex/config.toml` 中添加：

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

设置客户端 API Key（本地 Moon Bridge 不校验，任意占位值即可）：

```bash
export MOONBRIDGE_CLIENT_API_KEY="local-dev"
```

然后启动 Moon Bridge，再运行 `codex`。

### 一键启动脚本

当 `config.yml` 的 `mode` 为 `Transform` 或 `CaptureResponse` 时，可以使用脚本自动启动 Moon Bridge 和 Codex：

```bash
./scripts/start_codex_with_moonbridge.sh
```

也可以带一个初始任务进入 TUI：

```bash
./scripts/start_codex_with_moonbridge.sh '请运行测试并汇报结果'
```

脚本会自动生成隔离的 Codex 配置，不会修改你全局的 `~/.codex`；如果全局 `~/.codex/config.toml` 中存在 `[tui].status_line`，会复制到隔离配置里，保持当前 statusline 显示习惯。

## 配合 Claude Code 使用

当 `config.yml` 的 `mode` 为 `CaptureAnthropic` 时，可以使用脚本自动启动 Moon Bridge 和 Claude Code：

```bash
./scripts/start_claude_code_with_moonbridge.sh
```

也可以带一个初始任务：

```bash
./scripts/start_claude_code_with_moonbridge.sh '请读取 README 并总结项目用途'
```

脚本会自动设置隔离的 Claude Code 配置，不会修改全局的 `~/.claude`。

## 直接调用 API

非流式调用：

```bash
curl -sS http://localhost:38440/v1/responses \
  -H 'content-type: application/json' \
  -d '{"model":"moonbridge","input":"Hello"}'
```

流式调用：

```bash
curl -sS http://localhost:38440/v1/responses \
  -H 'content-type: application/json' \
  -d '{"model":"moonbridge","input":"Hello","stream":true}'
```

支持的标准参数包括 `instructions`、`max_output_tokens`、`temperature`、`top_p`、`stop`、`tools`、`tool_choice`、`parallel_tool_calls`、`prompt_cache_key`、`prompt_cache_retention` 等。

## 工具调用支持

Moon Bridge 支持以下工具类型：

- **函数工具**（`type: "function"`）：标准 JSON Schema 参数定义，Anthropic 返回的工具调用会映射为 OpenAI 的 `function_call`。
- **`local_shell`**：Codex CLI 的本地 shell 工具，自动映射为 Anthropic 兼容格式，shell 执行仍由 Codex 客户端完成。
- **`web_search`**：Codex 的搜索工具，按 Provider 能力注入 Anthropic `web_search_20250305`，搜索次数上限由 `provider.web_search.max_uses` 控制。
- **Custom grammar 工具**：Codex 内置需要 freeform grammar 的工具目前主要是 `apply_patch` 和 Code Mode `exec`。Moon Bridge 会把 `apply_patch` 拆成 add/delete/update/replace/batch 一组结构化工具，把 `exec` 暴露成 `source`；Provider 返回后再拼回 Codex 需要的 raw grammar call。
- **命名空间 / MCP 工具**：支持带命名前缀的工具名称。

## 响应与用量

响应格式与 OpenAI Responses API 一致，包含：

- `output[]`：模型输出的消息或工具调用
- `usage.input_tokens` / `usage.output_tokens`：输入和输出 token 数
- `usage.input_tokens_details.cached_tokens`：命中缓存的 token 数
- `status`：`completed` 或 `incomplete`

当启用 prompt caching 时，Anthropic 侧的缓存创建和命中成本会自动归一化到 OpenAI 风格的用量字段中，方便你在客户端统一查看。

如果配置了模型定价，服务终止时会自动汇总当前 session 的总费用和按模型拆解的费用明细。日志输出示例：

```
Session Stats: 42 requests, 12m30s duration
  Input:  154320 tokens (120000 fresh, 20000 cache creation, 14320 cache read)
  Output: 8500 tokens
  Cache Hit Rate: 9.3% (saved 14320 tokens)
  Total Cost: ¥0.3180
    moonbridge: ¥0.3180 (42 req, 154320 in, 8500 out)
```

## 错误处理

常见错误会返回 OpenAI 风格的错误响应：

- 鉴权失败：`401 invalid_api_key`
- 模型不可用：`403 model_not_found`
- 参数不支持：`400 unsupported_parameter`
- 上下文超限：`400 context_length_exceeded`
- 限流：`429 rate_limit_exceeded`
- 上游错误：`502 provider_error`
- 上游超时：`504 provider_timeout`

## 日志

使用脚本启动时，Moon Bridge 的日志分别写入：

- Codex 场景：`logs/moonbridge-codex.log`
- Claude Code 场景：`logs/moonbridge-claude-code.log`

手动启动时，标准输出即为服务日志。

---

更多细节请参考 `config.example.yml` 中的注释和 `docs/` 目录下的设计文档。
