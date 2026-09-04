# dlz-goai 嵌入式集成架构设计

> 状态：设计草案，尚未实施。
>
> 本文定义 `dlz-goai` 下一阶段的目标架构和公共 API。当前代码继续按现有 API 工作；本文不表示相关接口已经可用。

## 1. 背景

`dlz-goai` 已经具备模型调用、Tool、Agent Loop、MCP Tool Client、持久化 Runtime、审批、事件回放以及 RAG 基础能力。下一阶段不以继续堆叠具体实现为首要目标，而是建立一套稳定、简洁、可长期扩展的嵌入式集成 API，让应用能够：

- 在启动阶段装配模型、Runtime、Runner、Tool、RAG、MCP 和持久化组件；
- 为不同会话选择不同的已注册能力；
- 使用默认配置快速开始，同时能够替换任意具体实现；
- 在前端断开、页面切换或进程重启后继续查询、回放和恢复 Run；
- 在未来接入 Graph、Memory、Guardrail、Cache 等能力时，不推翻顶层调用方式。

本库的目标不是成为依赖注入容器，也不是把所有 AI 能力都实现在核心包中。核心负责统一语义和运行骨架；应用负责创建和装配具体组件。

## 2. 设计目标

### 2.1 核心定位

`dlz-goai` 的目标定位是：

> 一个具备合理默认值、可嵌入、可组合、可持久化和可恢复的轻量 Go AI Runtime。

核心由五部分组成：

1. 稳定契约：Message、Tool Call、LLM、Runner、Store、RAG、MCP 等接口和数据模型；
2. 运行骨架：Agent Loop、Run 状态机、事件、审批、恢复、回放和取消；
3. 扩展桩点：Runner、Middleware、Context Provider、Event Handler 等；
4. 集成 API：`Config`、`Client`、`Run` 以及可选的 `Session`；
5. 最小默认实现：默认 Runner、内存 Store、Tool Registry 和事件机制。

### 2.2 非目标

第一阶段不做以下事情：

- 不提供独立 HTTP AI 服务；
- 不实现完整 Graph 或多 Agent 编排系统；
- 不把所有 Provider、数据库和向量库放进核心；
- 不缓存所有会话历史；
- 不允许扩展改变 Run 状态、事件顺序等核心不变量；
- 不要求应用使用框架自带的配置文件格式或依赖注入方式。

## 3. 设计原则

### 3.1 核心定义语义，扩展提供实现

核心必须掌握：

- Message 和 Tool Call 协议；
- Run 状态机；
- Event 模型及序列语义；
- Agent Loop；
- Resume、Cancel、Replay 和 Subscribe 语义；
- 配置合并规则；
- 组件生命周期规则；
- 统一错误模型。

扩展可以替换：

- LLM Provider；
- Run、Event、Checkpoint 和 Session Store；
- Tool 和 Tool Registry；
- MCP Transport；
- Chunker、Embedder、Vector Store、Retriever 和 Reranker；
- Runner 和未来的 Graph Executor；
- 审批、权限、日志、Tracing 和 Metrics。

### 3.2 默认简单，逐层展开

期望的使用分布是：

- 大多数用户只使用 `DefaultConfig`、`NewClient` 和 `Run`；
- 需要定制的用户使用 Preset 和 `WithXXX`；
- 少数高级用户实现底层契约。

### 3.3 应用装配，前端选择

应用在启动阶段创建真实组件并按名称注册。前端只能选择应用已经注册且允许使用的组件，不能直接提交数据库连接、API Key、MCP 命令、任意 URL 或 Go 对象。

### 3.4 持久化是真实来源

Session、Message、Run、Event 和 Checkpoint 的持久化数据是唯一真实来源。内存只保存当前 Run 的工作上下文和实时控制信息。缓存是可选优化，不能成为正确性前提。

### 3.5 预设没有内部特权

Preset 只是一组公开 Option 的组合。官方预设、自定义预设和应用手工装配使用完全相同的契约和路径。

