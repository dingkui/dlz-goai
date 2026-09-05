# dlz-goai

**一句话讲清**：dlz-goai 是一个零第三方依赖的 Go 智能体基础库——统一的模型流式调用、进程内工具与 MCP 工具的统一抽象、带审批与事件流的工具调用循环，以及持久化运行（检查点、断点续跑、事件回放）与检索增强（RAG）。它让已有 Go 应用以较低成本获得**可恢复、可审批、可追踪**的 Agent 执行能力。

**它是一个工具调用运行时，不是智能体框架。** 不提供图编排、多智能体、独立 HTTP 服务——编排类能力等真实需求出现后再评估。

## 快速上手

```bash
go get github.com/dingkui/dlz-goai
```

最小工具调用循环（本地 Ollama + 一个只读工具）：

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

需要持久化恢复时一行装配：

```go
db, opts, _ := sqlite.OpenRuntime("runs.db")
defer db.Close()
rt := runtime.New(opts)
```

下一步：[快速开始](docs/快速开始.md) ｜ [实战案例：接入现有 Go 项目](docs/实战案例-接入现有Go项目.md)（规划中）

## 功能清单

| 包 | 职责 | 依赖 | 稳定性 |
|---|---|---|---|
| `tool` | 工具契约：定义、调用、结果、注册表、策略、类型化工具、恢复分级（RetryPolicy） | 无 | 稳定 |
| `message` | 对话数据契约：消息、增量、选项 | `tool` | 稳定 |
| `llm` | 模型能力接口 + 服务配置（不含任何实现） | `message` `tool` | 稳定 |
| `factory` | 可选：按配置构造内置 Provider | `llm` + 各 provider | 稳定 |
| `provider/openai` | 任何 OpenAI 兼容接口（DeepSeek/通义/vLLM…） | `internal/wire` | 稳定 |
| `provider/ollama` | Ollama 原生接口（含用量统计） | `internal/wire` | 稳定 |
| `agent` | 工具调用循环：步骤上限、超时、截断、审批、事件流、并行执行 | `message` `tool` | 稳定 |
| `mcp` | **MCP Tool Client / Adapter**（stdio + Streamable HTTP） | 无 | 稳定 |
| `runtime` | 持久化运行：状态登记、事件落库、调用级检查点、断点续跑（含事件对账）、回放与订阅 | `agent` `message` `tool` | 实验性 |
| `rag` | 检索增强：分块、Embedder/VectorStore/Retriever/Reranker 接口、RRF 融合、Pipeline、Retriever→Tool 适配 | `tool` | 实验性 |
| `embedding/ollama` `embedding/openai` | 双厂商 Embedder 实现 | `rag` | 实验性 |
| `storage/memory` | 内存 VectorStore（暴力余弦，开发/测试用） | `rag` | 实验性 |
| `storage/sqlite` | SQLite 持久实现：rag.VectorStore + runtime 三 Store（`sqlite.OpenRuntime` 开箱恢复预设），WAL 模式 | `rag` `runtime` + sqlite | 实验性 |

依赖严格单向：`llm` 不 import 任何 provider；`agent` 只认 `tool.Tool`；`mcp` 可单独使用；`runtime` 依赖 `agent`；`rag` 只依赖 `tool`（可独立用，也可经 `rag.NewTool` 包成工具接入 agent）。

> **依赖策略**：除 `storage/sqlite`（modernc.org/sqlite，纯 Go 无 CGO）外，所有模块零第三方依赖。不 import storage/sqlite 就不会引入该依赖。

**核心语义四条**（详见各使用手册）：

- **两种失败，两条通道**：`tool.Result.IsError` 是业务失败，原因回传模型自我纠正；`error` 是执行失败，终止当前步骤。
- **默认即安全**：未声明只读的工具默认需要审批；`Approve` 为 nil 一律拒绝；策略可按工具覆盖（`auto` / `confirm` / `deny`）。
- **边界即防线**：步数上限防死循环、单工具超时防挂起、结果超限 UTF-8 安全截断；中断时已流出的内容保留在轨迹里。
- **恢复实验性**：调用级检查点 + 事件对账 + RetryPolicy 分级，作为内部验证目标持续打磨，暂不构成公开兼容承诺（见 [runtime 手册](docs/使用手册/runtime.md)）。

