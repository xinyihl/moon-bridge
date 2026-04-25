# Anthropic Messages Provider 到 OpenAI Responses 转发层设计

更新时间：2026-04-25

## 目标

对外提供 OpenAI Responses 兼容接口，对内转发到 Anthropic Messages 兼容 Provider API。调用方继续使用 `/v1/responses` 的请求、响应和流式事件；转发层负责把输入、工具调用、模型参数、错误和用量转换成 OpenAI Responses 语义。

## 边界

- 支持：`POST /v1/responses` 非流式、`stream=true` 流式、文本、多轮消息、函数工具调用、工具结果回传。
- 有条件支持：图片输入、reasoning/thinking、结构化输出、Anthropic prompt caching，需要 Provider 明确支持并通过能力开关开启。
- 不默认支持：OpenAI 内置工具（`web_search`、`file_search`、`computer_use`、`code_interpreter` 等）、文件 ID 自动解析、后台响应、远程 response retrieve/cancel。

## 总体架构

```text
OpenAI Responses Client
        |
        | POST /v1/responses
        v
Responses Facade
  - 鉴权与请求校验
  - OpenAI DTO 解析
  - 能力降级/拒绝
        |
        v
Protocol Translator
  - request: Responses -> Anthropic Messages
  - stream: Anthropic SSE -> Responses SSE
  - response: Anthropic Message -> Response object
        |
        v
Anthropic Provider Client
  - POST /v1/messages
  - provider auth / anthropic-version
  - 超时、重试、限流、trace id
```

建议 Go 包结构：

```text
internal/openai        # OpenAI Responses DTO 和 SSE event DTO
internal/anthropic     # Anthropic Messages DTO 和 Provider client
internal/bridge        # 双向转换、能力检查、错误映射
internal/cache         # prompt cache 策略、breakpoint 注入、usage 归一化
internal/server        # HTTP handler: /v1/responses
internal/store         # 可选：store=true / previous_response_id
```

## 请求映射

| OpenAI Responses | Anthropic Messages | 处理规则 |
| --- | --- | --- |
| `model` | `model` | 通过配置表做别名映射，不在代码里硬编码供应商模型名。 |
| `instructions` | `system` | 作为最高优先级系统指令；多个 system/developer 输入按顺序拼接到 `system`。 |
| `input` 字符串 | `messages[0].content` | 转为单条 `role=user` 文本消息。 |
| `input[].role=user` | `messages[].role=user` | `input_text` 转 text block；图片按 Provider 能力转 image block。 |
| `input[].role=assistant` | `messages[].role=assistant` | 历史 assistant 文本转 text block；历史 function call 转 tool_use block。 |
| `function_call_output` | `role=user` + `tool_result` | `call_id` 映射为 Anthropic `tool_use_id`。 |
| `max_output_tokens` | `max_tokens` | Anthropic 侧需要输出上限；缺省值从服务配置注入。 |
| `temperature` / `top_p` | 同名字段 | 直传；不支持的参数忽略或返回 `unsupported_parameter`。 |
| `stop` | `stop_sequences` | 字符串或数组统一转字符串数组。 |
| `metadata` / `user` | `metadata` | 只转 Provider 支持字段；完整原值写入 trace 日志。 |
| `prompt_cache_key` | 无直接上游字段 | 仅用于转发层 cache 分区、上游 workspace/API key 选择和日志，不可伪装成 Anthropic 请求字段。 |
| `prompt_cache_retention` | `cache_control.ttl` | `in_memory` 映射为默认 5 分钟 `ephemeral`；`24h` 默认拒绝，除非配置允许降级为 Anthropic `1h`。 |
| `stream` | `stream` | 直传，并切换为 SSE 事件转换器。 |

### 输入规范化

