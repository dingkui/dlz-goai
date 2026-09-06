# 使用手册：runtime — 持久化运行与断点续跑

`runtime` 包装 `agent.Runner`，把一次运行变成**可登记、可回放、可恢复、可订阅**的过程：状态登记、事件落库、检查点、审批持久化、进程恢复。一般接入推荐 [Client](client.md)，本页用于直接操作底层 Runtime。

> **稳定性声明**：恢复语义为**实验性**——作为内部验证目标持续打磨，不作恢复可靠性保证。已验证场景见 `runtime` 包测试（崩溃窗口结果复用、审批中断续跑、进程重启续跑、落库失败中止）。

## 状态机

```text
pending -> running -> succeeded
              |
              +-> waiting_approval -> running
              +-> failed / canceled
```

| 状态 | 含义 |
|---|---|
| `pending` | 已登记未开始（Resume 重置后的短暂状态） |
| `running` | 运行中 |
| `waiting_approval` | 阻塞在工具审批上（`PendingApproval` 有待审批调用详情） |
| `succeeded` / `failed` / `canceled` | 终态（`Status.Terminal()` 判定） |

## 快速上手

```go
// 内存实现：开发调试用，不跨进程
rt := runtime.New(runtime.Options{
	Runs:        memory.NewRunStore(),
	Events:      memory.NewEventStore(),
	Checkpoints: memory.NewCheckpointStore(),
})

```

或选择 SQLite，以下为替代配置片段：

```go
// SQLite 保存记录；恢复行为为实验性。
db, opts, err := sqlite.OpenRuntime("runs.db")
if err != nil { panic(err) }
defer db.Close()
rt := runtime.New(opts)

runID := rt.BeginRun() // 不可猜测 ID（crypto/rand）
result, err := rt.Run(ctx, msgs, nil,
	agent.Config{RunID: runID, Tools: tools}, callModel, emit)
```

`cfg.RunID` 必填。`Run` 的返回值与 `agent.Run` 一致（`agent.Result`）。

## Run 期间自动发生什么

- **状态登记**：`pending → running → 终态`，审批等待时置 `waiting_approval` 并落库待审批调用详情。
- **事件落库**：配置 EventStore 时，agent 事件尝试写入该 Store 并分配单调递增 `Seq`——emit 收到的与落库内容一致（同 Seq 同内容）。
- **三级检查点**：①运行开始存初始检查点（用于首个审批前中断恢复）；②每条工具回执写入轨迹后存**调用级**检查点；③每步收尾存步级检查点。
- **审批包装**：`Config.Approve` 为 nil 时自动绑定内置 Broker，配合 `rt.Approve` 实现跨 HTTP 请求审批。

## 终态事件

正常存储条件下写入终态事件；存储失败时以返回错误为准，不能假设终态已持久化：

- `run_done`：成功（含用量统计）；
- `run_error`：失败（`Error` 字段有原因；取消为 "canceled"）；

另有 run_resumed：标记续跑，不是终态。

## Resume：断点续跑

```go
// 进程重启后第一步：把停留在非终态的登记标记为失败（附原因）
n, err := rt.Recover(ctx)

// 从最近检查点续跑；工具集由调用方重新提供
result, err := rt.Resume(ctx, runID,
	agent.Config{RunID: runID, Tools: tools}, callModel, emit)
```

状态约束：仅 `failed` / `canceled` 可 Resume；`succeeded` 返回 `ErrRunTerminal`；进行中返回 `ErrRunActive`。

**关键边界：工具集是运行时对象、不可序列化**——检查点保存"发生了什么"，不保存"能做什么"，Resume 时由调用方重新提供。

### 恢复对账

检查点与事件是两笔独立写入，进程可能在两者之间崩溃。Resume 时与事件库对账：

- **孤儿结果**：已执行且有结果记录、但未被检查点覆盖的调用——模型重新发起同样调用（工具名+参数匹配）时**直接复用记录结果，不重复执行**；
- **不确定调用**：已开始执行（`tool_started`）但没有结果记录，副作用可能已发生——按工具的 `tool.RetryPolicy` 分级：

