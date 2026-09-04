# dlz-goai

零第三方依赖的 Go 智能体基础库：统一的模型流式调用、进程内工具与 MCP 工具的统一抽象、带审批与事件流的工具调用循环。

**它是一个工具调用运行时，不是智能体框架。** 不提供 RAG、图编排、多智能体——那些是后续可选模块（见路线图）。

## 能力

| 包 | 职责 | 依赖 |
|---|---|---|
| `tool` | 工具契约：定义、调用、结果、注册表、策略、类型化工具 | 无 |
| `message` | 对话数据契约：消息、增量、选项 | `tool` |
| `llm` | 模型能力接口 + 服务配置（不含任何实现） | `message` `tool` |
| `factory` | 可选：按配置构造内置 Provider | `llm` + 各 provider |
| `provider/openai` | 任何 OpenAI 兼容接口（DeepSeek/通义/vLLM…） | `internal/wire` |
| `provider/ollama` | Ollama 原生接口（含用量统计） | `internal/wire` |
| `agent` | 工具调用循环：步骤上限、超时、截断、审批、事件流、并行执行 | `message` `tool` |
| `mcp` | **MCP Tool Client / Adapter**（stdio + Streamable HTTP） | 无 |
| `runtime` | 持久化运行：状态登记、事件落库、步级检查点、断点续跑、回放与订阅 | `agent` `message` `tool` |
| `rag` | 检索增强：分块、Embedder/VectorStore/Retriever/Reranker 接口、RRF 融合、Pipeline、Retriever→Tool 适配 | `tool` |
| `embedding/ollama` `embedding/openai` | 双厂商 Embedder 实现 | `rag` |
| `storage/memory` | 内存 VectorStore（暴力余弦，开发/测试用） | `rag` |

依赖严格单向：`llm` 不 import 任何 provider；`agent` 只认 `tool.Tool`；`mcp` 可单独使用；`runtime` 依赖 `agent`；`rag` 只依赖 `tool`（不依赖 agent，可独立用，也可经 `rag.NewTool` 包成工具接入 agent）。

> MCP 定位说明：本库实现 `initialize / tools/list / tools/call`，
> 定位是 **MCP Tool Client / Adapter**（把 MCP 工具接入 agent），
> 不是完整的 MCP SDK——resources、prompts、sampling 等不在当前范围。

## 快速开始

```go
provider := ollama.New("") // 或 openai.New(baseURL, apiKey)；或 factory.NewProvider(svc)

tools := []tool.Tool{
    tool.NewFunc("get_time", "获取当前时间", nil, true,
        func(context.Context, map[string]any) (tool.Result, error) {
            return tool.Text(time.Now().Format(time.RFC3339)), nil
        }),
}

result, err := agent.New().Run(
    context.Background(),
    []message.Message{{Role: message.RoleUser, Content: "现在几点了？"}},
    nil,
    agent.Config{Tools: tools},
    func(ctx context.Context, msgs []message.Message, opts *message.Options, cb func(message.Delta)) error {
        return provider.ChatStream(ctx, "qwen3:8b", msgs, opts, cb)
    },
    func(e agent.Event) { /* 驱动 UI / SSE / 审计日志 */ },
)
```

完整可运行示例见 [examples](examples/)：[`minimal`](examples/minimal)（进程内工具）、[`mcp`](examples/mcp)（MCP 服务端接入）、[`approval`](examples/approval)（人工确认）。

## 核心语义

**两种失败，两条通道。** `tool.Result.IsError` 表示工具成功执行但业务失败（参数不合法、记录不存在），原因会回传给模型，给它自我纠正的机会；`error` 表示执行本身失败（连接超时、进程崩溃），会终止当前步骤。

**默认即安全。** 未声明只读的工具默认需要审批；策略可按工具覆盖（`auto` / `confirm` / `deny`）；审批 Handler 由调用方注入（交互弹窗、HTTP 端点、静态策略均可），`agent.Broker` 提供流式运行与外部决策解耦的现成实现。

**边界即防线。** 步数上限防死循环；单工具超时防挂起；结果超限自动截断（UTF-8 安全）；中断时已流出的内容保留在轨迹里，界面显示过的文字不丢失。

**类型化工具。** 用 Go 结构体定义输入，JSON Schema 自动生成、参数自动绑定：