1. 将 OpenAI `input` 统一规整成有序消息列表。
2. 抽取 `system` 和 `developer` 角色内容，合并到 Anthropic 顶层 `system`。
3. 保持 `user` / `assistant` 消息顺序，避免跨轮合并导致工具调用上下文错位。
4. 对多模态内容按能力表处理：
   - `input_text`：支持。
   - `input_image.image_url`：Provider 支持 URL 时直传；否则下载转 base64 需要显式开启。
   - `input_image.file_id`、`input_file`：默认拒绝，返回 OpenAI 风格错误。
5. `previous_response_id` 只有在开启 `store=true` 且本地存储存在时支持；否则返回 400。

## Prompt 缓存映射

这是桥接层必须单独处理的一等能力。OpenAI prompt caching 对调用方基本自动生效，而 Anthropic Messages 需要通过 `cache_control` 启用自动缓存或显式缓存断点。转发层不能只做字段翻译，否则所有 Anthropic 侧缓存都会失效或不可预测。

### 策略模式

| 模式 | 行为 | 适用场景 |
| --- | --- | --- |
| `off` | 不向 Anthropic 请求注入 `cache_control`。 | 严格无缓存或 Provider 不支持缓存。 |
| `automatic` | 在 Anthropic 顶层请求添加 `cache_control:{type:"ephemeral"}`，可选 `ttl:"1h"`。 | 多轮对话、希望缓存断点随历史自动前移。 |
| `explicit` | 在稳定前缀的最后一个 content block / tool / system block 上添加 `cache_control`。 | 大系统提示、工具定义、长文档、示例集等静态前缀。 |
| `hybrid` | 顶层自动缓存 + 少量显式断点。 | 同时缓存工具/系统提示和增长中的对话历史。 |

默认建议使用 `automatic`，但遇到“前缀后面带时间戳、用户动态上下文、随机 tool 参数顺序”等场景必须切换到 `explicit`，把断点放在最后一个稳定块上。

### 缓存创建方案

Anthropic 没有单独的“创建缓存”API。缓存是在一次正常 `messages.create` 请求里，由 `cache_control` 标记触发创建；后续请求只有在模型、上游 workspace/API key、工具定义、system、messages 前缀和断点位置都保持一致时才可能命中。因此桥接层需要一个 `CacheCreationPlanner`，在每次转发前生成缓存创建计划。

#### 创建时机

| 时机 | 行为 | 推荐默认值 |
| --- | --- | --- |
| 首次真实请求 | 在真实请求里注入 `cache_control`，让首次请求承担写缓存成本。 | 默认启用，最简单可靠。 |
| 主动预热 | 后台发送一次包含稳定前缀和极短动态尾巴的普通 Messages 请求。 | 默认关闭，只给长系统提示/大工具集开启。 |
| 并发首请求 | 同一 cache key 的首个请求做 leader，其它请求等待 leader 上游响应开始后再转发。 | 可配置，避免并发同时写缓存。 |
| 缓存快过期 | 根据本地 registry 的 `expires_at` 提前刷新。 | 默认关闭，避免无业务请求时烧 token。 |

主动预热不是“免费建缓存”：它仍然是一次模型调用，会产生输入和少量输出成本。预热请求必须把断点放在稳定前缀末尾，预热用的临时用户消息不能进入要缓存的前缀。

#### 计划输入

`CacheCreationPlanner` 输入：

- `model`：OpenAI 模型名和映射后的 Anthropic Provider 模型名。
- `tenant_id` / `api_key_id`：用于隔离不同客户、不同上游 key 的缓存。
- `prompt_cache_key`：OpenAI 侧路由提示，只作为本地分区键，不透传。
- `prompt_cache_retention`：映射为 Anthropic `5m` 或 `1h` TTL。
- `tools_hash`：对工具 schema 做稳定 JSON 编码后的哈希。
- `system_hash`：对 system blocks 做稳定 JSON 编码后的哈希。
- `message_prefix_hash`：对可缓存历史消息前缀做稳定 JSON 编码后的哈希。
- `estimated_tokens`：本地粗估 token，用于跳过明显低于缓存阈值的短前缀。
- `cache_registry_state`：本地记录的 warming / warm / expired / not_cacheable 状态。

