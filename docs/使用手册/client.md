# 使用手册：Client — 应用接入入口

Client 在应用启动时装配一次，管理后台运行及结果等待。导入 `dlzgoai "github.com/dingkui/dlz-goai"`。v0.1.0 提供的是实验性最小门面，尚无 Session、命名 Registry 或链式 DefaultConfig。

完整可运行入门见 [快速开始](../快速开始.md)，业务示例见 [fullstack](../../examples/fullstack/main.go)。

## 装配与关闭

`NewClient(dlzgoai.Options{Model: callModel, Tools: tools})` 创建 Client；Model 必填，Tools 是默认工具集，Runtime 不填时使用内存存储。共享模型和工具实现需要支持并发调用。

SQLite 装配片段（导入 `runtime` 与 `storage/sqlite`）：

```go
// 放在应用主生命周期函数内，不能在短暂的构造函数里 defer 关闭数据库。
db, opts, err := sqlite.OpenRuntime("runs.db")
if err != nil { return err }
defer db.Close()
rt := runtime.New(opts)
// 只在独占执行权的进程启动阶段处理遗留运行，不能每次 HTTP 请求都调用。
if _, err := rt.Recover(ctx); err != nil { return err }
client, err := dlzgoai.NewClient(dlzgoai.Options{Model: callModel, Tools: tools, Runtime: rt})
if err != nil { return err }
defer client.Close() // 后注册的 defer 先执行：先停止任务，再关闭数据库。
// 在此启动并等待应用服务退出。
```

`Close` 取消受管运行并等待其退出，不关闭应用注入的数据库或模型资源。工具和 Provider 必须响应 ctx 取消，否则关闭可能持续等待。已完成运行的结果目前保留到 Close，没有自动淘汰策略。

## 请求与结果

| 字段 | 当前行为 |
|---|---|
| `Input` | 简便的单条 user 输入。 |
| `Messages` | 非空时优先于 Input；多轮历史由应用加载和传入。 |
| `Options` | 模型参数，如 System、Temperature、MaxTokens。 |
| `Tools` | nil 使用 Client 默认；非 nil 覆盖，空 slice 可清空工具。 |
| `Policies` | 按工具名配置 auto / confirm / deny，由服务端业务策略生成。 |
| `MaxSteps / ToolTimeout / MaxToolResultBytes / ToolExecution` | 透传 Agent 执行配置，详见 [Agent](agent.md)。 |

```go
run, err := client.Start(ctx, dlzgoai.Request{Input: "查询工单"})
if err != nil { return err }
result, err := run.Wait(ctx)
if err != nil { return err }
// result.Content 是最终文本，result.Messages 是本轮完整轨迹。
```

Start 返回句柄后，执行与持久化仍在后台进行。后台错误从 Wait 返回；GetRun/Stream 紧接 Start 调用时，可能尚未完成运行登记，遇到 `runtime.ErrRunNotFound` 应在请求期限内短暂重试。不要将取得句柄当作持久提交成功。

## 生命周期

| 调用 | ctx 与结果 |
|---|---|
| `Start / Resume` | 立即返回句柄，后台执行不绑定请求 ctx。v0.1.0 当前不检查传入 ctx；应用如需拒绝已取消的提交，应先检查 ctx.Err()。 |
| `Wait` | ctx 取消只停止等待；运行继续。 |
| `Cancel` | 显式请求取消，再用 Wait 或状态确认结束。bool 表示找到受管记录并提交取消，不表示已执行回滚。 |
| `GetRun` | 查询登记，包括持久化的历史运行，不推进执行。 |
| `Close` | 停止受管运行并清理内存结果。 |

Wait 仅适用于当前 Client 管理的运行。重启后查询旧运行用 GetRun/Replay；需要继续失败或取消的运行时调用 Resume，然后 Wait 新句柄。

## 审批与事件

默认只读工具自动执行，其他工具使用 confirm；显式 Policies 优先。Client 使用 Runtime Broker 等待审批，通过 `client.Approve(runID, callID, approved)` 提交决策。批准前应由应用校验当前用户对运行和操作的权限。

`approval_required` 事件可能早于 Broker 等待器注册；提交遇到 `agent.ErrApprovalNotPending` 时，应重新读取当前审批状态并允许短暂重试，不能默认批准。完整事件与 HTTP 接入方式见 [事件集成](../指南/事件与流式集成.md)。

| 方法 | 用途 |
|---|---|
| `Subscribe` | 实时通知，缓冲有限会丢事件；不用于完整重放。 |
| `Replay` | 全部已保存历史，适合审计。 |
| `Stream(ctx, runID, afterSeq, emit)` | 按序号补读并跟随实时流，适合页面重连；需要可用的 RunStore 与 EventStore。 |

## 实验性恢复

恢复行为仍在验证中，不作可靠性保证。内存 Store 不跨进程；SQLite 保存状态不代表外部副作用恰好一次。

启动时先对遗留记录执行 `client.Runtime().Recover(ctx)`，再按业务确认的 runID 调用 `client.Resume(ctx, runID)`。Recover 返回受影响数量，不返回运行列表，也不自动续跑。Resume 的状态校验或执行失败在 Wait 中呈现。

当前 Resume 使用 Client 默认工具集和新的 Agent 默认配置，不恢复此前 Request 的 Tools、Policies、步骤限制等全部覆盖项。应用需要确保恢复所用工具和授权符合原业务要求；需精确配置时使用底层 Runtime.Resume。不要重复并发 Resume 同一 runID。

恢复分级、检查点和单进程边界的详细说明集中在 [Runtime 手册](runtime.md)。
