# Anthropic Messages to OpenAI Responses Bridge

A protocol translation layer that exposes an OpenAI Responses-compatible API (`POST /v1/responses`) while forwarding requests to any Anthropic Messages-compatible provider. The bridge handles request/response mapping, tool call translation, prompt caching, streaming, and error normalization.

Supported clients include [Codex CLI](https://github.com/openai/codex), custom toolchains, and any application built against the OpenAI Responses API that needs to route through an Anthropic Messages provider.

## Architecture

```
OpenAI Responses Client
        |
        | POST /v1/responses
        v
HTTP Handler (/v1/responses, /responses)
  - Request parsing & auth check
  - Trace recording (optional)
        |
        v
Bridge (Protocol Translator)
  - Request:  OpenAI Responses → Anthropic Messages
  - Response: Anthropic Message → OpenAI Response object
  - Stream:   Anthropic SSE → OpenAI Responses SSE events
  - Tool:     OpenAI function/custom → Anthropic tool + reverse
  - Cache:    CacheCreationPlanner + cache_control injection
  - Error:    Provider errors → OpenAI-style error objects
        |
        v
Provider Client
  - POST /v1/messages (non-streaming)
  - POST /v1/messages?stream=true (streaming)
  - Auth, retry, timeout, User-Agent
```

Package layout (Go module `moonbridge`):

```
internal/openai        OpenAI Responses DTOs and SSE event types
internal/anthropic     Anthropic Messages DTOs and HTTP client
internal/bridge        Protocol conversion, error mapping, streaming state machine
internal/cache         Cache creation planner, breakpoint injection, usage normalization
internal/config        YAML config loading and validation
internal/server        HTTP handlers for /v1/responses and /responses
internal/proxy         Transparent proxy modes (CaptureResponse, CaptureAnthropic)
internal/trace         Request/response dump to local filesystem
internal/app           Application assembly and lifecycle
internal/e2e           Real provider end-to-end tests
```

## Configuration

YAML-based, documented via [config.example.yml](/home/zhiyi/Projects/misc/MoonBridge/config.example.yml). The config file is loaded from `config.yml` by default, overridable via the `MOONBRIDGE_CONFIG` environment variable. Sensitive credentials and local overrides go into `.gitignore`-d `config.yml`; the example file is always checked in.

### Modes

| Mode | Purpose |
| --- | --- |
| `Transform` | Protocol translation: OpenAI Responses in, Anthropic Messages out. The primary use case. |
| `CaptureResponse` | Transparent proxy that captures real OpenAI Responses API traffic without conversion. Use for protocol alignment testing. |
| `CaptureAnthropic` | Transparent proxy that captures real Anthropic Messages API traffic without conversion. Use for understanding native client behavior. |

## Request Mapping

| OpenAI Responses Field | Anthropic Messages Field | Handling |
| --- | --- | --- |
| `model` | `model` | Alias mapping via config; model names are not hardcoded. |
| `instructions` | `system` | Highest-priority system instruction; developer role input prepended. |
| `input` (string) | `messages[0].content` | Single user text message. |
| `input[].role=user` | `messages[].role=user` | Text blocks passed through; images converted if provider supports it. |
| `input[].role=assistant` | `messages[].role=assistant` | Text content and tool_use blocks from history. |
| `function_call_output` / tool outputs | `role=user` + `tool_result` | `call_id` → `tool_use_id`. |
| `max_output_tokens` | `max_tokens` | Default injected from config when absent. |
| `temperature` / `top_p` | Same-named | Passed through directly; unsupported parameters return error. |
| `stop` | `stop_sequences` | Normalized to string array. |
| `stream` | `stream` | Passed through; switches SSE converter. |
| `tool_choice:"auto"` | `tool_choice:auto` or omitted | Prefer native auto. |
| `tool_choice:"none"` | `tool_choice:none` or omit tools | If provider doesn't support none, tools are omitted. |
| `tool_choice:"required"` | `tool_choice:any` | Any tool must be called. |
| `tool_choice:{function:{name}}` | `tool_choice:{type:"tool",name}` | Named tool. |

### Input Normalization

1. `input` is parsed as a string or array of items.
2. `system`/`developer` role items are extracted into Anthropic's top-level `system` field.
3. `user`/`assistant` message order is preserved without cross-turn merging.
4. Multi-modal content (`input_text`, `input_image`) is processed according to the provider capability profile.
5. `previous_response_id` requires `store=true` and active local storage; otherwise returns 400.

### Custom Tools and Codex Compatibility

When bridging Codex CLI traffic, the bridge handles OpenAI `custom` grammar tools that cannot be treated as plain JSON functions:

| Instruction Kind | Anthropic Schema | Reverse Mapping |
| --- | --- | --- |
| `apply_patch` | Structured `operations` array with paths, hunks, and line operations | Reconstructed into `*** Begin Patch` / `*** End Patch` raw grammar, with normalization of trailing markers (`+*** End Patch` → `*** End Patch`). |
| `exec` (Code Mode) | `{source: string}` | `source` field returned as raw custom tool input. |
| Other custom / freeform | `{input: string}` | Raw input string extracted from `input` field. |

`namespace` tools are flattened into `namespace__tool` naming for Anthropic. On the response side, `custom_tool_call` items are emitted with `response.custom_tool_call_input.delta` streaming events.

### Web Search Bridging

OpenAI `web_search`/`web_search_preview` tools are converted to Anthropic `web_search_20250305` server tools. On the response side, `server_tool_use:web_search` is mapped back to Codex `web_search_call` output items. Empty search results and preamble messages (`Search results for query:`) are filtered to avoid polluting conversation history.

### History Consolidation

When converting Codex conversation history with consecutive tool calls, the bridge:

1. Merges consecutive `function_call` / `local_shell_call` items into a single Anthropic assistant `tool_use` message.
2. Merges consecutive tool outputs into a single user `tool_result` message.
3. This prevents upstream providers from rejecting requests due to unmatched `tool_calls`/`tool_messages`.

## Prompt Caching

Prompt caching is a first-class concern in this bridge because OpenAI's caching is automatic while Anthropic's requires explicit `cache_control` markers. A naive field-by-field translation would silently break caching.

### Strategy Modes

| Mode | Behavior | Use Case |
| --- | --- | --- |
| `off` | No `cache_control` injected. | Strict no-cache or provider doesn't support it. |
| `automatic` | Top-level request `cache_control:{type:"ephemeral"}` added. | Multi-turn conversations where the cache breakpoint shifts with history. |
| `explicit` | `cache_control` placed on the last stable tool, system block, or message content block. | Large system prompts, tool definitions, long documents, example sets. |
| `hybrid` | Both top-level and block-level `cache_control`. | Simultaneously caching tools/system and growing conversation history. |

The config has two independent controls:

- `automatic_prompt_cache`: controls top-level request `cache_control`.
- `explicit_cache_breakpoints`: controls block-level breakpoints on tools/system/messages.

When both are on with `mode: automatic`, the planner effectively produces a hybrid plan.

### Cache Creation Plan

Anthropic has no separate "create cache" API. Cache is created as part of a regular `messages.create` request when `cache_control` markers are present. The `CacheCreationPlanner` runs before every forwarded request.

**Planner Input:**

- `model` (OpenAI alias and resolved provider model)
- `prompt_cache_key` (bridge-local routing hint, never sent to provider)
- `prompt_cache_retention` → mapped to TTL (`in_memory` → `5m`, `24h` → `1h` with downgrade opt-in)
- Hashes of `tools`, `system`, `messages` (canonical JSON → SHA-256)
- Estimated token count for threshold checks
- Local cache registry state (warm / warming / expired / not_cacheable)

**Planner Output:**

```go
type CacheCreationPlan struct {
    Mode        string            // off, automatic, explicit, hybrid
    TTL         string            // 5m, 1h
    LocalKey    string            // SHA-256 of all stable fingerprint components
    Breakpoints []CacheBreakpoint // scope: tools, system, messages
    WarmPolicy  string            // none, leader, background
    Reason      string
}
```

**Decision Flow:**

1. Provider capability check → skip if prompt caching is disabled.
2. Stability check → exclude timestamps, request IDs, random values, latest user message from cache prefix.
3. Token threshold check → skip if estimated tokens are below `min_cache_tokens`.
4. Value check → skip if `estimated_tokens * expected_reuse` is below `minimum_value_score`.
5. Registry check → if already warm, re-inject same breakpoints for read hit.
6. Breakpoint selection → prefer `tools` → `system` → `messages` stable prefix, max 4 breakpoints.

**Breakpoint Placement:**

| Prefix Pattern | Breakpoint Position | Notes |
| --- | --- | --- |
| Large tool set + short system | Last tool definition | Stable across sessions |
| Long system prompt | Last system text block | `system` must be block array |
| Long document as first user message | Last text/image block in first user message | Follow-up questions after breakpoint |
| Multi-turn session | Last stable history message | Latest user question excluded |
| Post-tool-call continuation | After last stable `tool_result` | Tool results vary per call |

**Injection Algorithm:**

1. Build Anthropic request without cache fields.
2. Canonical JSON-encode and hash `tools`, `system`, `messages`.
3. Run `CacheCreationPlanner` to get `CacheCreationPlan`.
4. For `automatic`/`hybrid`: set `request.CacheControl = {type:"ephemeral", ttl:"1h"}`.
5. For `explicit`/`hybrid`: set `block.CacheControl` on selected tool/system/message blocks.
6. Re-encode request and log `request_fingerprint` for hit-rate analysis.

### Usage Normalization

When caching is active, Anthropic's `usage.input_tokens` only represents fresh input after the last cache breakpoint. The bridge normalizes to OpenAI expectations:

```text
openai.usage.input_tokens =
  anthropic.usage.cache_read_input_tokens
  + anthropic.usage.cache_creation_input_tokens
  + anthropic.usage.input_tokens

openai.usage.input_tokens_details.cached_tokens =
  anthropic.usage.cache_read_input_tokens
```

Provider-level breakdowns (`cache_creation_input_tokens`, `cache_creation.ephemeral_*`) are preserved in `response.metadata.provider_usage` for cost analysis. Note that `cached_tokens` is always serialized, even when zero, to avoid Codex parsing errors.

### Concurrency & Warming

- **Singleflight**: Only one "leader" request writes cache for a given `local_cache_key`; followers either wait for the first upstream response event or forward directly.
- **Background warm**: Optional; sends a minimal request (e.g., "reply OK only") with cache markers to pre-warm. This still consumes tokens.
- **Registry**: In-memory, recording `local_cache_key`, breakpoint hashes, TTL, timestamps, and recent usage signals. No prompt text is stored.

### Result Determination

Cache effectiveness is determined from Anthropic usage signals, not from the presence of `cache_control` in the request:

| Usage Signal | Cache Registry Update |
| --- | --- |
| `cache_creation_input_tokens > 0` | `warm` — write succeeded, update expires_at |
| `cache_read_input_tokens > 0` | `warm` — read hit, accumulate hit rate |
| Both 0, low `input_tokens` | `not_cacheable` — likely below threshold |
| Both 0, sufficient tokens | `missed` — prefix instability suspected |
| Provider cache parameter error | `failed` — retry without cache fields |

## Tool Call Mapping

| OpenAI | Anthropic | Notes |
| --- | --- | --- |
| `{type:"function", name, description, parameters}` | `{name, description, input_schema}` | `parameters` must be a JSON Schema object. |
| `{type:"local_shell"}` | `{name:"local_shell", ...}` | Codex `local_shell_call` ↔ `tool_use`. Command, working_directory, timeout_ms, env. |
| `{type:"custom"}` with grammar | Structured JSON schema per grammar kind | `apply_patch` → operations array; `exec` → source string. |
| `namespace` | Flattened `namespace__tool` | Child functions/customs expanded with namespace prefix. |
| `web_search_preview` | `{type:"web_search_20250305"}` | Max uses from config. |
| `file_search`, `computer_use_preview`, `image_generation` | Skipped | Silently ignored in tool declarations. |

### Response Side

Anthropic `tool_use` → OpenAI response items:

| Anthropic | OpenAI |
| --- | --- |
| `text` block | `output[].type="message"` with `output_text` content parts |
| `tool_use` (function) | `output[].type="function_call"` with `call_id`, `name`, `arguments`, `status` |
| `tool_use` (local_shell) | `output[].type="local_shell_call"` with structured `action` |
| `tool_use` (custom) | `output[].type="custom_tool_call"` with grammar-reconstructed `input` |
| `server_tool_use:web_search` | `output[].type="web_search_call"` with `action` (filtered if empty) |

### Next Turn

OpenAI `function_call_output` items → Anthropic `tool_result` blocks in a `role=user` message.

## Streaming

Anthropic SSE events are converted to OpenAI Responses SSE via a state machine that tracks content index, output index, item IDs, and sequence numbers.

| Anthropic Event | OpenAI Responses SSE Events |
| --- | --- |
| `message_start` | `response.created` → `response.in_progress` |
| `content_block_start` (text) | `response.output_item.added` (message) → `response.content_part.added` (output_text) |
| `content_block_delta` (text_delta) | `response.output_text.delta` |
| `content_block_stop` (text) | `response.output_text.done` → `response.content_part.done` → `response.output_item.done` |
| `content_block_start` (tool_use) | `response.output_item.added` (function_call / local_shell_call / custom_tool_call) |
| `content_block_delta` (input_json_delta) | `response.function_call_arguments.delta` | `response.custom_tool_call_input.delta` (custom tools) | web search JSON accumulated internally |
| `content_block_stop` (tool_use) | `response.function_call_arguments.done` → `response.output_item.done` |
| `message_delta` | Updates aggregated response usage and status |
| `message_stop` | `response.completed` or `response.incomplete` |
| `error` | `response.failed` |
| `ping` | Ignored or forwarded as comment frame |

SSE invariants:

- Every event carries a monotonically increasing `sequence_number`.
- Text delta events include `item_id`, `output_index`, and `content_index`.
- Final usage is not emitted until `message_stop`.
- Web search and custom tool streaming events for `input_json_delta` are accumulated internally and emitted at `content_block_stop`.
- Provider connection failure produces `response.failed` and closes the SSE connection.

## Stop Reason Mapping

| Anthropic | OpenAI Status | Incomplete Details |
| --- | --- | --- |
| `end_turn` | `completed` | — |
| `tool_use` | `completed` | — |
| `stop_sequence` | `completed` | — |
| `max_tokens` | `incomplete` | `{reason:"max_output_tokens"}` |
| `model_context_window` | `incomplete` | `{reason:"max_input_tokens"}` |
| `refusal` | `completed` | — (`refusal` content part emitted) |
| `pause_turn` | `incomplete` | `{reason:"provider_pause"}` |

## Error Mapping

| Scenario | HTTP Status | OpenAI Error Code |
| --- | --- | --- |
| Auth failure | 401 | `invalid_api_key` |
| Permission / model unavailable | 403 | `model_not_found` / `permission_denied` |
| Unsupported field | 400 | `unsupported_parameter` |
| Invalid JSON schema | 400 | `invalid_request_error` |
| Context exceeded | 400 / 413 | `context_length_exceeded` |
| Provider rate limit | 429 | `rate_limit_exceeded` |
| Provider 5xx | 502 | `provider_error` |
| Provider timeout | 504 | `provider_timeout` |

Error responses use the standard OpenAI format:

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

## Operational Notes

### Trace Recording

When `trace_requests: true`, the bridge dumps request/response pairs to the local filesystem for debugging. Traces are organized by mode and session:

- `Transform`: `trace/Transform/{session_id}/Response/{n}.json` + `Anthropic/{n}.json`
- `Capture`: `trace/Capture/{Response|Anthropic}/{session_id}/{n}.json`

API keys are redacted. Trace paths are in `.gitignore`.

### Proxy Modes

The two capture modes run a transparent HTTP proxy that forwards requests to the upstream provider without protocol conversion:

- **CaptureResponse**: Records OpenAI Responses API native traffic. Auth is overridden by proxy config but `User-Agent` passes through.
- **CaptureAnthropic**: Records Anthropic Messages API native traffic. Useful for capturing Claude Code's actual request patterns to inform Transform mode defaults.

### Capability Profile

Provider capabilities are declared in `config.yml`:

```yaml
cache:
  mode: "explicit"           # off / automatic / explicit / hybrid
  ttl: "5m"                  # 5m / 1h
  prompt_caching: true
  automatic_prompt_cache: false
  explicit_cache_breakpoints: true
  allow_retention_downgrade: false
  max_breakpoints: 4
```

All unsupported-but-requested capabilities produce explicit errors — no silent degradation.

### Security

- Provider API keys are never exposed to the client; client and upstream keys are configured separately.
- Logs redact `Authorization` headers, tool argument key-like fields, and image/file base64 content.
- Request/response traces include trace IDs but not full prompts (unless audit mode is on).
- `.gitignore` covers `config.yml`, `config.yaml`, `helloagents/`, `.codex`, `.claude`, `AGENTS.md`, `CLAUDE.md`, `trace/`, and `FakeHome/`.

## Implementation Status

- DTO definitions for OpenAI Responses and Anthropic Messages (request, response, streaming events, errors)
- Non-streaming text request/response conversion
- Streaming conversion with SSE state machine (text, tool_use delta, lifecycle events)
- Function tool schema mapping and tool call bridging
- Codex-specific tool support: `local_shell`, `custom` (apply_patch, exec), `namespace`, `web_search`
- Codex conversation history consolidation (consecutive tool calls → merged Anhtropic rounds)
- Prompt cache planner with automatic/explicit/hybrid breakpoint strategies
- `cache_control` injection and usage normalization
- Cache registry (in-memory) with state tracking and concurrency leader/follower pattern
- Error mapping (OpenAI-style errors from provider errors)
- Trace recording (per-mode, per-session, per-request-number)
- Transparent proxy modes (CaptureResponse, CaptureAnthropic)
- Config schema validation
- Real provider end-to-end tests

### Known Gaps

- No OpenAI built-in tools (`web_search`, `file_search`, `computer_use`, `code_interpreter` — only `web_search` is bridged to Anthropic server tools)
- No file ID resolution or background response support
- No `previous_response_id` / `response.store` persistence
- No real token counter; cache thresholds use rough estimation (`len(json)/4`)
- Cache registry is in-memory only; no persistence across restarts
- No background cache warming worker
- 24h retention not supported by default; requires `allow_retention_downgrade` opt-in

## References

- [Anthropic Messages API](https://docs.anthropic.com/en/api/messages)
- [Anthropic Tool Use](https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/overview)
- [Anthropic Streaming](https://docs.anthropic.com/en/docs/build-with-claude/streaming)
- [Anthropic Prompt Caching](https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching)
- [OpenAI Responses API](https://platform.openai.com/docs/api-reference/responses/create)
- [OpenAI Responses Object](https://platform.openai.com/docs/api-reference/responses/object)
- [OpenAI Streaming Responses](https://platform.openai.com/docs/guides/streaming-responses)
- [OpenAI Prompt Caching](https://platform.openai.com/docs/guides/prompt-caching)
- [Codex CLI](https://github.com/openai/codex)