本地 cache key 只用于转发层管理，不代表 Anthropic 真实缓存 key：

```text
local_cache_key =
  sha256(provider_id, upstream_workspace, upstream_api_key_id,
         anthropic_model, ttl, prompt_cache_key,
         tools_hash, system_hash, message_prefix_hash,
         breakpoint_shape)
```

#### 创建决策

1. 能力检查：Provider 未开启 `prompt_caching` 时直接跳过；请求显式禁用缓存时跳过。
2. 稳定性检查：包含时间戳、request id、随机数、一次性检索片段、最新用户消息时，不把这些块纳入缓存前缀。
3. 阈值检查：前缀 token 明显低于模型缓存最小阈值时不创建，并标记 `not_cacheable`。
4. 价值检查：`estimated_tokens * expected_reuse_count` 低于配置阈值时不创建，避免小 prompt 写缓存反而更贵。
5. registry 检查：已有 `warm` 且未过期时继续注入同样断点以读缓存；`warming` 时按并发策略等待或直通。
6. TTL 选择：OpenAI `in_memory` 或缺省值映射 `5m`；显式长缓存只允许映射 `1h`，`24h` 默认拒绝。
7. 断点选择：优先 `tools` → `system` → `messages` 的稳定大前缀，最多 4 个断点。

输出结构：

```go
type CacheCreationPlan struct {
	Mode        string // off, automatic, explicit, hybrid
	TTL         string // 5m, 1h
	LocalKey    string
	Breakpoints []CacheBreakpoint
	WarmPolicy  string // none, leader, background
	Reason      string
}

type CacheBreakpoint struct {
	Scope     string // tools, system, messages
	BlockPath string // e.g. tools[3], system[0], messages[12].content[0]
	TTL       string
	Hash      string
}
```

#### 断点放置

| 前缀形态 | 断点位置 | 说明 |
| --- | --- | --- |
| 大工具集 + 普通 system | 最后一个 tool definition | 工具 schema 最稳定，适合跨会话复用。 |
| 长 system / 长开发规范 | 最后一个 system text block | `system` 需要用 block 数组表达，才能挂 `cache_control`。 |
| 长文档作为首轮 user 内容 | 文档最后一个 text/image block | 用户后续问题必须放在断点之后。 |
| 多轮长会话 | 最近一段稳定历史消息末尾 | 最新用户问题不纳入缓存前缀。 |
| 工具调用后的继续对话 | 最后一个稳定 `tool_result` 后 | 工具结果若每次不同，不应该缓存。 |

混合 TTL 时，长 TTL 断点要放在短 TTL 断点之前，避免上游拒绝请求或命中行为异常。所有断点都必须在稳定编码后再注入，避免因为 Go map 顺序变化导致每次都是新前缀。

#### 注入算法

1. 先构造不带缓存字段的 Anthropic request。
2. 对 `tools`、`system`、`messages` 做 canonical JSON 编码和哈希。
3. 运行 `CacheCreationPlanner` 得到 `CacheCreationPlan`。
4. `automatic`：在顶层请求设置 `cache_control:{type:"ephemeral"}`；TTL 为 `1h` 时附加 `ttl:"1h"`。
5. `explicit`：在选中的 tool / system block / content block 上设置 `cache_control:{type:"ephemeral"}`。
6. `hybrid`：先放显式稳定断点，再按断点数量余量决定是否加顶层 automatic。
7. 注入完成后再次 canonical JSON 编码，生成 `request_fingerprint` 写入日志，便于排查命中率。

示例：缓存工具和 system，最新用户问题不缓存。

```json
{
  "model": "claude-sonnet-4-5",
  "max_tokens": 1024,
  "tools": [
    {
      "name": "search_docs",
      "description": "Search project docs",
      "input_schema": {"type": "object", "properties": {"query": {"type": "string"}}},
      "cache_control": {"type": "ephemeral", "ttl": "1h"}
    }
  ],
  "system": [
    {
      "type": "text",
      "text": "You are Moon Bridge...",
      "cache_control": {"type": "ephemeral", "ttl": "1h"}
    }
  ],
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "本轮问题，不放入缓存前缀"}]}
  ]
}
```

