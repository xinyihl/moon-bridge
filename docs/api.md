# Moon Bridge API

Moon Bridge 对外暴露 API 兼容 OpenAI Responses API。Transform 模式下，请求会按模型别名路由：Anthropic 协议 Provider 会经过协议转换后发送为 Anthropic Messages API；`protocol: "openai"` 的 Provider 会保留 OpenAI Responses 格式直接透传到上游。

## 端点

| 路径 | 方法 | 说明 |
|------|------|------|
| `/v1/responses` | POST | OpenAI Responses 协议端点 |
| `/responses` | POST | 兼容 `responses` 路径的别名端点 |

两个端点行为完全一致。在 **Capture** 模式下，代理会转发所有路径（包含 `/v1/messages`）到上游。

## 支持的输入字段

Transform 模式下，以下 OpenAI Responses 请求字段被支持：

| 字段 | 类型 | 说明 |
|------|------|------|
| `model` | `string` | 必需。会通过 `provider.models` 路由到上游 Provider，并映射为上游真实模型名 |
| `input` | `string` 或 `array` | 消息输入，详见下文 |
| `instructions` | `string` | 开发者/系统指令，转为 Anthropic system 块的前缀 |
| `max_output_tokens` | `int` | 默认值由 `provider.default_max_tokens` 控制（最终 fallback 1024）|
| `temperature` | `float` | 透传 |
| `top_p` | `float` | 透传 |
| `stop` | `string \| string[]` | 转为 Anthropic `stop_sequences` |
| `tools` | `array` | 工具声明，详见下文 |
| `tool_choice` | `string \| object` | `"auto"` / `"none"` / `"required"` / `{type:"function",name:"..."}` |
| `stream` | `bool` | 启用 SSE 流式响应 |
| `metadata` | `object` | 透传至 Anthropic 请求 |
| `prompt_cache_key` | `string` | 自定义缓存键，与 cache planner 配合使用 |
| `prompt_cache_retention` | `string` | `"in_memory"`（映射为 `5m` TTL）或 `"24h"`（仅当 `allow_retention_downgrade:true` 时降级为 `1h`）|
| `parallel_tool_calls` | `bool` | 已解析；Anthropic 转换层当前不单独使用该字段 |
| `store` / `previous_response_id` | `bool` / `string` | 已解析；当前不提供本地 response store，不会按 previous response 拉取历史 |

### input

支持字符串和数组两种格式。

- 字符串：转换为单条 `user` 消息。
- 数组：每项对应一个 conversation item。支持的类型包括：
  - `message`（`user` / `assistant` / `system` / `developer` role）
  - 文本 content part：`input_text` / `text` / `output_text`
  - `function_call` — 转为 Anthropic `tool_use` block
  - `function_call_output` / `*_output` — 转为 `tool_result` block
  - `local_shell_call` — 转为 `tool_use`（name=`local_shell`）
  - `custom_tool_call` — 转为 `tool_use`，保留原始 `input` 内容
  - `web_search_call` — **忽略**，不会传给上游

当前 Transform 转换层只从 content part 中提取文本；图片、文件 ID、后台 response store 等能力尚未实现。

### tools

| OpenAI `type` | Anthropic 映射 |
|---------------|----------------|
| `function` | Anthropic tool 标准 `input_schema` |
| `local_shell` | Anthropic tool，`name: "local_shell"` |
| `custom` | Anthropic tool；Codex `apply_patch` grammar 拆成 add/delete/update/replace/batch 结构化工具，Code Mode `exec` 暴露为 `source` schema，其他 custom freeform 包装为 `input` |
| `namespace` | 发往 Anthropic 时展平为子工具的 `namespace__tool` 命名；响应回 Codex 时 function 工具拆回 `namespace` + 子工具 `name` |
| `web_search` / `web_search_preview` | 默认映射为 Anthropic server tool `web_search_20250305`；当 `provider.web_search.support` 为 `injected` 时注入 `tavily_search` / `firecrawl_fetch` function 工具；探测不支持或配置为 `disabled` 时跳过 |
| `file_search` / `computer_use_preview` / `image_generation` | **忽略** |

## 流式事件

Anthropic Messages SSE 事件流会被逐事件转换为 OpenAI Responses 格式的事件流。

### 事件映射