```go
type searchInput struct {
    Query string `json:"query"`
    Limit int    `json:"limit,omitempty"`
}
tool.Typed("search", "搜索知识库", true,
    func(ctx context.Context, in searchInput) (tool.Result, error) { ... })
```

**并行执行。** 默认串行（顺序完全稳定）；一次响应包含多个工具调用时可启用并行，结果仍按调用顺序写回（协议要求 tool 消息与调用一一对应）：

```go
agent.Config{Tools: tools, ToolExecution: agent.ToolParallel}
```

## API 稳定性

- `tool` / `message`：已稳定，结构体字段与 JSON tag 视为公共契约（影响持久化兼容性）。
- `agent.Config` / `agent.Event` / `llm.Provider`：新增字段采用可选扩展，不破坏现有调用。
- `internal/*`：内部实现，随时可能变动。
- 1.0 之前，次版本升级可能有少量 API 调整，迁移成本控制在 sed 级别。

## 路线图

- [x] 第一阶段：`message` `tool` `llm` `provider` `factory` `agent` `mcp`
- [x] `runtime` — 持久化运行：状态机、EventStore、CheckpointStore、Resume、Replay、Subscribe、审批持久化、进程恢复
- [x] `rag` + `embedding` + `storage` — 检索增强：分块、Embedder/VectorStore/Retriever/Reranker 接口、RRF 融合、Pipeline、Retriever→Tool 适配（可独立使用，可包装成 tool）
- [ ] `retrieval/{vector,fulltext,hybrid}` + `rerank/{llm,score}` + `splitter/{text,markdown}` — 拆分检索策略与重排实现（当前策略在 rag 内，后续按需拆包）
- [ ] `storage/sqlite` — SQLite 持久实现（VectorStore/FullTextStore/EventStore/CheckpointStore）
- [ ] `compose` — 等真实应用产生编排需求后再评估（Graph / Workflow）

## runtime 用法

当一次运行需要跨进程可恢复时，用 `runtime` 包装 `agent.Runner`：存储全部走接口注入，自带内存实现，SQLite 等持久实现由调用方提供。

```go
rt := runtime.New(runtime.Options{
    Runs:        memory.NewRunStore(),
    Events:      memory.NewEventStore(),
    Checkpoints: memory.NewCheckpointStore(),
})

runID := rt.BeginRun()
// 运行期间：状态自动登记（pending→running→succeeded）、事件落库、每步存检查点
result, err := rt.Run(ctx, initial, opts, agent.Config{RunID: runID, Tools: tools}, model, nil)

// 进程重启后：恢复非终态运行、从检查点续跑
rt.Recover(ctx)
result, err = rt.Resume(ctx, runID, agent.Config{Tools: tools}, model, nil)

// 回放某次运行的全部事件
events, _ := rt.Replay(ctx, runID)

// 订阅进行中运行的事件（UI 广播）
ch, cancel := rt.Subscribe(runID)
defer cancel()
```

关键边界：**工具集是运行时对象、不可序列化**，所以 Resume 时工具由调用方重新提供——检查点保存"发生了什么"，不保存"能做什么"。

## rag 用法

rag 可独立构建知识库检索，也可经 `rag.NewTool` 包成工具接入 agent。

```go
// 建库：分块 → 向量化 → 存入 VectorStore
store := memory.NewVectorStore()
splitter := rag.TextSplitter{ChunkSize: 900}
chunks := splitter.Split(doc.Content)
vecs, _ := ollamaEmbedder.EmbedBatch(ctx, chunkTexts(chunks))
_ = store.Store(ctx, doc.ID, chunks, vecs)

// 检索：query → embed → search → 可选 rerank
pipeline := rag.Pipeline{Embedder: ollamaEmbedder, Store: store, DefaultTopK: 5}
hits, _ := pipeline.Retrieve(ctx, "查询问题", rag.RetrieveOptions{})

// 接入 agent：把检索器包成工具
tool := rag.NewTool("kb_search", "搜索知识库", pipeline, 5)
result, _ := agent.New().Run(ctx, msgs, opts, agent.Config{Tools: []tool.Tool{tool}}, model, emit)
```

通用算法（`rag.Cosine`、`rag.EncodeVec`/`DecodeVec`）可直接复用，不依赖任何 Embedder。

## License

MIT