#### 并发与预热

- `singleflight`：同一 `local_cache_key` 首次创建时只允许一个 leader 请求写缓存，followers 根据配置等待 leader 收到上游首个响应事件，或直接转发但不保证命中。
- `background warm`：只允许对显式稳定前缀使用；预热请求加一个固定短尾巴，例如“请仅回复 OK”，并设置很小的 `max_tokens`。
- `warm registry`：记录 `local_cache_key`、断点哈希、TTL、`created_at`、`expires_at`、最近一次 `cache_creation_input_tokens`、最近一次 `cache_read_input_tokens`，不保存 prompt 原文。
- `stale refresh`：默认不做；如果业务明确需要低延迟，可在 `expires_at - refresh_window` 后由下一次真实请求刷新。

#### 创建结果判定

Anthropic 是否真的创建或命中缓存必须看 usage，而不能只看请求里有没有 `cache_control`：

| usage 信号 | 本地状态 | 处理 |
| --- | --- | --- |
| `cache_creation_input_tokens > 0` | `warm` | 写缓存成功，更新 `expires_at`。 |
| `cache_read_input_tokens > 0` | `warm` | 命中缓存，累计命中率指标。 |
| 两者都是 0 且 `input_tokens` 很低 | `not_cacheable` | 可能低于阈值，短期禁用该 key。 |
| 两者都是 0 且 token 足够 | `missed` | 记录断点、TTL、hash，用于排查前缀不稳定。 |
| Provider 返回缓存参数错误 | `failed` | 可按配置降级重试一次：去掉 cache 字段后转发。 |

关键指标：

- `cache.create.tokens`
- `cache.read.tokens`
- `cache.hit_rate`
- `cache.write_count`
- `cache.miss_count`
- `cache.not_cacheable_count`
- `cache.prefix_fingerprint_changed_count`

### 断点注入规则

1. Anthropic 缓存前缀顺序固定为 `tools` → `system` → `messages`，所以转发层必须先稳定序列化 tools，再构造 system，再构造 messages。
2. `instructions` 若需要显式缓存，不能只作为字符串传给 `system`，应转为 system text block 并附加 `cache_control`。
3. 工具定义变化会让后续缓存失效；Go 实现里必须对 tool JSON Schema 做稳定排序和稳定编码，避免 map 随机顺序破坏缓存命中。
4. 显式断点最多按 Provider 限制控制在 4 个以内；`hybrid` 模式下顶层自动缓存也会占用一个断点槽位。
5. 不要把断点放在每次变化的用户消息、时间戳、request id 或临时上下文后面；缓存写入只发生在断点位置，变化块会导致后续请求无法命中。
6. Anthropic 短 prompt 低于模型缓存阈值时可能静默不缓存；必须通过 usage 字段判断是否真的命中或写入。

### OpenAI 参数兼容

- `prompt_cache_key`：OpenAI 用它影响缓存路由；Anthropic 没有等价字段。转发层只把它作为内部分区键，可用于选择固定上游 API key/workspace、trace 聚合和 cache 策略，不透传给 Provider。
- `prompt_cache_retention:"in_memory"`：映射到 Anthropic 默认 5 分钟 ephemeral cache。
- `prompt_cache_retention:"24h"`：Anthropic 只有 5 分钟和 1 小时 TTL。默认返回 `unsupported_parameter`，如果配置 `allow_retention_downgrade=true`，则降级为 `ttl:"1h"` 并在 `metadata.cache_retention_downgraded=true` 标记。
- 未传 `prompt_cache_retention`：按服务端默认策略决定是否启用 Anthropic 缓存，不应因为 OpenAI 兼容接口未传该字段就关闭缓存。

### 用量归一化