| Anthropic 事件 | OpenAI 事件 |
|----------------|-------------|
| `message_start` | `response.created`, `response.in_progress` |
| `content_block_start` | `response.output_item.added`, `response.content_part.added` |
| `content_block_delta` (text) | `response.output_text.delta` |
| `content_block_delta` (input_json) | `response.function_call_arguments.delta` |
| `content_block_stop` | `response.output_text.done`, `response.content_part.done`, `response.output_item.done` |
| `message_delta` | 更新状态/usage |
| `message_stop` | `response.completed` / `response.incomplete` |
| `error` | `response.failed` |

### 特殊工具流式转换

- **`local_shell_call`**：input_json_delta 转为 local shell action，不产生 `function_call_arguments.delta`。
- **`web_search_call`**：流式 `input_json_delta` 并入 `action` 字段而非 `function_call_arguments`；空搜索 action 会被过滤。
- **`custom_tool_call`**：流式 `input_json_delta` 转为 `response.custom_tool_call_input.delta` 事件，最终产出 `custom_tool_call` 输出项。
- **`apply_patch` 拆分工具**：Anthropic 侧的 `apply_patch_add_file` / `apply_patch_delete_file` / `apply_patch_update_file` / `apply_patch_replace_file` / `apply_patch_batch` 最终都会回映射为 Codex 看到的 `custom_tool_call.name="apply_patch"`。

## 非流式响应

```json
{
  "id": "resp_msg_xxxx",
  "object": "response",
  "status": "completed",
  "model": "moonbridge",
  "output": [ ... ],
  "output_text": "...",
  "usage": {
    "input_tokens": 1000,
    "output_tokens": 200,
    "total_tokens": 1200,
    "input_tokens_details": {
      "cached_tokens": 500
    }
  },
  "metadata": {
    "provider_message_id": "msg_xxxx",
    "provider_usage": { ... }
  }
}
```

字段说明：
- `id`：以 `resp_` 前缀包装上游消息 ID。
- `status`：根据 Anthropic `stop_reason` 映射：`end_turn`/`stop_sequence` → `completed`；`max_tokens` → `incomplete`（reason=`max_output_tokens`）；`model_context_window` → `incomplete`（reason=`max_input_tokens`）。
- `usage.input_tokens`：Anthropic Transform 响应中会累加 Anthropic 的 `input_tokens` + `cache_creation_input_tokens` + `cache_read_input_tokens`；OpenAI 协议直通 Provider 的响应 usage 保持上游原样。
- `usage.input_tokens_details.cached_tokens`：始终序列化（即使为 0），兼容 Codex 反序列化。

日志里的每请求 `Usage: ... Input` 是单独的可读展示口径，采用 `input_tokens + cache_read_input_tokens`，不把 `cache_creation_input_tokens` 额外计入展示值。`Billing` 始终是当前 session 累计费用。

## 输出项类型

| Output Type | 来源 | 说明 |
|-------------|------|------|
| `message` | Anthropic `text` block | 含 `output_text` content |
| `function_call` | Anthropic `tool_use`（非 local_shell）| id 前缀 `fc_` |
| `local_shell_call` | Anthropic `tool_use`（name=local_shell）| id 前缀 `lc_` |
| `custom_tool_call` | Anthropic `tool_use`（在 ConversionContext 中标记为 custom；grammar proxy 会先拼回 raw input）| id 前缀 `ctc_` |
| `web_search_call` | Anthropic `server_tool_use:web_search` | id 前缀 `ws_` |

## 错误响应

遵循 OpenAI 错误格式：

```json
{
  "error": {
    "message": "model is required",
    "type": "invalid_request_error",
    "param": "model",
    "code": "missing_required_parameter"
  }
}
```

Anthropic Provider 错误会被映射为 OpenAI 等价的 HTTP 状态码和错误码：

| Provider 状态码 | OpenAI 状态码 | Error Code |
|----------------|---------------|------------|
| 401 | 401 | `invalid_api_key` |
| 403 | 403 | `permission_denied` |
| 429 | 429 | `rate_limit_exceeded` |
| 504 | 504 | `provider_timeout` |
| 5xx | 502 | `provider_error` |
| 4xx | 400 | `invalid_request_error` |

## 透明代理 API（Capture 模式）

**CaptureResponse** 和 **CaptureAnthropic** 模式下，所有请求按原协议透传至上游，不进行协议转换。请求/响应会被 trace 系统完整记录（路径下配置）。
代理会覆盖 `Authorization` / `X-Api-Key` Header，但保留客户端 `User-Agent` 等原始 Header。

Transform 模式中的 `protocol: "openai"` Provider 不是 Capture 模式：它只对匹配该 Provider 的模型别名执行 Responses 直通，并会在请求体中把 `model` 改写为配置的上游模型名。
