# dlz-goai

**为已有 Go 应用接入 AI 助手，让模型调用业务工具、等待人工审批，并实时展示执行过程。**

通过 Go API 直接嵌入现有项目，复用你的业务代码和基础设施。支持 OpenAI 兼容接口与 Ollama，本地工具和 MCP 工具使用统一的调用方式。

- **连接业务能力**：将 Go 函数或 MCP 工具交给模型调用。
- **控制执行过程**：配置工具权限、人工审批、步骤上限和执行超时。
- **展示任务进度**：通过统一事件流展示模型输出、工具调用和审批状态。
- **接入知识检索**：使用 RAG 为回答提供业务资料。
- **保存运行记录**：可选 SQLite 持久化与事件回放，并提供实验性的检查点恢复能力。

适合为现有后台、内部工具或桌面应用增加 AI 功能，例如查询业务数据、检索知识库，以及经人工确认后执行操作。

## 快速上手

推荐使用 Client：启动时绑定模型与默认工具，之后通过 `Start → Wait` 获取结果。需要 Go 1.24+，先在已有 Go module 中安装当前开发版本（Go 会记录具体提交的伪版本）：

```bash
go get github.com/dingkui/dlz-goai@master
```

启动本地 Ollama，执行 `ollama pull qwen3:8b`，将下列代码保存为 `main.go` 后执行 `go run .`。此示例为普通对话，无需审批：

```go
package main

import (
    "context"
    "fmt"
    "log"

    dlzgoai "github.com/dingkui/dlz-goai"
    "github.com/dingkui/dlz-goai/message"
    "github.com/dingkui/dlz-goai/provider/ollama"
)

func main() {
    provider := ollama.New("")
    client, err := dlzgoai.NewClient(dlzgoai.Options{
        Model: func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
            return provider.ChatStream(ctx, "qwen3:8b", msgs, opts, emit)
        },
    })
    if err != nil {
        log.Print(err)
        return
    }
    defer client.Close()

    run, err := client.Start(context.Background(), dlzgoai.Request{Input: "用一句话介绍 Go。"})
    if err != nil {
        log.Print(err)
        return
    }
    result, err := run.Wait(context.Background())
    if err != nil {
        log.Print(err)
        return
    }
    fmt.Println(result.Content)
}
```

Client 当前为实验性。默认记录保存在内存；需要持久化时可注入 SQLite Runtime，详见 [Client 手册](docs/使用手册/client.md)。检查点恢复仍在验证中，不作可靠性保证。

通过 `Options.Tools` 接入业务函数或 MCP 工具，写操作可通过独立界面提交审批。无需模型服务的完整演示见 [工单实战](docs/实战案例-接入现有Go项目.md)；需要检索知识库时见 [RAG](docs/使用手册/rag.md)。

下一步：[快速开始](docs/快速开始.md) ｜ [文档导航](docs/README.md) ｜ [事件与流式集成](docs/指南/事件与流式集成.md)

## 包与能力

| 包 | 职责 | 依赖 | 稳定性 |
|---|---|---|---|
| `dlzgoai`（根包） | **最小嵌入门面**：`Client` 装配模型/工具/存储，`Start`/`Wait`/`Approve`/`Resume`/`Stream` 管理运行生命周期 | `runtime` `agent` 等 | 实验性 |
| `tool` | 工具契约：定义、调用、结果、注册表、策略、类型化工具、恢复分级（RetryPolicy） | 无 | 稳定 |
| `message` | 对话数据契约：消息、增量、选项 | `tool` | 稳定 |
| `llm` | 模型能力接口 + 服务配置（不含任何实现） | `message` `tool` | 稳定 |
| `factory` | 可选：按配置构造内置 Provider | `llm` + 各 provider | 稳定 |
| `provider/openai` | OpenAI 兼容接口（DeepSeek/通义/vLLM…） | `internal/wire` | 稳定 |
| `provider/ollama` | Ollama 原生接口（含用量统计） | `internal/wire` | 稳定 |
| `agent` | 工具调用循环：步骤上限、超时、截断、审批、事件流、并行执行 | `message` `tool` | 稳定 |
| `mcp` | **MCP Tool Client / Adapter**（stdio + Streamable HTTP） | 无 | 稳定 |
| `runtime` | 持久化运行：状态登记、事件落库、调用级检查点、断点续跑（含事件对账）、回放与订阅 | `agent` `message` `tool` | 实验性 |
| `rag` | 检索增强：分块、Embedder/VectorStore/Retriever/Reranker 接口、RRF 融合、Pipeline、Retriever→Tool 适配 | `tool` | 实验性 |
| `embedding/ollama` `embedding/openai` | 双厂商 Embedder 实现 | `rag` | 实验性 |
| `storage/memory` | 内存 VectorStore（暴力余弦，开发/测试用） | `rag` | 实验性 |
| `storage/sqlite` | SQLite 持久实现：rag.VectorStore + runtime 三 Store（`sqlite.OpenRuntime` 持久化预设），WAL 模式 | `rag` `runtime` + sqlite | 实验性 |

## 设计与适用范围

