# Plugin System Design

## Overview

Moon Bridge 的插件系统将当前的 `extension.Hook` 接口重构为一套更通用、可组合的插件架构。
目标是让所有非核心功能（DeepSeek V4 扩展、Web Search 注入、未来的缓存策略、日志增强等）
都能以插件形式实现，而主网桥代码只包含协议转换的核心逻辑。

## 核心概念

### Plugin（插件）

```go
// Plugin 是所有插件的基础接口。
type Plugin interface {
    // Metadata
    Name() string
    Version() string
    Description() string

    // Lifecycle
    Init(ctx PluginContext) error
    Shutdown() error

    // Enablement — 插件可以声明自己在哪些维度上激活。
    // 返回 nil 表示该维度不限制（始终激活）。
    EnabledForModel(modelAlias string) bool
}
```

### PluginContext（插件上下文）

```go
// PluginContext 提供插件初始化时所需的环境信息。
type PluginContext struct {
    Config     map[string]any       // 插件自身的配置（从 config.yml 的 plugins.<name> 读取）
    AppConfig  config.Config        // 只读的全局配置引用
    Logger     *slog.Logger         // 带插件名前缀的 logger
}
```

### Capability Interfaces（能力接口）

插件通过实现零个或多个能力接口来声明自己参与哪些管道阶段。
Registry 在注册时通过类型断言检测插件实现了哪些接口，只在对应阶段调用。

```go
// --- 请求管道 ---

// InputPreprocessor 在 JSON 解析前转换原始输入。
type InputPreprocessor interface {
    PreprocessInput(ctx RequestContext, raw json.RawMessage) json.RawMessage
}

// RequestMutator 在 Anthropic 请求构建完成后修改它。
type RequestMutator interface {
    MutateRequest(ctx RequestContext, req *anthropic.MessageRequest)
}

// ToolInjector 向请求注入额外的工具定义。
type ToolInjector interface {
    InjectTools(ctx RequestContext) []anthropic.Tool
}

// MessageRewriter 在消息列表构建过程中重写消息。
type MessageRewriter interface {
    RewriteMessages(ctx RequestContext, messages []anthropic.Message) []anthropic.Message
}

// --- 提供者管道 ---

// ProviderWrapper 包装上游 provider client。
// 用于实现 server-side tool execution（如 injected web search）。
type ProviderWrapper interface {
    WrapProvider(ctx RequestContext, client Provider) Provider
}

// --- 响应管道 ---

// ContentFilter 过滤/转换响应中的 content block。
type ContentFilter interface {
    FilterContent(ctx RequestContext, block anthropic.ContentBlock) (skip bool, extra []openai.OutputItem)
}

// ResponsePostProcessor 在响应转换完成后修改最终输出。
type ResponsePostProcessor interface {
    PostProcessResponse(ctx RequestContext, resp *openai.Response)
}

// --- 流式管道 ---

// StreamInterceptor 拦截流式事件。
type StreamInterceptor interface {
    NewStreamState() any
    OnStreamEvent(ctx StreamContext, event anthropic.StreamEvent) (consumed bool, emit []openai.StreamEvent)
    OnStreamComplete(ctx StreamContext)
}

// --- 错误处理 ---

// ErrorTransformer 重写上游错误消息。
type ErrorTransformer interface {
    TransformError(ctx RequestContext, err error) error
}

// --- 会话管理 ---

// SessionStateProvider 提供 per-session 状态。
type SessionStateProvider interface {
    NewSessionState() any
}
```

### RequestContext（请求上下文）

```go
// RequestContext 携带单次请求的上下文信息。
type RequestContext struct {
    ModelAlias   string
    SessionData  map[string]any  // per-session plugin state
    RequestOpts  RequestOptions  // web search mode, etc.
    Reasoning    map[string]any  // OpenAI reasoning config
}
```

### StreamContext（流式上下文）

```go
// StreamContext 扩展 RequestContext，增加流式状态。
type StreamContext struct {
    RequestContext
    StreamState  any             // 该插件的 per-stream state
    BlockIndex   int
    EventType    string          // "start", "delta", "stop"
}
```

