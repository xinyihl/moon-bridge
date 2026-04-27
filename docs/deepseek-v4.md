# DeepSeek V4 扩展

Moon Bridge 内置 DeepSeek V4 Provider 扩展，处理 DeepSeek 特有的推理内容（thinking/reasoning_content）行为，使得 Codex CLI 等客户端可以通过 Anthropic Messages 兼容接口使用 DeepSeek V4 模型。

## 为什么需要扩展

DeepSeek V4 (deepseek-v4-pro 等) 基于 Anthropic Messages 兼容接口暴露，但存在几个与标准 Anthropic 协议不同的行为：

- **reasoning_content 不能回传**：DeepSeek 在前一轮响应中返回 `reasoning_content`，但若下一轮请求的 input 中包含该字段，上游会返回 400 错误。
- **thinking 块不自动保留**：与原生 Anthropic 不同，DeepSeek 不会在后续轮次中自动保留前一轮的 `thinking` block。客户端需要自行记忆并重新注入。
- **temperature / top_p 被忽略**：DeepSeek 不支持这些参数，传了可能引发某些代理层 Warning，但无实际作用。
- **reasoning_effort → thinking 映射**：OpenAI 客户端使用 `reasoning.effort` 控制推理深度，DeepSeek 使用 `thinking` 配置 + `budget_tokens` 控制。

## 配置启用

在 `config.yml` 的具体 Provider 段中设置：

```yaml
provider:
  providers:
    deepseek:
      base_url: "https://api.deepseek.com"
      api_key: "replace-with-deepseek-api-key"
      deepseek_v4: true
      models:
        deepseek-v4-pro: {}
  routes:
    moonbridge: "deepseek/deepseek-v4-pro"
```

开启后，Moon Bridge 只对路由到该 Provider 的 Transform 请求启用 DeepSeek 兼容逻辑。其他 Provider 的请求不会剥离 reasoning_content、注入 thinking，也不会移除 temperature / top_p。

## Thinking 跨轮回放

DeepSeek 的 thinking 模式要求在有多轮工具调用的对话中，必须把前一轮的 thinking 内容重新传入后续请求。Moon Bridge 通过以下机制实现：

### 响应侧（Anthropic → OpenAI）

当 DeepSeek 返回 `content[].thinking` block 且该次响应包含工具调用时，Moon Bridge 会将 thinking 文本放入一个 `type: "reasoning"` 的 OpenAI Responses output item 中：

```json
{
  "type": "reasoning",
  "summary": [{"type": "summary_text", "text": "模型推理内容"}]
}
```

如果该次响应没有工具调用，thinking 内容会被直接丢弃（DeepSeek 文档说明无工具调用的轮次不需要回传 reasoning）。

### 请求侧（OpenAI → Anthropic）

当 Codex 在后续请求的 `input` 数组中回传了 `type: "reasoning"` item 时，Moon Bridge 会提取 `summary[0].text` 并将其重构为 Anthropic 格式的 `content[].thinking` block，注入到对应的 assistant 消息前。

```json
{
  "type": "message",
  "role": "assistant",
  "content": [
    {"type": "thinking", "thinking": "模型推理内容"},
    {"type": "text", "text": "最终回答"},
    {"type": "tool_use", "id": "...", "name": "...", "input": {...}}
  ]
}
```

### 为什么用 summary 字段

`type: "reasoning"` output item 的 `summary` 字段是 OpenAI Responses API 的标准字段。Codex 的 `ContextManager` 会自动记录并回放所有 `ResponseItem`，包括 `type: "reasoning"`。这确保了 thinking 内容可以跨 HTTP 请求持久化，而不依赖 Moon Bridge 的内存状态。

### 仅回放必要内容

根据 DeepSeek 官方文档：
- **无工具调用的轮次**：`reasoning_content` 不需要回传（API 会忽略）
- **有工具调用的轮次**：`reasoning_content` 必须完整回传（缺少则 400 错误）

Moon Bridge 只在响应包含工具调用时才生成 `type: "reasoning"` item，避免在上下文中携带不必要的推理内容。

## 功能详解

### 1. reasoning_content 剥离

每次将历史消息转为下一轮 Anthropic input 时，扩展会遍历所有消息内容，删除顶层的 `reasoning_content` 字段以及嵌套在 `content` 数组中的 `reasoning_content` 部分。

这样 DeepSeek 不会因为收到非法字段而返回 400。

### 2. reasoning_effort 映射

当 Codex 等 OpenAI 客户端在请求中传入 `reasoning`（如 `{"effort": "high"}`），扩展会：

| OpenAI effort | DeepSeek thinking level | budget_tokens |
|---------------|------------------------|---------------|
| low / medium  | high                    | max_tokens / 2 |
| high / max    | max                     | max_tokens * 3/4 |

同时移除 `temperature` 和 `top_p` 字段。

### 3. 流式处理

流式模式下，扩展通过 `StreamState` 逐事件收集 `thinking_delta` / `reasoning_content_delta` / `signature_delta`。当后续出现 `tool_use` 时，Moon Bridge 会先下发一个完整的 `type: "reasoning"` output item（包含 `response.output_item.added` 与 `response.output_item.done`，且两者都携带必需的 `summary` 字段），再下发工具调用 item。

如果当前请求历史里已经存在工具链，且本轮 DeepSeek 返回的是最终文本回答而不是新的工具调用，Moon Bridge 也会在文本消息前下发 reasoning item。DeepSeek thinking + tool-call 流程里的最终文本回答同样需要在后续 resume 时带回 `content[].thinking`，否则上游仍会返回缺少 thinking block 的 400。

这样做是为了让 Codex 将 reasoning item 作为历史项持久化。只把 reasoning 放进最终 `response.completed.response.output` 不够可靠，resume 时 Codex 可能不会把它重放到下一轮 input，进而导致 DeepSeek 报缺少 `content[].thinking`。

## 模块结构

```
internal/extensions/deepseek_v4/
├── deepseek_v4.go    # 核心转换函数：剥离、提取、注入、请求变异
├── deepseek_v4_test.go
├── state.go          # State / StreamState：请求级记忆管理和流式状态跟踪
```

## 与 Bridge 的集成

扩展的触发点分布在 Bridge 层的多个位置：

| 位置 | 操作 |
|------|------|
| `bridge/bridge.go:FromAnthropicWithPlanAndContext()` | 非流式响应中收集 thinking 文本，生成 reasoning output item |
| `bridge/stream.go:ConvertStreamEventsWithContext()` | 流式响应中维护 DeepSeek thinking 状态，并根据工具历史决定是否持久化最终文本 reasoning |
| `bridge/request.go:convertInput()` | 解析 `type: "reasoning"` input item，重构 thinking block |
| `bridge/bridge.go:ToAnthropic()` | 调用 `ToAnthropicRequest` 变异请求 |
| `bridge/stream_events.go` | 流式事件中识别和收集 thinking delta，并在工具调用前下发 reasoning output item |

## 注意事项

- 扩展仅在 `mode: Transform` 且当前模型路由到的 Provider 配置了 `deepseek_v4: true` 时生效。
- Thinking 的跨轮回放依赖 Codex 在 `input` 数组中回传 `type: "reasoning"` output item。如果客户端不会回传（如非 Codex 客户端），则跨轮回放可能失败。
- `ReasoningResponseItem` 的 `summary` 字段当前只携带 thinking 文本，不携带 signature。对于 signature-only thinking block（无文本只有签名），不会生成 reasoning item。
- 同一 HTTP 请求内的工具链（同次响应中先 thinking 后多次 tool_use），仍使用请求级 `State` 缓存。
