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

依赖严格单向：`llm` 不 import 任何 provider（接口与实现解耦，自定义实现零牵连）；`agent` 只认 `tool.Tool` 接口；`mcp` 可单独使用。

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

- [x] 第一阶段：`message` `tool` `llm` `provider` `factory` `agent` `mcp`（当前）
- [ ] `runtime` — 持久化运行（Run/EventStore/CheckpointStore/Resume/Replay/审批状态持久化）
- [ ] `rag` + `embedding` + `retrieval` + `storage` — 检索增强（可独立使用，可包装成 tool）
- [ ] `compose` — 等真实应用产生编排需求后再评估（Graph / Workflow）

## License

MIT
