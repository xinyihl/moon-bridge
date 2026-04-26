# 开发提示

## 环境配置

### 配置文件

项目使用 YAML 配置。参考 `config.example.yml` 创建 `config.yml`：

```bash
cp config.example.yml config.yml
# 编辑 config.yml，填入真实的 Provider base_url 和 api_key
```

`config.yml` 已在 `.gitignore` 中，不会提交到仓库。也可通过 `MOONBRIDGE_CONFIG` 环境变量指定其他路径。

### 文件结构

```
.
├── cmd/moonbridge/          # 命令行入口
├── internal/
│   ├── app/                 # 应用组装
│   ├── config/              # YAML 配置解析
│   ├── bridge/              # 协议转换核心
│   ├── extensions/          # Provider 扩展（如 DeepSeek V4）
│   ├── cache/               # Prompt cache 规划
│   ├── anthropic/           # Anthropic Messages 客户端
│   ├── openai/              # OpenAI Responses DTO
│   ├── server/              # HTTP 服务器
│   ├── proxy/               # 透明代理
│   ├── trace/               # 请求/响应转储
│   └── e2e/                 # 端到端测试
├── helloagents/             # 知识库
├── docs/                    # 文档
├── scripts/
│   ├── start_codex_with_moonbridge.sh     # Transform / CaptureResponse 模式
│   └── start_claude_code_with_moonbridge.sh  # CaptureAnthropic 模式
├── go.mod
└── config.example.yml
```

### 启动服务

```bash
# 构建并启动 Transform 模式
go build -o moonbridge ./cmd/moonbridge
./moonbridge

# 指定配置文件
./moonbridge -config /path/to/config.yml

# 覆盖地址和模式
./moonbridge -addr 0.0.0.0:8080 -mode CaptureResponse

# 打印配置信息供脚本使用
./moonbridge -print-addr
./moonbridge -print-codex-model
./moonbridge -print-codex-config moonbridge
```

启动脚本 `scripts/start_codex_with_moonbridge.sh` 和 `scripts/start_claude_code_with_moonbridge.sh` 会自动构建二进制、管理服务进程生命周期，并设置临时 `CODEX_HOME` / `CLAUDE_CONFIG_DIR`。
Codex 脚本会从 `${MOONBRIDGE_CODEX_CONFIG:-$HOME/.codex/config.toml}` 复制 `[tui].status_line` 到 `FakeHome/Codex/config.toml`，但不会改动全局配置。

## 测试

### 运行单元测试

```bash
go test ./...
```

### 运行端到端测试

E2E 测试需要有效的 `config.yml`（包含真实 Provider API Key）：

```bash
go test ./internal/e2e/ -v -count=1
```

### 测试要点

- 非流式请求：验证 `FromAnthropicWithPlanAndContext()` 在各类停止原因、缓存 usage、多工具调用场景下的正确性。
- 流式请求：验证 `ConvertStreamEventsWithContext()` 的事件顺序、item ID 前缀、custom tool 和 web search 的特殊 delta 事件。
- 历史转换：验证 `convertInput()` 中连续工具调用的归并逻辑，以及 `output_text` 的压缩。
- Codex 兼容：验证空 `cached_tokens` 序列化、namespace 展平、`web_search_call` 过滤、custom grammar 保留。
- Cache planner：验证各种配置组合（off / automatic / explicit / hybrid）下的断点注入与注册表状态管理。
- DTO：验证 `input_tokens_details.cached_tokens` 在值为 `0` 时仍被序列化。
- DeepSeek V4：验证 reasoning_content 剥离、thinking 记忆/重注入、effort 映射、流式 delta 收集等逻辑。

## Debug

### 启用请求追踪

在 `config.yml` 中设置 `trace_requests: true`，所有请求和响应会被写入 `trace/` 目录。详见 [architecture.md](architecture.md) trace 模块说明。

### 测试用例编写风格

使用表格驱动测试（table-driven tests）。对于协议转换的断言，优先对比整个请求/响应对象，而非逐字段断言。

## 配置变更

本项目仍在开发中，不需要保留旧配置兼容性。配置结构变更时：

1. 更新 `internal/config/config.go` 中的 FileConfig 和 FromFileConfig。
2. 更新 `config.example.yml`。
3. 更新启动脚本（如适用）。
4. 更新本目录下的相应文档。

## 变更日志

所有实质性变更需记录在 `helloagents/CHANGELOG.md`。

## 工具映射备忘

- `namespace` 下的 `function` 子工具发往 Anthropic 时展平为 `namespace__tool`，如 `mcp__deepwiki__ask_question`；响应回 Codex 时必须拆为 `namespace:"mcp__deepwiki__"` + `name:"ask_question"`，否则 Codex 不能解析为 MCP 调用。
- `namespace` 下的 `custom` 子工具同样展平为 `namespace__tool`，保留 grammar 信息。
- 查询 Codex 内部工具实现必须优先走 DeepWiki；当前确认需要 grammar/freeform 的内置 custom 工具主要是 `apply_patch` 和 Code Mode `exec`。
- `apply_patch` 不直接暴露 raw grammar 给 Anthropic，而是拆成 `apply_patch_add_file`、`apply_patch_delete_file`、`apply_patch_update_file`、`apply_patch_replace_file`、`apply_patch_batch` 一组结构化 schema，响应回 Codex 前统一拼回 raw patch grammar；proxy 描述不能包含 Codex 原始 `FREEFORM` / grammar 提示，避免和 JSON schema 冲突。`replace_file` / `update_file + content` 代表整文件替换，会拼成 `Delete File` + `Add File`，不要生成空 `Update File` hunk。
- Code Mode `exec` 转换成 `{source: string}` schema，响应回 Codex 前再把 `source` 原样作为 custom tool input；proxy 描述也不要暴露 raw grammar。
- MCP / DeepWiki 的使用偏好写在 `AGENTS.md`，不要写进 Transform 转换层；转换层只做协议映射，不注入项目特定提示词。
- `web_search` 桥接使用 Anthropic `web_search_20250305` server tool，不被当成普通 function 处理；`provider.web_search.support:auto` 会在 Transform 启动时用流式请求探测 Provider 是否接受该工具，只有探测证明可用才注入。
- Provider 可能返回空 `text_delta` 紧挨工具调用；流式转换必须忽略空文本增量，历史转换也必须跳过空 `output_text`，否则下一轮 Anthropic 请求会出现缺少 `text` 字段的非法 text block。
- `file_search`、`computer_use_preview`、`image_generation` 目前直接忽略。
- `local_shell` 使用独立 schema 和 output item，不走 `function_call` 路径。

## 当前实测结论

缓存配置组合 `mode: automatic` + `prompt_caching: true` + `automatic_prompt_cache: true` + `explicit_cache_breakpoints: true` 在当前 Provider / `claude-opus-4-6` / Codex 请求形态下，第 2 轮可达到基本全输入缓存命中。该结论仅限于当前测试环境；若后续 `cache_read_input_tokens` 长期为 0 或成本异常，应回退到 `mode: explicit` + `automatic_prompt_cache: false`。

## 依赖

- Go 1.22+（项目使用 `range-over-int` 等新特性）
- `gopkg.in/yaml.v3` — YAML 配置解析
- 无其他外部依赖
