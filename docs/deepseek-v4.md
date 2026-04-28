# DeepSeek V4 扩展

Moon Bridge 内置 DeepSeek V4 Provider 扩展，处理 DeepSeek 特有的推理内容（thinking/reasoning_content）行为，使得 Codex CLI 等客户端可以通过 Anthropic Messages 兼容接口使用 DeepSeek V4 模型。

## 为什么需要扩展

DeepSeek V4 (deepseek-v4-pro 等) 基于 Anthropic Messages 兼容接口暴露，但存在几个与标准 Anthropic 协议不同的行为：

- **reasoning_content 不能回传**：DeepSeek 在前一轮响应中返回 `reasoning_content`，但若下一轮请求的 input 中包含该字段，上游会返回 400 错误。
- **thinking 块不自动保留**：与原生 Anthropic 不同，DeepSeek 不会在后续轮次中自动保留前一轮的 `thinking` block。客户端需要自行记忆并重新注入。
- **temperature / top_p 被忽略**：DeepSeek 不支持这些参数，传了可能引发某些代理层 Warning，但无实际作用。
- **推理档位使用 Codex 标准 effort 元数据表达**：Provider 模型目录用与其他模型相同的 `default_reasoning_level` / `supported_reasoning_levels` 声明 `high` / `xhigh` 两档；Transform 会把 Codex/OpenAI 请求里的 `reasoning.effort` 映射到 DeepSeek Anthropic 兼容参数 `output_config.effort`，其中 `xhigh` 映射为 DeepSeek 的 `max`。

## 配置启用

在 `config.yml` 的具体 Provider 段中设置：

```yaml
provider:
  providers:
    deepseek:
      base_url: "https://api.deepseek.com"
      api_key: "replace-with-deepseek-api-key"
      models:
        deepseek-v4-pro:
          deepseek_v4: true
          default_reasoning_level: "high"
          supported_reasoning_levels:
            - effort: "high"
              description: "High reasoning effort"
            - effort: "xhigh"
              description: "Extra high reasoning effort (maps to DeepSeek max)"
  routes:
    moonbridge: "deepseek/deepseek-v4-pro"
```

开启后，Moon Bridge 只对路由到该 Provider 模型的 Transform 请求启用 DeepSeek 兼容逻辑。其他 Provider 或未启用模型的请求不会剥离 reasoning_content、注入 thinking，也不会移除 temperature / top_p。

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

如果 DeepSeek 只返回 `signature_delta`、没有 thinking 文本，Moon Bridge 会把签名编码进 `summary[0].text` 的内部标记中。Codex 会照常持久化并回放该 reasoning item，Moon Bridge 在下一轮再解码成空文本但带 signature 的 Anthropic `thinking` block。

如果该次响应没有工具调用，thinking 内容会被直接丢弃（DeepSeek 文档说明无工具调用的轮次不需要回传 reasoning）。

注意：不会为了兜底生成空 `reasoning.summary`。response 侧只有真实 thinking 文本或可编码的 signature-only thinking 时才生成 reasoning item。

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

如果旧历史或异常历史里缺少可回放的 reasoning item，DeepSeek V4 扩展会在 tool_use / 工具链后的 assistant 文本前补一个空 `thinking` block，避免 Moon Bridge 重启后因为内存缓存为空而再次向上游发送缺少 `content[].thinking` 的请求。这个兜底只发生在 Anthropic 请求侧，不会回写或伪造 Codex 历史中的空 summary；真正插入兜底块时会输出 warning 日志，正常从 summary 或进程内 `State` 恢复 thinking 时不会告警。主桥接层只保留并传递 OpenAI `reasoning.summary`，summary 解码、State 恢复、空 thinking 兜底和 warning 都属于 DeepSeek V4 插件职责。

最新重启 resume trace 验证到一种旧历史形态：27 个 assistant 消息在重启前由内存 `State` 注入了 `thinking:""` + `signature`，但旧版本没有把 signature-only thinking 生成到 `reasoning.summary`，因此 Codex 无法回放它们。新版本只能对之后新产生的 signature-only thinking 编码进 summary；旧历史中已经缺失的 signature 会走空 `thinking` block 兜底。

### 为什么用 summary 字段