Anthropic 开启 prompt caching 后，`usage.input_tokens` 只代表最后一个 cache breakpoint 之后的新鲜输入。OpenAI 侧更适合暴露总输入 token 和 cached token：

```text
openai.usage.input_tokens =
  anthropic.usage.cache_read_input_tokens
  + anthropic.usage.cache_creation_input_tokens
  + anthropic.usage.input_tokens

openai.usage.input_tokens_details.cached_tokens =
  anthropic.usage.cache_read_input_tokens
```

`cache_creation_input_tokens`、`cache_creation.ephemeral_5m_input_tokens`、`cache_creation.ephemeral_1h_input_tokens` 放入 `metadata.provider_usage`，用于成本分析和命中率报表。

## 工具调用映射

| OpenAI tool | Anthropic tool | 处理规则 |
| --- | --- | --- |
| `{type:"function", name, description, parameters}` | `{name, description, input_schema}` | `parameters` 必须是 JSON Schema object。 |
| `tool_choice:"auto"` | `tool_choice:auto` 或省略 | 优先使用 Provider 原生 auto。 |
| `tool_choice:"none"` | `tool_choice:none` 或不传 tools | 若 Provider 不支持 none，则不传 `tools`。 |
| `tool_choice:"required"` | `tool_choice:any` | 表示必须调用某个工具。 |
| 指定函数名 | `tool_choice:{type:"tool", name}` | 名称必须存在于 tools。 |
| `parallel_tool_calls=false` | 能力开关/后置校验 | Anthropic 可能返回多个 `tool_use`，需要配置是否拒绝多工具结果。 |

Anthropic 返回 `tool_use` 时，OpenAI Response 的 `output[]` 里生成 `function_call` item：

```json
{
  "type": "function_call",
  "id": "fc_xxx",
  "call_id": "toolu_xxx",
  "name": "lookup_order",
  "arguments": "{\"order_id\":\"123\"}",
  "status": "completed"
}
```

调用方下一轮提交：

```json
{
  "type": "function_call_output",
  "call_id": "toolu_xxx",
  "output": "{\"status\":\"shipped\"}"
}
```

转发层把它转成 Anthropic `tool_result` content block，并放入下一轮 `role=user` 消息。

## 响应映射

| Anthropic Message | OpenAI Response | 处理规则 |
| --- | --- | --- |
| `id` | `metadata.provider_message_id` | OpenAI `id` 由转发层生成 `resp_*`，Provider ID 保留在 metadata。 |
| `type:"message"` | `object:"response"` | 固定输出 OpenAI Response object。 |
| `role:"assistant"` | `output[].type="message"` | 文本 content 合并为一个或多个 `output_text` part。 |
| `content[].text` | `output[].content[].text` | 同时累计到顶层 `output_text` 便捷字段。 |
| `content[].tool_use` | `output[].type="function_call"` | `tool_use.id` 作为 `call_id`。 |
| `usage.input_tokens` | `usage.input_tokens` | 未启用缓存时同名映射；启用缓存时按“用量归一化”计算总输入。 |
| `usage.cache_read_input_tokens` | `usage.input_tokens_details.cached_tokens` | 代表 OpenAI 侧 cached token。 |
| `usage.cache_creation_input_tokens` | `metadata.provider_usage.cache_creation_input_tokens` | 保留 Provider 成本字段，避免丢失写缓存成本。 |
| `usage.output_tokens` | `usage.output_tokens` | 同名映射。 |
| `stop_reason` | `status` / `incomplete_details` | 见下表。 |

`stop_reason` 建议映射：

| Anthropic `stop_reason` | OpenAI `status` | OpenAI `incomplete_details` | 说明 |
| --- | --- | --- | --- |
| `end_turn` | `completed` | `null` | 普通完成。 |
| `tool_use` | `completed` | `null` | 输出包含 `function_call`，等待调用方提交工具结果。 |
| `stop_sequence` | `completed` | `null` | 命中用户 stop，不视为错误。 |
| `max_tokens` | `incomplete` | `{reason:"max_output_tokens"}` | 输出达到上限。 |
| `model_context_window` | `incomplete` | `{reason:"max_input_tokens"}` | 上下文窗口限制。 |
| `refusal` | `completed` | `null` | 输出 refusal 内容；必要时映射到 `refusal` content part。 |
| `pause_turn` | `incomplete` | `{reason:"provider_pause"}` | Provider 需要继续轮询或重试时使用扩展 reason。 |

