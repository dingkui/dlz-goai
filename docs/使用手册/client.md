# 使用手册：Client — 应用接入入口

Client 在应用启动时装配一次，管理后台运行及结果等待。导入 `dlzgoai "github.com/dingkui/dlz-goai"`。本文描述当前 master 的实验性门面（v0.1.0 不含后续恢复修复），尚无 Session、命名 Registry 或链式 DefaultConfig。

完整可运行入门见 [快速开始](../快速开始.md)，业务示例见 [fullstack](../../examples/fullstack/main.go)。

## 装配与关闭

`NewClient(dlzgoai.Options{Model: callModel, Tools: tools})` 创建 Client；Model 可省略并在 Request.Model 中逐次提供，Tools 是默认工具集，Runtime 不填时使用内存存储。共享模型和工具实现需要支持并发调用。

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

`Close` 取消受管运行并等待其退出，不关闭应用注入的数据库或模型资源。工具和 Provider 必须响应 ctx 取消，否则关闭可能持续等待。完成结果及恢复配置默认保留最近 128 条；Options.MaxCompletedRuns 可调整，负数禁用缓存。Forget(runID) 主动释放完成记录。数据库历史不受影响，已有 Run 句柄仍保留该次执行的结果。

## 版本差异

v0.1.0 是最初门面；当前 master 增加逐请求模型、恢复配置快照、提交就绪、重复恢复保护和缓存上限。依赖 v0.1.0 的应用应先升级到包含这些修改的提交。

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

Start 成功返回时，运行登记和初始检查点已经写入所配置的 Store，可以立即调用 GetRun/Stream。模型与工具继续在后台执行；执行错误从 Wait 返回。内存 Store 的写入不代表跨进程持久化。

## 生命周期

| 调用 | ctx 与结果 |
|---|---|
| `Start / Resume` | 检查提交 ctx，等待登记就绪后返回；成功提交后的执行不绑定请求 ctx。 |
| `Wait` | ctx 取消只停止等待；运行继续。 |
| `Cancel` | 显式请求取消，再用 Wait 或状态确认结束。bool 表示找到受管记录并提交取消，不表示已执行回滚。 |
| `GetRun` | 查询登记，包括持久化的历史运行，不推进执行。 |
| `Close` | 停止受管运行并清理内存结果。 |

Wait 仅适用于当前 Client 管理的运行。重启后查询旧运行用 GetRun/Replay；需要继续失败或取消的运行时调用 Resume，然后 Wait 新句柄。

## 审批与事件

默认只读工具自动执行，其他工具使用 confirm；显式 Policies 优先。Client 使用 Runtime Broker 等待审批，通过 `client.Approve(runID, callID, approved)` 提交决策。批准前应由应用校验当前用户对运行和操作的权限。

默认 Broker 在等待器注册后再发布待审批状态及事件，收到事件即可提交决策。重复审批或已经结束的等待仍会返回错误，应用应刷新状态。自定义审批器可实现 agent.ReadyApprovalHandler 保持相同就绪顺序。详见 [事件集成](../指南/事件与流式集成.md)。

| 方法 | 用途 |
|---|---|
| `Subscribe` | 实时通知，缓冲有限会丢事件；不用于完整重放。 |
| `Replay` | 全部已保存历史，适合审计。 |
| `Stream(ctx, runID, afterSeq, emit)` | 按序号补读并跟随实时流，适合页面重连；需要可用的 RunStore 与 EventStore。 |

## 实验性恢复

恢复行为仍在验证中，不作可靠性保证。内存 Store 不跨进程；SQLite 保存状态不代表外部副作用恰好一次。

同一 Client 中，`Resume(ctx, runID)` 使用该运行缓存的模型、工具、Policies 和执行限制，配置中的 slice/map 已复制。重复恢复活动运行返回 `runtime.ErrRunActive`；旧 Run 句柄不会被新一次执行替换。

重启或缓存淘汰后，应用通过 `ResumeWith(ctx, runID, originalRequest)` 显式提供原模型、工具和策略，或在 Options.ResumeResolver 中按 runID 重建配置。缺少原配置返回 `ErrResumeConfigRequired`，不会静默使用默认模型和工具。ResumeWith 的 Model 必填；Tools 为完整权限集，nil 表示无工具。输入和推理参数从检查点加载。

应用可先保存模型选择、实际工具白名单和策略，再以 `Request.RunID` 提交相同 ID。只持久化稳定名称和业务配置，密钥由应用的服务配置加载。恢复时重新检查业务授权；原工具缺失或权限撤销时应明确拒绝。

进程启动时，在独占执行权的前提下先执行 `client.Runtime().Recover(ctx)`。它标记被打断的运行，不自动续跑；随后由应用选择需要恢复的 runID。Resume 的状态校验和准备错误直接返回，后续执行错误由新句柄 Wait 返回。

[HTTP 示例](../../examples/http/main.go) 提供启动、查询、SSE 重连、取消、恢复和审批接口，使用确定性模型，无需 API Key。生产应用在自己的路由层接入身份认证与业务授权。

恢复测试包括审批等待、工具副作用后、检查点写入前、终态提交前四个真实子进程硬退出场景。这些测试是当前行为证据，不构成外部副作用恰好一次的保证。详细分级见 [Runtime 手册](runtime.md)。