核心包零第三方依赖；可选的 `storage/sqlite` 使用 `modernc.org/sqlite` 纯 Go 驱动，无需 CGO。各包可以按需使用：`agent` 只依赖消息与工具契约，模型 Provider 可替换；`rag` 可独立使用，也可包装成工具。

应用负责组件装配、业务权限和 HTTP 接口，dlz-goai 提供模型调用、工具循环、审批和运行记录。当前聚焦嵌入式工具调用运行时，图编排、多智能体和独立 HTTP 服务暂不在范围内，编排能力会根据实际需求评估。

MCP 支持 `initialize / tools/list / tools/call`，通过 stdio 或 Streamable HTTP 将服务端工具接入本地循环。协议范围和客户端标识配置见 [MCP 手册](docs/使用手册/mcp.md)。

## 常见问题

**支持哪些模型服务？**
OpenAI 兼容接口可通过 `provider/openai` 接入；本地 Ollama 使用原生接口，支持用量与耗时统计。其他协议可实现 `llm.Provider`，见 [自定义 Provider](docs/扩展手册/自定义Provider.md)。工具调用等能力取决于所选模型和服务端支持。

**模型不支持工具调用怎么办？**
可以先使用空工具集进行普通对话。需要执行工具时，应选择支持工具调用的模型，并用实际调用验证；`Ping` / `ListModels` 可检查服务连通性与模型列表，不能代替工具调用能力验证。

**审批怎么接入我的界面？**
Runtime 默认通过 `agent.Broker` 等待审批，应用收到审批事件后展示确认界面，再通过自己的 HTTP 端点调用 `rt.Approve(runID, callID, approved)` 提交决策。直接使用 `agent.Runner` 时，可注入自定义审批 Handler；需要审批但未配置 Handler 的调用会被拒绝。见 [审批示例](examples/approval/main.go)。

**Windows / macOS 支持吗？**
纯 Go 实现，跨平台；CI 覆盖 ubuntu 与 windows。`storage/sqlite` 用纯 Go 驱动，无需 CGO。

## 文档

- [文档导航](docs/README.md) — 按任务选择阅读路径
- [Client 手册](docs/使用手册/client.md) — 推荐入口、生命周期与持久化装配

- [快速开始](docs/快速开始.md) — 安装、最小示例、官方示例走读、常见报错
- [实战案例：接入现有 Go 项目](docs/实战案例-接入现有Go项目.md) — Client 工单接入与模拟中断演示（配 [examples/fullstack](examples/fullstack/main.go) 可运行示例）
- [使用手册：llm](docs/使用手册/llm.md) — 模型调用、多服务配置、流式与用量
- [使用手册：agent](docs/使用手册/agent.md) — 工具循环、审批、事件流、并行
- [使用手册：tool](docs/使用手册/tool.md) — 工具契约、类型化工具、Registry、恢复分级
- [使用手册：mcp](docs/使用手册/mcp.md) — MCP 服务接入、命名空间、生命周期
- [使用手册：runtime](docs/使用手册/runtime.md) — 持久化运行、断点续跑、事件回放
- [使用手册：rag](docs/使用手册/rag.md) — 分块、向量化、检索管线、知识库工具
- [使用手册：storage](docs/使用手册/storage.md) — 内存/SQLite 实现选型、自定义 Store
- 扩展手册：[自定义 Provider](docs/扩展手册/自定义Provider.md) ｜ [自定义工具](docs/扩展手册/自定义工具.md) ｜ [自定义存储](docs/扩展手册/自定义存储.md)
- 接入指南：[事件与流式集成](docs/指南/事件与流式集成.md) ｜ [错误处理](docs/指南/错误处理.md)
- [升级计划](docs/升级计划.md)（版本策略 / API 兼容承诺 / 路线图）· [变更记录](docs/_变更记录.md)

## API 稳定性与路线图

- `tool` / `message`：已稳定，结构体字段与 JSON tag 视为公共契约（影响持久化兼容性）。
- `agent.Config` / `agent.Event` / `llm.Provider`：新增字段采用可选扩展，不破坏现有调用。
- `internal/*`：内部实现，随时可能变动。
- 1.0 之前，次版本升级可能有少量 API 调整，升级前请查看 [变更记录](docs/_变更记录.md)。

路线图（已完成：agent/llm/mcp 第一阶段、runtime、rag+storage/sqlite）：

- [ ] `retrieval/{vector,fulltext,hybrid}` + `rerank/{llm,score}` + `splitter/{text,markdown}` — 拆分检索策略与重排实现；FullTextStore 暂无实现（应用可接 SQLite FTS5）
- [x] 最小 Client 门面（`Start`/`Wait`/`Approve`/`Resume`/`Stream`/`Close`，见根包 `client.go`）；独立接入示例见 examples/client 与 examples/fullstack
- [ ] `compose` — 等真实应用产生编排需求后再评估（Graph / Workflow）

## License

MIT

HTTP 接入示例见 [examples/http](examples/http/README.md)：无需 API Key，可验证提交、审批、取消、恢复和 SSE 重连。