> MCP 定位说明：本库实现 `initialize / tools/list / tools/call`，定位是 **MCP Tool Client / Adapter**（把 MCP 工具接入 agent），不是完整的 MCP SDK——resources、prompts、sampling 等不在当前范围。客户端标识默认上报 `DefaultClientInfo()`（name 为 "dlz-goai"），宿主应用应通过 `Client.Info` / `StdioClient.Info` 覆盖为自己的产品名。

## 常见问题

**和 langchaingo / eino 有什么区别？**
dlz-goai 不做框架：不接管你的代码结构、不要求图编排、依赖面极小（核心零第三方依赖）。它提供的是一段可以嵌进现有 Go 服务的执行内核——模型调用、工具循环、审批、持久化恢复。适合"给已有系统加 AI 执行能力"，不适合从零搭建编排式 AI 平台。

**恢复（断点续跑）可信吗？**
恢复语义目前标注为**实验性**：作为内部验证目标持续打磨，配套故障注入回归测试（崩溃窗口结果复用、审批中断续跑、进程重启续跑、落库失败中止）。暂不构成公开兼容承诺；外部副作用的"恰好一次"需要工具与业务系统配合（如幂等键），本库不承诺。

**支持哪些模型服务？**
任何 OpenAI 兼容接口（OpenAI、DeepSeek、通义、vLLM、llama.cpp 等）走 `provider/openai`；本地 Ollama 走原生接口（能拿到 token 用量与耗时）。其他协议实现 `llm.Provider` 三方法即可接入（见扩展手册，规划中）。

**模型不支持工具调用怎么办？**
部分本地模型不支持 function calling。运行时会收到服务端错误；调用方应在选型时确认模型能力，或用 `Ping`/`ListModels` 做启动预检。空工具集会自动退化为普通对话，不报错。

**审批怎么接入我的界面？**
用 `agent.Broker`：运行中阻塞等待，决策通过独立 HTTP 端点提交（`rt.Approve(runID, callID, approved)`），与 SSE 流解耦。未配置审批 Handler 时，非只读工具一律拒绝——宁可不做，也不未确认执行。

**Windows / macOS 支持吗？**
纯 Go 实现，跨平台；CI 覆盖 ubuntu 与 windows。`storage/sqlite` 用纯 Go 驱动，无需 CGO。

## 文档

- [快速开始](docs/快速开始.md) — 安装、最小示例、官方示例走读、常见报错
- [使用手册：llm](docs/使用手册/llm.md) — 模型调用、多服务配置、流式与用量
- [使用手册：agent](docs/使用手册/agent.md) — 工具循环、审批、事件流、并行
- [使用手册：runtime](docs/使用手册/runtime.md) — 持久化运行、断点续跑、事件回放
- 使用手册 tool / mcp / rag / storage、扩展手册、升级计划 — 编写中

## API 稳定性与路线图

- `tool` / `message`：已稳定，结构体字段与 JSON tag 视为公共契约（影响持久化兼容性）。
- `agent.Config` / `agent.Event` / `llm.Provider`：新增字段采用可选扩展，不破坏现有调用。
- `internal/*`：内部实现，随时可能变动。
- 1.0 之前，次版本升级可能有少量 API 调整，迁移成本控制在 sed 级别。

路线图（已完成：agent/llm/mcp 第一阶段、runtime、rag+storage/sqlite）：

- [ ] `retrieval/{vector,fulltext,hybrid}` + `rerank/{llm,score}` + `splitter/{text,markdown}` — 拆分检索策略与重排实现；FullTextStore 暂无实现（应用可接 SQLite FTS5）
- [ ] 最小 Client 门面 + 独立接入示例（见 [嵌入式集成架构设计](docs/embedded-integration-design.md) 设计草案）
- [ ] `compose` — 等真实应用产生编排需求后再评估（Graph / Workflow）

## License

MIT