| 分级 | 恢复行为 |
|---|---|
| `RetryPolicyRetrySafe`（默认） | 重新执行 |
| `RetryPolicyNeedsVerify` / `RetryPolicyNoRetry` | **本轮运行内持续阻断**（模型重试不能绕过），返回处置指引让模型先核实外部状态或转人工 |

```go
type myTool struct{ tool.Func }
func (t myTool) RetryPolicy() tool.RetryPolicy { return tool.RetryPolicyNoRetry }
```

- **协议安全边界**：调用级检查点可能停在步中途（assistant 带多个调用、仅部分回执）。Resume 先把轨迹回退到最后一个完整边界（悬空的 tool_calls 会被部分模型服务拒绝），被丢弃段中已完成的调用由上面的对账机制复用，不丢失。

### fail-closed：落库失败即中止

事件、检查点、状态登记任一持久化失败都会**中止运行并把错误传给调用方**——持久化是恢复语义的前提，宁可失败也不静默丢记录后假装可以恢复。这些行为以已配置 Store 为前提；nil Store 不持久化，内存 Store 不跨进程。存储故障时失败状态本身也可能无法保存。

## 事件的三种读法

```go
// 1. Replay：回放全部已持久化事件（顺序即发生顺序）
events, err := rt.Replay(ctx, runID)

// 2. Subscribe：实时订阅（缓冲 256，慢消费者会丢事件；运行结束时通道关闭）
ch, cancel := rt.Subscribe(runID)
defer cancel()

// 3. Stream：回放 + 实时 + 缺口补读 —— 面向当前尝试补读并跟随事件
err = rt.Stream(ctx, runID, afterSeq, func(e agent.Event) { /* SSE 转发 */ })
```

`Stream` 是前端断线重连的标准答案：`afterSeq` 传客户端最后收到的 Seq（0 表示从头），先补齐历史，再追实时；实时通道丢事件时检测 Seq 缺口自动从 Store 补读；运行已结束时做最终补齐并兜底合成终态事件。补读依赖可用 EventStore。Stream 会过滤旧尝试终态，也可能合成终态提示；原始审计用 Replay，不保证两者逐条相同。

## 审批跨 HTTP 请求

```go
// 运行 goroutine：等待在 Broker 上
rt.Run(ctx, msgs, nil, agent.Config{RunID: runID, Tools: tools}, callModel, emit)

// 审批端点（另一个 HTTP 请求）：
rec, _ := rt.Get(ctx, runID) // rec.Status == waiting_approval, rec.PendingApproval 有详情
err := rt.Approve(runID, callID, true) // 解除阻塞，运行继续
```

等待期间取消（`rt.Cancel(runID)` 或调用方 ctx 取消）按拒绝处理并中止该调用；运行结束时全部等待按拒绝关闭，不泄漏。

## Cancel 与 ctx

- `rt.Cancel(runID)`：显式取消，登记为 `canceled`；
- 调用方 ctx 取消：同样视为取消；
- **前端断开 ≠ 取消**：把 Run 放在脱离请求生命周期的 context 上（如 `context.Background()` 派生），断线只影响 SSE 连接，重连后用 `Stream(afterSeq)` 续传。

## Recover 与单进程边界

进程重启后先调 `rt.Recover(ctx)`：把停留在 pending/running/waiting_approval 的登记统一标记为 `failed`（附恢复提示），返回受影响数量；之后按需对每个 runID `Resume`。

> 首版为**单进程模型**：多实例并发恢复同一 RunID 需外部协调（租约/执行权校验），本库不提供。外部副作用的"恰好一次"需要工具与业务系统配合（如幂等键）。

## Store 接口

三个 Store 全部接口注入，可只配部分（nil 跳过对应能力）：

| 接口 | 语义要点 |
|---|---|
| `RunStore` | `Create/Update/Get/List`；Update 是全量覆盖 |
| `EventStore` | `Append`（append-only）/ `List`（按发生顺序） |
| `CheckpointStore` | `Save`（同 RunID 覆盖）/ `Load`（无检查点返回 `ErrNoCheckpoint`） |

内存实现见 `runtime/memory`；SQLite 实现（含 `OpenRuntime` 开箱预设）见 `storage/sqlite`。见 [自定义存储](../扩展手册/自定义存储.md)。