## 4. 对外对象模型

公开模型收敛为：

```text
Config
  │ NewClient
  ▼
Client
  ├── Run
  ├── Resume
  ├── Cancel
  ├── Replay
  ├── Subscribe
  └── Session（可选轻量句柄）
          │
          └── Run / Resume / Cancel / Replay / Subscribe
```

内部仍然存在 Runtime、Registry、事件总线和活跃 Run 管理，但不额外公开 `Engine` 概念。

### 4.1 Config

`Config` 是应用级装配方案，包含已经初始化好的组件、注册名称、默认选择和全局策略。它不是会话状态，也不保存历史消息。

建议 API：

```go
cfg := goai.DefaultConfig().
    WithModel("qwen3", model).
    WithRunner("agent", agent.DefaultRunner()).
    WithRAG("documents", documentRAG).
    WithMCP("filesystem", filesystemMCP).
    WithRuntimeStore(runtimeStore).
    WithSessionStore(sessionStore).
    Defaults(
        goai.DefaultModel("qwen3"),
        goai.DefaultRunner("agent"),
    )

client, err := cfg.NewClient(ctx)
```

`DefaultConfig` 提供最小默认值。`With` 操作推荐采用不可变值语义：返回新配置，不修改原配置。实现必须复制内部 slice 和 map，避免不同配置共享可变底层数据。

### 4.2 Client

`Client` 是应用级共享运行入口，应用启动时创建一次，所有会话复用。它内部持有：

- Model、Runner、RAG、MCP 和 Tool Registry；
- Runtime；
- Session、Message、Run、Event 和 Checkpoint Store；
- 默认配置；
- 活跃 Run 索引；
- 实时事件订阅管理。

最小调用：

```go
client, err := goai.DefaultConfig().
    WithModel("default", model).
    NewClient(ctx)

run, err := client.Run(ctx, goai.Request{
    SessionID: "session-001",
    Input:     message.User("你好"),
})
```

### 4.3 Session

`Session` 是可选的轻量句柄，只保存 `Client` 引用、Session ID 和少量覆盖项，不缓存完整历史。应用可以在每次 HTTP 请求中重新创建它。

```go
session, err := client.NewSession(
    goai.WithSessionID("session-001"),
    goai.UseModel("qwen3"),
    goai.UseRunner("agent"),
    goai.UseRAG("documents"),
    goai.UseMCP("filesystem"),
)

run, err := session.Run(ctx, message.User("分析这个项目"))
```

不需要绑定 Session 句柄的应用可以直接把同样的信息放在 `Request` 中调用 `client.Run`。

### 4.4 Run

`Run` 表示一次独立、可持久化的执行：

```go
type Run interface {
    ID() string
    Status(context.Context) (RunStatus, error)
    Events() <-chan Event
    Wait(context.Context) (Result, error)
    Cancel(context.Context) error
}
```

Client 还应支持通过 Run ID 重新访问：

```go
run, err := client.Resume(ctx, runID)
events, err := client.Replay(ctx, runID, afterSeq)
stream, err := client.Subscribe(ctx, runID, afterSeq)
err = client.Cancel(ctx, runID)
```

## 5. 配置作用域

Option 必须区分作用域，不能把所有配置混在同一个 `Option` 类型中。

### 5.1 应用级 ConfigOption

用于应用启动时装配：

- Model、Runner、RAG 和 MCP 注册；
- Store；
- 全局 Tool；
- Approval；
- Middleware、Event Handler；
- 默认组件选择；
- 全局安全和资源限制。

### 5.2 SessionOption

用于前端控制某个会话：

- Session ID；
- 已注册 Model 和 Runner 的名称；
- RAG、MCP 和 Tool 的允许列表；
- System Prompt 或 Prompt Template ID；
- Context/Memory 策略；
- 用户、租户和业务 Metadata。

会话配置如果需要在重新连接后继续生效，必须写入 Session Store。

### 5.3 RunOption

只作用于本轮：

