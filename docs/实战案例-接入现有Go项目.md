# 实战案例：给工单系统接入 Client

目标是让助手调用业务工具，并在指派前等待用户确认。完整源码见 [examples/fullstack](../examples/fullstack/main.go)，使用模拟模型和内存 Store，不需要 API key，不修改外部工单。

## 运行与预期输出

在当前库仓库根目录执行：

```bash
go run ./examples/fullstack
```

输出依次包含：等待审批 → 显式取消 → 模拟审批 → 工具回执 → 事件数量与执行次数。该示例演示同进程取消后续跑，进程和内存 Store 始终存在，不代表真实崩溃恢复，也不代表外部副作用恰好一次。

## 1. 包装已有业务函数

示例通过 `tool.Typed` 定义工单号和处理人，生成参数 schema，并在处理函数内检查必填业务值。真实项目将模拟返回替换为 `TicketService.AssignTicket(ctx, ...)`。

写操作声明为非只读。示例包装器实现 `RetryClassifier` 并返回 NoRetry，用于实验性恢复中识别不确定调用；它不代替业务幂等键或人工核实。工具返回错误的行为见 [错误处理](指南/错误处理.md)。

## 2. 应用启动时创建 Client

配置片段：

```go
client, err := dlzgoai.NewClient(dlzgoai.Options{
    Model: callModel,
    Tools: []tool.Tool{assign},
})
if err != nil { return err }
defer client.Close() // 应放在应用主生命周期内。
```

真实模型通过 `provider.ChatStream` 适配 ModelFunc。工具与 Provider 实例供多次运行共享，需要支持并发使用。

## 3. 提交任务，返回运行标识

```go
run, err := client.Start(requestCtx, dlzgoai.Request{Input: content})
if err != nil { return err }
runID := run.ID()
// HTTP 层将 runID 返回前端；执行已在 Client 的后台 goroutine 中启动。
```

Start 成功返回时已完成运行登记和初始检查点，后续执行结果用 Wait 获取；连接结束不取消运行。

## 4. 审批与订阅使用独立端点

前端通过 `client.Stream(ctx, runID, afterSeq, emit)` 展示进度。收到待审批调用后展示参数，用户确认再提交 `client.Approve(runID, callID, approved)`。

审批端点需要校验登录用户、运行归属和操作权限。示例中的固定允许仅用于模拟用户选择，不应原样接到生产审批端点。不要先阻塞 Wait 再处理审批，否则运行可能一直等待。

HTTP 结构与流式写入方式见 [事件与流式集成](指南/事件与流式集成.md)。

## 5. 取消后继续

完整示例先等待 waiting_approval，再调用 Cancel 并等待第一次运行退出。随后 Resume，同一模型根据恢复后的消息重新提出调用，再模拟用户确认。

同进程 Resume 沿用原请求配置；重启后通过 ResumeWith 或 ResumeResolver 重建原模型、工具和策略。准备错误直接返回，执行错误从新句柄 Wait 获取。见 [Client 手册](使用手册/client.md)。

## 6. 改为持久化记录

在应用启动时用 `sqlite.OpenRuntime` 创建 Runtime 并注入 Client；应用退出时先关闭 Client，再关闭数据库。完整装配片段见 [Client 手册](使用手册/client.md)。

三种情况需要分别处理：

| 情况 | 行为 |
|---|---|
| 页面关闭、服务仍运行 | 重新订阅 Stream；无需 Recover 或 Resume。 |
| 显式取消，进程仍在 | 等待运行退出，再按业务需要 Resume。 |
| 服务进程退出并重启 | 重新打开持久 Store，启动阶段 Recover，再查询遗留运行并按业务决策 Resume。 |

Recover 只标记遗留非终态记录并返回数量，不自动继续，也不返回列表。当前恢复为实验性能力；多实例执行权与外部操作核实由应用负责。

## 7. 内部验证建议

- 同进程演示用于验证 Client 接入与审批流程。
- 真实重启应单独验证：子进程 + SQLite，在等待审批、工具执行和提交位置设置故障点，退出后用新进程检查。
- 前端断线用独立测试验证重连和事件消费，不用 Cancel 代替断线。
- 记录测试条件、工具实际副作用次数和恢复结果，不将单次成功演示描述成可靠性保证。

## 接入检查

- Client 在应用启动时创建，在应用退出时关闭。
- 请求参数经过服务端校验，工具与审批权限来自应用策略。
- 错误从 Start、Wait、Stream、Approve 分别处理，不忽略持久化错误。
- 前端保存最后消费的 Seq；审计使用 Replay，UI 跟随使用 Stream。
- 模型与工具响应 ctx，关闭数据库前先等待任务退出。

下一步：[MCP](使用手册/mcp.md)、[RAG](使用手册/rag.md)、[存储](使用手册/storage.md)。