`type: "reasoning"` output item 的 `summary` 字段是 OpenAI Responses API 的标准字段。Codex 的 `ContextManager` 会自动记录并回放所有 `ResponseItem`，包括 `type: "reasoning"`。这确保了 thinking 内容可以跨 HTTP 请求持久化，而不依赖 Moon Bridge 的内存状态。

历史兼容上，旧版本只把非空 thinking 文本写入 summary。signature-only thinking 的文本为空，旧逻辑会跳过 reasoning item，只能依赖进程内 Session 缓存；这就是 Moon Bridge 重启后 resume 失败的核心原因。

### 仅回放必要内容

根据 DeepSeek 官方文档：
- **无工具调用的轮次**：`reasoning_content` 不需要回传（API 会忽略）
- **有工具调用的轮次**：`reasoning_content` 必须完整回传（缺少则 400 错误）

Moon Bridge 只在响应包含工具调用时才生成 `type: "reasoning"` item，避免在上下文中携带不必要的推理内容。

## 功能详解

### 1. reasoning_content 剥离

每次将历史消息转为下一轮 Anthropic input 时，扩展会遍历所有消息内容，删除顶层的 `reasoning_content` 字段以及嵌套在 `content` 数组中的 `reasoning_content` 部分。

这样 DeepSeek 不会因为收到非法字段而返回 400。

### 2. 请求参数清理

扩展会移除 `temperature` 和 `top_p` 字段，避免 DeepSeek-compatible Provider 或代理层因为不支持这些采样参数而产生 Warning 或拒绝请求。

当 OpenAI Responses 请求包含 `reasoning.effort: "high"` 时，扩展会写入 `output_config.effort: "high"`；当请求包含 `reasoning.effort: "xhigh"` 时，会写入 DeepSeek 需要的 `output_config.effort: "max"`。插件也接受直接传入的 `max`，但 Codex 的 model catalog 中必须使用合法枚举 `xhigh`。

### 3. 流式处理

流式模式下，扩展通过 `StreamState` 逐事件收集 `thinking_delta` / `reasoning_content_delta` / `signature_delta`。当后续出现 `tool_use` 时，Moon Bridge 会先下发一个完整的 `type: "reasoning"` output item（包含 `response.output_item.added` 与 `response.output_item.done`，且两者都携带必需的 `summary` 字段），再下发工具调用 item。signature-only thinking 会编码进 summary 内部标记，保证重启后仍能从 Codex 历史恢复。

如果当前请求历史里已经存在工具链，且本轮 DeepSeek 返回的是最终文本回答而不是新的工具调用，Moon Bridge 也会在文本消息前下发 reasoning item。DeepSeek thinking + tool-call 流程里的最终文本回答同样需要在后续 resume 时带回 `content[].thinking`，否则上游仍会返回缺少 thinking block 的 400。

这样做是为了让 Codex 将 reasoning item 作为历史项持久化。只把 reasoning 放进最终 `response.completed.response.output` 不够可靠，resume 时 Codex 可能不会把它重放到下一轮 input，进而导致 DeepSeek 报缺少 `content[].thinking`。

## 模块结构

```
internal/extension/deepseek_v4/
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
| `bridge/bridge.go:ToAnthropic()` | 调用 `ToAnthropicRequest` 清理 DeepSeek 不支持的采样参数，并按 `reasoning.effort` 写入 `output_config.effort` |
| `bridge/stream_events.go` | 流式事件中识别和收集 thinking delta，并在工具调用前下发 reasoning output item |

## 注意事项

- 扩展仅在 `mode: Transform` 且当前模型在 Provider 模型目录中配置了 `deepseek_v4: true` 时生效。
- 推理档位不需要额外的 DeepSeek 专用配置字段，也不需要为 `high` / `max` 创建虚拟模型 slug；在模型目录中声明 Codex 合法的 `high` / `xhigh` reasoning levels 即可。
- Thinking 的跨轮回放依赖 Codex 在 `input` 数组中回传 `type: "reasoning"` output item。如果客户端不会回传（如非 Codex 客户端），则跨轮回放可能失败。
- `ReasoningResponseItem` 的 `summary` 字段通常携带 thinking 文本；signature-only thinking 会使用 Moon Bridge 内部标记编码 signature。
- 同一 HTTP 请求内的工具链（同次响应中先 thinking 后多次 tool_use）仍会使用请求级 `State` 缓存；跨重启时则优先依赖 Codex 回放的 reasoning item，缺失时补空 thinking block 兜底。