## 流式转换

OpenAI Responses 使用 SSE 事件；Anthropic Messages streaming 也使用 SSE，但事件名和 payload 不同。转发层必须维护一个 response 聚合状态，用来生成稳定的 `output_index`、`content_index`、`item_id`、`sequence_number`。

| Anthropic stream event | OpenAI Responses stream event |
| --- | --- |
| `message_start` | `response.created`，随后 `response.in_progress` |
| `content_block_start` text | 首个文本块时发 `response.output_item.added`，再发 `response.content_part.added` |
| `content_block_delta` text_delta | `response.output_text.delta` |
| `content_block_stop` text | `response.output_text.done`，再发 `response.content_part.done` |
| `content_block_start` tool_use | `response.output_item.added`，item 类型为 `function_call` |
| `content_block_delta` input_json_delta | `response.function_call_arguments.delta` |
| `content_block_stop` tool_use | `response.function_call_arguments.done`，再发 `response.output_item.done` |
| `message_delta` | 更新聚合 response 的 usage、stop_reason、status |
| `message_stop` | `response.completed` 或 `response.incomplete` |
| `error` | `response.failed` 或 OpenAI `error` event |
| `ping` | 忽略或转发注释帧 `: ping` |

SSE 输出要求：

- 每个事件包含单调递增 `sequence_number`。
- 流式过程中不要提前发送最终 `usage`，除非 Provider 已在 `message_delta` 给出。
- Anthropic caching 的 usage 可能出现在 `message_start`，需要在流式聚合状态里累加并在最终 response 里输出归一化 usage。
- 工具参数以字符串 delta 透传，最终 done 事件输出完整 JSON 字符串。
- Provider 连接中断时，发送 `response.failed`，并关闭 SSE。

## 错误映射

| Provider 情况 | OpenAI HTTP 状态 | OpenAI error |
| --- | --- | --- |
| 鉴权失败 | 401 | `invalid_api_key` |
| 权限不足/模型不可用 | 403 | `model_not_found` 或 `permission_denied` |
| 请求字段不支持 | 400 | `unsupported_parameter` |
| JSON Schema 无效 | 400 | `invalid_request_error` |
| 上下文超限 | 400 或 413 | `context_length_exceeded` |
| Provider 限流 | 429 | `rate_limit_exceeded` |
| Provider 5xx | 502 | `provider_error` |
| Provider 超时 | 504 | `provider_timeout` |

错误响应统一为 OpenAI 风格：

```json
{
  "error": {
    "message": "Unsupported tool type: web_search_preview",
    "type": "invalid_request_error",
    "param": "tools[0].type",
    "code": "unsupported_parameter"
  }
}
```

## 能力开关

建议启动时加载 Provider capability profile：

```yaml
provider:
  base_url: "https://provider.example.com"
  anthropic_version: "2023-06-01"
  default_max_tokens: 1024
  models:
    "openai-compatible-model": "provider-anthropic-model"
  capabilities:
    text: true
    images_url: true
    images_base64: false
    function_tools: true
    parallel_tool_calls: true
    prompt_caching: true
    automatic_prompt_cache: true
    explicit_cache_breakpoints: true
    reasoning: false
    json_schema_strict: false
    store_responses: false
  cache:
    mode: "automatic"          # off / automatic / explicit / hybrid
    ttl: "5m"                  # 5m / 1h
    allow_retention_downgrade: false
    max_breakpoints: 4
```

所有“不支持但请求中出现”的能力必须显式失败，避免静默降级造成业务误判。

## 安全与发布要求