- 最大步骤数；
- 本轮超时；
- Response Format；
- 模型采样参数；
- 本轮 Metadata；
- 临时但受应用策略允许的覆盖项。

### 5.4 类型隔离

建议使用不同的接口防止传错层级：

```go
type ConfigOption interface {
    applyConfig(*Config) error
}

type SessionOption interface {
    applySession(*SessionConfig) error
}

type RunOption interface {
    applyRun(*RunConfig) error
}
```

RAG、MCP 和 Runner 子系统拥有自己的 Option 类型，不能把 `rag.WithChunkSize` 直接传给顶层 Config。

## 6. 配置合并和默认值

固定优先级为：

```text
核心最小默认值
    < Preset
    < 应用显式 ConfigOption
    < 持久化 Session 配置
    < 本轮 RunOption
```

越靠后优先级越高。行为必须确定且可测试。

### 6.1 最小默认方案

除 Model 外，核心提供：

- 默认单 Agent Runner；
- 默认内存 Run/Event/Checkpoint/Session Store；
- 默认 Tool Registry；
- 默认串行 Tool 执行；
- 默认事件机制；
- 默认无 RAG；
- 默认无 MCP；
- 默认无人工审批 Handler；
- 默认 Context Builder。

核心不隐式选择 Ollama、OpenAI 或具体模型。完整零配置体验由 Provider/Preset 扩展提供，例如 `preset.LocalOllama("qwen3")`。

### 6.2 启用和关闭可选能力

注册能力和选择能力必须分开：

```go
cfg.WithRAG("documents", documentRAG) // 应用注册
goai.UseRAG("documents")             // 会话选择
```

不推荐单独使用 `EnableRAG bool`，因为应用可能注册多个知识库。空选择表示不启用。为了覆盖 Preset，可以提供 `WithoutRAG`、`WithoutMCP` 等显式关闭 Option。

## 7. 应用注册与前端选择

应用启动时构建能力目录：

```text
Models
├── qwen3
└── deepseek

Runners
├── agent
└── research

RAG
├── product-docs
└── code-docs

MCP
├── filesystem
└── database
```

前端只传名称：

```json
{
  "session_id": "s-001",
  "model": "qwen3",
  "runner": "agent",
  "rag": ["code-docs"],
  "mcp": ["filesystem"]
}
```

Client 在 Run 开始前完成：

1. 名称解析；
2. 是否存在校验；
3. 会话/用户权限校验；
4. 组件兼容性校验；
5. 默认值补全；
6. 生成不可变的本轮 `ResolvedConfig`。

持久化只保存名称、版本和普通配置，不序列化 Go 组件实例。Resume 时再次通过 Registry 解析组件；缺失或版本不兼容时返回明确错误。

## 8. Runner 与未来 Graph

Runner 是整体执行策略的主要扩展边界：

```text
Runtime
  └── Runner
      ├── 内置 Agent Loop
      ├── 未来 Graph Runner
      └── 应用自定义 Runner
```

默认用法：

```go
client, err := goai.DefaultConfig().
    WithModel("default", model).
    NewClient(ctx)
```

调整内置循环：

```go
runner := agent.NewRunner(
    agent.WithMaxSteps(12),
    agent.WithParallelTools(true),
)
```

未来 Graph 通过 Runner 接入：

```go
cfg.WithRunner("workflow", graph.NewRunner(workflow))
```

Graph 不控制 Runtime，也不重新定义 Run、Event、Cancel、Approval 和 Resume 语义。

## 9. RAG 二级装配

顶层只依赖完整 RAG 契约：

```go
cfg.WithRAG("documents", documentRAG)
```

RAG 内部由应用自行装配：

```go
documentRAG, err := rag.DefaultConfig().
    WithChunker(chunker).
    WithEmbedder(embedder).
    WithDocumentStore(documentStore).
    WithVectorStore(vectorStore).
    WithRetriever(retriever).
    WithReranker(reranker).
    New(ctx)
```

边界划分：