## 配置 Schema

插件在 `config.yml` 中通过 `plugins` 块配置：

```yaml
plugins:
  deepseek_v4:
    # 无额外配置，启用由 model 的 deepseek_v4: true 控制

  web_search_injected:
    tavily_api_key: "tvly-..."
    firecrawl_api_key: "fc-..."
    max_rounds: 5

  # 未来插件示例
  prompt_cache:
    strategy: "aggressive"
    ttl: 3600

  rate_limiter:
    rpm: 60
    tpm: 100000
```

## Registry 改进

```go
type Registry struct {
    plugins            []Plugin
    inputPreprocessors []InputPreprocessor
    requestMutators    []RequestMutator
    toolInjectors      []ToolInjector
    messageRewriters   []MessageRewriter
    providerWrappers   []ProviderWrapper
    contentFilters     []ContentFilter
    responsePostProcs  []ResponsePostProcessor
    streamInterceptors []StreamInterceptor
    errorTransformers  []ErrorTransformer
    sessionProviders   []SessionStateProvider
}

func (r *Registry) Register(p Plugin) {
    r.plugins = append(r.plugins, p)
    // 类型断言分发到各能力列表
    if v, ok := p.(InputPreprocessor); ok {
        r.inputPreprocessors = append(r.inputPreprocessors, v)
    }
    // ... 其他能力接口
}
```

## 与当前系统的对比

| 维度 | 当前 Hook 系统 | 新 Plugin 系统 |
|------|---------------|---------------|
| 接口粒度 | 单一大接口（20+ 方法） | 多个小能力接口（每个 2-3 方法） |
| 启用控制 | 仅 per-model | per-model + per-request + 配置 |
| 工具注入 | 不支持（硬编码） | `ToolInjector` 接口 |
| Provider 包装 | 不支持（硬编码） | `ProviderWrapper` 接口 |
| 配置 | 无 schema | 插件声明自己的配置 |
| 生命周期 | 无 | Init/Shutdown |
| 流式处理 | 6 个分散方法 | 统一 `OnStreamEvent` |
| 类型安全 | `any` 到处传 | 插件自己管理强类型状态 |
| 注册时检测 | 运行时全调用 | 注册时类型断言，只调用实现了的 |

## 迁移路径

### Phase 1: 定义新接口（不破坏现有代码）
- 新建 `internal/extension/plugin/` 包
- 定义 `Plugin` + 所有能力接口
- 定义 `Registry` 新实现
- 定义 `PluginContext`, `RequestContext`, `StreamContext`

### Phase 2: 迁移 DeepSeek V4
- 实现 `deepseek_v4.Plugin`，实现 `InputPreprocessor`, `RequestMutator`,
  `MessageRewriter`, `ContentFilter`, `StreamInterceptor`, `ErrorTransformer`,
  `SessionStateProvider`
- 删除旧 `extension.Hook` 实现

### Phase 3: 迁移 Web Search Injected
- 实现 `websearch_injected.Plugin`，实现 `ToolInjector`, `ProviderWrapper`
- 从 `request.go` 和 `server.go` 移除硬编码调用

### Phase 4: 清理
- 删除 `internal/extension/` 包
- 更新 `bridge.go` 使用新 `plugin.Registry`
- 更新文档

## 未来可扩展的插件类型

- **PromptCache**: 实现 `RequestMutator`（注入 cache_control）+ `ResponsePostProcessor`（记录缓存命中）
- **RateLimiter**: 实现 `ProviderWrapper`（包装 client 加限流）
- **CostTracker**: 实现 `ResponsePostProcessor`（记录 token 消耗和费用）
- **AutoRetry**: 实现 `ErrorTransformer` + `ProviderWrapper`（自动重试可恢复错误）
- **ContentGuard**: 实现 `InputPreprocessor`（过滤敏感内容）+ `ContentFilter`（过滤响应）
- **ModelRouter**: 实现 `RequestMutator`（根据请求内容动态选择模型）