- 不把 Provider API Key 暴露给客户端；客户端 key 与上游 key 分开配置。
- 日志默认脱敏 `Authorization`、tool arguments 中的密钥形态字段、图片/文件 base64。
- 请求和响应都记录 trace id，但不记录完整 prompt，除非开启安全审计模式。
- `.gitignore` 已忽略 `helloagents/`、`.codex`、`.claude`、`AGENTS.md`、`CLAUDE.md` 等 Agent 本地路径，发布包只包含产品代码和文档。

## 最小实现顺序

1. 定义 OpenAI Responses 和 Anthropic Messages DTO。
2. 实现非流式文本请求：`input` / `instructions` / `max_output_tokens` / `usage`。
3. 实现 prompt cache 创建计划：稳定哈希、断点选择、singleflight、预热、registry。
4. 实现 prompt cache 注入与回收：`cache_control` 注入、TTL 映射、usage 归一化、命中率日志。
5. 实现工具 schema 和 `tool_use` ↔ `function_call` 转换。
6. 实现工具结果 `function_call_output` ↔ `tool_result`。
7. 实现 Anthropic SSE 到 OpenAI Responses SSE 的事件转换。
8. 增加能力开关、错误映射、超时重试、trace id。
9. 增加兼容性测试：文本、多轮、缓存创建/命中/未命中、并发预热、工具调用、工具结果、max tokens、Provider error、streaming delta。

## 首版实现状态

- 已实现：配置加载、OpenAI/Anthropic DTO、Anthropic Provider client、非流式 `/v1/responses` 和 `/responses`、基础 SSE 输出、function tool 映射、Codex `local_shell` 工具映射、tool result 映射、prompt cache planner、`cache_control` 注入、usage 归一化、OpenAI 风格错误。
- 已测试：配置默认值与校验、DTO JSON、Provider client header/response/SSE 解析、缓存 planner、请求转换、响应转换、流式事件转换、HTTP handler。
- 暂未实现：OpenAI 内置工具、文件 ID 解析、response retrieve/cancel、持久化 registry、真实 token 计数、后台预热 worker。
- 当前缓存 registry 为内存实现，重启后不会保留命中状态；Anthropic 侧真实缓存仍由 Provider 管理。

## Codex CLI 兼容说明

- Codex CLI 可通过 `wire_api = "responses"` 和 `base_url = "http://localhost:8080/v1"` 接入 Moon Bridge。
- 转发层接受 Codex 常见请求字段：`parallel_tool_calls`、`reasoning`、`include`、`text`、`store`、`client_metadata`、`prompt_cache_key`。
- Codex `local_shell` tool 会转成 Anthropic tool，Provider 返回 `tool_use` 后再映射回 OpenAI `local_shell_call`。
- Codex 下一轮的 `local_shell_call_output` 会转成 Anthropic `tool_result`。
- Codex 默认附带的 OpenAI 原生内置工具（例如 `web_search`）会被转发层忽略，不会传给 Anthropic Provider；需要搜索能力时应后续单独实现 Anthropic 侧可用工具。
- Moon Bridge 只负责模型协议转发，不在服务端执行 shell；shell 执行仍由 Codex 客户端完成。

## 官方资料

- Anthropic Messages API：https://docs.anthropic.com/en/api/messages
- Anthropic Messages examples：https://platform.claude.com/docs/en/api/messages-examples
- Anthropic tool use：https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/overview
- Anthropic streaming：https://docs.anthropic.com/en/docs/build-with-claude/streaming
- Anthropic prompt caching：https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
- OpenAI Responses API reference：https://platform.openai.com/docs/api-reference/responses/create
- OpenAI Response object：https://platform.openai.com/docs/api-reference/responses/object
- OpenAI streaming responses：https://platform.openai.com/docs/guides/streaming-responses
- OpenAI streaming events：https://platform.openai.com/docs/api-reference/responses-streaming
- OpenAI prompt caching：https://platform.openai.com/docs/guides/prompt-caching