- 核心 `rag`：Document、Chunk、Query、Hit、Chunker、Embedder、Store、Retriever、Reranker 等契约；
- 无依赖通用算法：可以保留在核心或轻量算法包；
- 具体实现：SQLite FTS、pgvector、Qdrant、Ollama/OpenAI Embedder 等放入扩展包；
- Pipeline：作为可选装配方案或 Preset，不作为唯一 RAG 运行方式。

官方预设同样使用公开契约：

```go
documentRAG, err := rag.Configure(
    ragpreset.SQLiteHybrid(database),
).
    WithEmbedder(customEmbedder).
    New(ctx)
```

## 10. MCP 装配

MCP 由应用创建、连接并注册：

```go
mcpManager := mcp.Configure(
    mcp.WithServer("filesystem", filesystemTransport),
    mcp.WithServer("database", databaseTransport),
).New()

cfg.WithMCP("workspace", mcpManager)
```

会话通过名称选择。MCP Tool 最终适配为标准 `tool.Tool`，进入统一 Tool Registry 和审批策略。核心 Agent Loop 不区分本地 Tool 与 MCP Tool。

MCP 的连接、重连、工具列表刷新和关闭策略属于 MCP 组件，不进入顶层 Config 的实现细节。

## 11. 会话状态与 Go Context

必须区分三类上下文：

```text
context.Context   调用超时、取消、Trace 和请求范围值
Session State     会话配置、总结、消息、记忆
Model Context     本轮实际发送给模型的内容
```

`context.Context` 不保存历史消息、RAG 配置或 Run 状态。会话数据保存在明确的 Store 中。

建议持久化模型：

```text
sessions      会话配置、总结、版本、Metadata
messages      用户、assistant、tool 消息
runs          Run 状态和会话关联
run_events    带单调 Seq 的事件
checkpoints   可恢复执行状态
```

## 12. 会话内容的内存策略

默认不跨轮缓存完整会话内容。

每轮执行：

```text
读取 Session 配置
  → 读取 Summary、重要记忆和最近消息
  → 构造 Working Context
  → 在本轮 Model/Tool 循环中复用
  → 每个事件及时持久化
  → 本轮结束后释放
```

不应在 Agent 每一步都重新读取数据库；同一个 Run 内部复用 Working Context。下一轮再从持久化数据构建。

Context Builder 不能无限读取完整历史，应该按 Token Budget 组合：

```text
System Prompt
+ 会话总结
+ 已确认的重要事实/待办
+ RAG 检索结果
+ 最近历史消息
+ 当前用户消息
+ Tool 定义
```

第一版不提供默认 Session 内容缓存。未来如性能数据证明需要，可以通过 Store Decorator 接入 TTL/LRU 缓存；数据库仍是真实来源。

内存中允许保留：

- 已初始化的共享组件；
- 当前执行 Run 的 Working Context；
- `runID -> cancel/subscriber/approval` 等活跃控制信息；
- 有容量限制的实时事件队列。

等待长时间审批时应保存 Checkpoint，并允许释放 Working Context；批准后重新构建并恢复。

## 13. Run 生命周期与 ctx 语义

前端连接生命周期不能决定持久 Run 的生命周期。

建议明确：

- `Run/Start`：创建持久 Run；调用方 ctx 只控制提交和创建过程；
- `Wait`：ctx 取消只停止等待，不自动取消 Run；
- `Cancel`：显式取消 Run；
- `Resume`：从持久化 Checkpoint 继续；
- `Replay`：读取 `afterSeq` 之后的历史事件；
- `Subscribe`：先补齐持久化事件，再接入实时事件流。

如果需要传统同步行为，可额外提供：

```go
result, err := client.Execute(ctx, request)
```

`Execute` 的 ctx 可以控制整个同步执行，而持久运行优先使用 `Run/Start + Wait + Cancel`。

核心不变量：

- `runID` 唯一；
- Event `Seq` 在单个 Run 内单调递增；
- 客户端断开不等于取消；
- Tool Call 和 Tool Result 可关联；
- Event 和 Checkpoint 的持久化顺序与执行顺序一致；
- Resume 不重复执行已确认完成的步骤；
- 状态只能按允许的状态机转换。

## 14. 组件生命周期和所有权

组件由应用初始化时，默认所有权属于应用：

```go
db, err := sql.Open(...)
defer db.Close()

store := sqlite.NewRuntimeStore(db)
client, err := goai.DefaultConfig().
    WithRuntimeStore(store).
    NewClient(ctx)
defer client.Close()
```

规则：

- 应用注入的 Model、数据库、RAG 和 MCP 实例默认由应用关闭；
- `Client.Close` 只停止 Client 自己创建的 goroutine、订阅和默认内部组件；
- Client 不能擅自关闭可能被其他业务共享的外部组件；
- Preset 如果创建资源，必须明确返回或声明资源所有权；
- 关闭顺序、重复 Close 和部分初始化失败必须可测试。

## 15. 通用扩展桩点

无法提前预测所有未来能力，因此除了领域接口，还需要少量稳定的横向扩展面：

- `Runner`：替换整体执行方式；
- `Middleware`：介入模型和 Tool 调用前后；
- `ContextProvider`：注入 RAG、Memory、业务上下文；
- `EventHandler`：日志、审计、Tracing、Metrics；
- `ApprovalPolicy`：决定是否允许、拒绝或等待审批；
- `Metadata`：携带租户、用户、Trace 和业务关联信息。

扩展不能直接修改 Runtime 内部状态。Middleware 的执行顺序、错误传播和是否允许修改输入输出必须形成明确契约。

不建议设计一个无类型的万能 `Plugin` 或使用 Go 动态插件。优先使用小接口、构造注入、Functional Options、Registry 和 Middleware。

## 16. 校验与错误模型

`NewClient` 必须完成启动校验：

- 至少存在一个可用 Model；
- 默认名称必须在对应 Registry 中存在；
- Store 组合满足 Runtime 要求；
- Runner 所需能力已注册；
- 同名组件不能静默覆盖；
- RAG 组件维度和能力兼容；
- MCP/Tool 名称冲突必须明确处理；
- 配置中的敏感数据不能出现在错误和日志中。

统一错误至少包含：

```go
type Error struct {
    Code      string
    Message   string
    Retryable bool
    Cause     error
}
```

建议错误码：

```text
invalid_config
component_not_found
component_conflict
model_unavailable
tool_not_found
tool_invalid_arguments
tool_execution_failed
approval_rejected
run_not_found
run_not_resumable
run_canceled
persistence_failed
context_limit_exceeded
```

## 17. 并发与一致性

- 同一 Session 默认应串行接受会改变历史的 Run，或使用 Session Version 做乐观并发控制；
- 并行 Tool 的结果可以并发计算，但必须按确定顺序生成持久事件和模型消息；
- Active Run 以内存 `runID` 索引管理取消、审批和实时订阅；
- Event Store 和 Checkpoint Store 负责重启恢复；
- 慢订阅者不能导致事件永久丢失，应能通过 Seq 发现缺口并从 Store 补齐；
- 多进程部署时不能依赖单进程缓存维持正确性。

## 18. 包和模块边界

目标结构：

```text
dlz-goai
├── goai.go / config.go      顶层集成 API
├── message                  公共消息协议
├── llm                      模型契约
├── tool                     Tool 契约和 Registry
├── agent                    默认 Agent Loop
├── runtime                  Run 状态机、事件和恢复
├── rag                      RAG 契约
├── mcp                      MCP Tool Client 契约
└── internal                 不承诺兼容的实现细节

扩展层（初期可同仓库分包，成熟后再拆 module/repo）
├── provider/ollama
├── provider/openai
├── storage/sqlite
├── storage/postgres
├── embedding/*
├── retrieval/*
├── observability/*
└── preset/*
```

不要在第一阶段同时进行 API 设计、目录搬迁、多 Module 拆分和应用迁移。先通过新门面包装现有实现，再迁移真实应用，最后移动具体实现。

## 19. 迁移计划

### 阶段一：增加门面，不移动现有实现

- 新增 `DefaultConfig`、链式 Config 和 `NewClient`；
- 新增 Client 的 `Run/Resume/Cancel/Replay/Subscribe`；
- 内部适配现有 `agent.Runner` 和 `runtime.Runtime`；
- 保留现有底层 API，避免一次性破坏兼容。

### 阶段二：建立 Registry、Preset 和配置校验

- 支持 Model、Runner、RAG 和 MCP 命名注册；
- 明确 Option 覆盖顺序；
- 实现最小默认方案；
- 提供配置解析结果的只读检查能力，输出时隐藏敏感信息。

### 阶段三：迁移真实应用

- 先让 `ollama-manager` 只通过 Client 门面调用 Runtime；
- 再让 `mdk` 使用同一门面，并验证 RAG、MCP、Trace 和审批；
- 应用保留组件初始化和前端选择映射代码。

### 阶段四：整理扩展层

- 将具体 SQLite、Provider、Embedding 和检索实现逐步移出核心契约层；
- Pipeline 降级为可选 Preset；
- 为每类扩展提供 Contract Test Suite；
- 根据真实依赖和版本需求决定是否拆 Go Module 或仓库。

### 阶段五：扩展能力

只有在真实应用出现明确需求后，再通过既有 Runner/ContextProvider/Middleware 桩点加入 Graph、Memory、Guardrail、Cache 等能力。

## 20. 验收标准

完成目标架构后，应满足：

1. 只提供 Model 就能启动最小对话；
2. 应用只初始化一次 Client，多个 Session 共享底层组件；
3. 前端通过组件名称选择 Model、Runner、RAG、MCP 和 Tool；
4. `ollama-manager` 与 `mdk` 使用同一套公开 Client API；
5. 应用不再自行实现 Agent Tool Loop 和 Run 状态机；
6. 页面切换后可以按 Run ID 和 Seq 重新订阅；
7. 每轮从持久化数据构建上下文，结束后释放完整 Working Context；
8. 更换 Store、Embedder、Retriever 或 Runner 不需要修改核心；
9. 官方 Preset 不依赖私有接口；
10. 所有核心不变量都有测试，扩展具备可复用的契约测试。

## 21. 完整目标示例

应用启动装配：

```go
documentRAG, err := rag.DefaultConfig().
    WithChunker(chunker).
    WithEmbedder(embedder).
    WithVectorStore(vectorStore).
    WithRetriever(retriever).
    New(ctx)
if err != nil {
    return err
}

client, err := goai.DefaultConfig().
    WithModel("qwen3", chatModel).
    WithRunner("agent", agent.DefaultRunner()).
    WithRAG("documents", documentRAG).
    WithMCP("filesystem", filesystemMCP).
    WithTools(orderTool, searchTool).
    WithRuntimeStore(runtimeStore).
    WithSessionStore(sessionStore).
    Defaults(
        goai.DefaultModel("qwen3"),
        goai.DefaultRunner("agent"),
    ).
    NewClient(ctx)
if err != nil {
    return err
}
defer client.Close()
```

根据前端选择开始会话：

```go
session, err := client.NewSession(
    goai.WithSessionID(request.SessionID),
    goai.UseModel(request.Model),
    goai.UseRunner(request.Runner),
    goai.UseRAG(request.RAG...),
    goai.UseMCP(request.MCP...),
    goai.UseTools(request.Tools...),
)
if err != nil {
    return err
}

run, err := session.Run(ctx, message.User(request.Content))
```

重新连接和控制：

```go
events, err := session.Subscribe(ctx, runID, lastSeq)
run, err := session.Resume(ctx, runID)
err = session.Cancel(ctx, runID)
```

以上顶层调用方式应在未来增加 Graph、Memory、更多 RAG 和更多存储实现后保持稳定。
