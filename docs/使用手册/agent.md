# 使用手册：agent — 工具调用循环

`agent` 是本库的核心：模型 → 工具 → 模型的受控循环，带审批、事件流、并行执行与三层防线。它只依赖 `message` 与 `tool`，不认识任何模型厂商和 MCP——工具来自哪里由调用方决定。

## 心智模型

```
用户消息 ──► 模型(流式) ──► 要工具？──否──► 最终回答（EventFinal）
                 │                    是
                 ▼
           逐个执行工具（策略判定 → 审批 → 带超时执行 → 结果截断）
                 │
                 ▼
           tool 消息回写轨迹 ──► 回到模型（下一步）
```

步数用尽时不报错：去掉工具再调一次模型做**兜底总结**，已完成的工作不会因步数上限变成错误。

## 快速上手

```go
tools := []tool.Tool{
	tool.NewFunc("get_time", "获取当前本地时间", nil, true,
		func(context.Context, map[string]any) (tool.Result, error) {
			return tool.Text(time.Now().Format(time.RFC3339)), nil
		}),
}

result, err := agent.New().Run(ctx,
	[]message.Message{{Role: message.RoleUser, Content: "现在几点了？"}},
	nil,
	agent.Config{Tools: tools},
	callModel, // ModelFunc，见下文
	nil,       // Emitter：不要事件流就传 nil
)
```

**空工具集不报错**：自动退化为一次普通流式模型调用，便于普通对话与 Agent 共用同一套 Runtime、事件和重连协议。

## Config 全字段

```go
type Config struct {
	Tools              []tool.Tool        // 允许模型调用的工具
	Policies           map[string]tool.Policy // 按工具名覆盖策略（auto/confirm/deny）
	MaxSteps           int                // 步骤上限；0 用默认 6，硬上限 20
	ToolTimeout        time.Duration      // 单工具超时；0 用默认 45s
	MaxToolResultBytes int                // 单结果截断阈值；0 用默认 128KB
	Approve            ApprovalHandler    // 审批器；confirm 策略且 nil 时拒绝
	ToolExecution      ToolExecution      // 默认串行；ToolParallel 并行
	OnStep             func(step int, messages []message.Message)
	OnToolDone         func(step, callIndex int, call tool.Call, result message.Message, messages []message.Message)
	Transform          func(messages []message.Message) []message.Message // 每轮调模型前裁剪上下文
	RunID              string             // 透传到事件，供 runtime 关联
}
```

`Result` 返回：`Content`（最终回答）、`Messages`（完整轨迹，中断时也保留已流出部分）、`Steps`、`FinishReason`（`stop` / `max_steps`）、`Citations`（引用汇总去重）、用量统计。

## ModelFunc — 与厂商解耦的接缝

agent 不 import 任何 provider。一行闭包完成适配：

```go
callModel := func(ctx context.Context, msgs []message.Message, opts *message.Options, cb func(message.Delta)) error {
	return provider.ChatStream(ctx, "qwen3:8b", msgs, opts, cb)
}
```

这也让测试极简单——用函数桩模拟"先调工具再回答"，不需要真实模型服务（runtime 的全部恢复测试都是这么写的）。

## 工具的两种失败，两条通道

| 通道 | 写法 | 行为 |
|---|---|---|
| 业务失败 | `return tool.Error("记录不存在"), nil` | 原因写进 tool 消息**回传模型**，给它自我纠正的机会 |
| 执行失败 | `return tool.Result{}, err` | 转换为工具错误回执交回模型，循环可继续 |

当前 Agent 会将这两种工具失败都交回模型。真正导致运行结束的错误见 [错误处理](../指南/错误处理.md)。

参数解析失败、模型请求未注册的工具、策略禁止、用户拒绝——都走业务失败通道回传模型。

## 审批：默认即安全

策略判定顺序：`Config.Policies` 显式配置 → 工具实现 `tool.ReadOnlyTool` 且 `IsReadOnly()==true` → `auto` → **其余一律 `confirm`**。

```go
// 方式一：简单回调（同步场景）
cfg.Approve = agent.ApprovalFunc(func(ctx context.Context, req agent.ApprovalRequest) (bool, error) {
	return req.ToolName == "safe_tool", nil
})

// 方式二：Broker —— 流式运行与外部决策解耦（Web 界面场景）
broker := agent.NewBroker()
runID := broker.Begin()
cfg.Approve = broker.For(runID)
// 审批请求到达时（EventApprovalRequired 事件），把 runID+callID 暴露给界面；
// 用户点击后从另一个 HTTP 请求提交决策：
broker.Resolve(runID, callID, true)
```

规则：

- 裸 Agent 在 confirm 策略且 Approve 为 nil 时拒绝；显式 auto/deny 优先。Client/Runtime 默认绑定 Broker 等待审批。
- 审批等待被中断（ctx 取消、运行结束）返回错误并中止该调用；裸 Agent 使用者需在运行结束时调用 broker.End(runID)；Runtime 管理其 Broker 生命周期。
- 每次决策通过 `EventApprovalRequired` 事件推送（含 `Approved *bool` 字段标注决议结果）。

## 事件流

`Emitter` 是 `func(Event)`，每收到一个事件回调一次（nil 安全）：

| 事件 | 触发时机 | 关键字段 |
|---|---|---|
| `run_start` | 运行开始 | |
| `model_delta` | 模型正文增量 | `Content` |
| `tool_proposed` | 模型请求调用（尚未校验策略） | `CallID` `ToolName` `Arguments` |
| `approval_required` | 等待审批 / 决议完成（`Approved` 非 nil） | `CallID` `Arguments` `Approved` |
| `tool_started` | 工具开始执行 | `CallID` |
| `tool_result` / `tool_error` | 工具返回 / 业务失败 | `Result` `Citations` |
| `step_done` | 一步结束（模型调用 + 全部工具） | `Step` |
| `final` | 运行结束 | `Content` `Finish` `Citations` |

事件里的工具带来源标注（`SourceID`/`SourceName`，MCP 工具来自其 adapter），前端可区分"本地工具"与"某 MCP 服务"。

## 并行执行

默认串行，事件与消息顺序完全稳定。一次响应含多个工具调用时可并行：

```go
agent.Config{Tools: tools, ToolExecution: agent.ToolParallel}
```

结果仍按调用顺序写回（协议要求 tool 消息与调用一一对应），仅事件到达顺序不保证。

## 三层防线

1. **步数上限**：`MaxSteps`（默认 6，硬上限 20）防死循环；用尽后去工具兜底总结。
2. **单工具超时**：`ToolTimeout`（默认 45s），通过 ctx 传递超时；工具必须响应 ctx，不会强制终止忽略取消的函数。
3. **结果截断**：单结果超过 `MaxToolResultBytes`（默认 128KB）按 UTF-8 安全边界截断并标注——直接按字节切会产生非法 UTF-8，让整个请求被服务端拒绝。

**中断韧性**：用户停止、连接断开时，已流出的正文保留在 `Result.Messages` 轨迹里——界面上显示过的文字不丢失。

## 钩子

- `Transform`：每轮调模型前裁剪/改写上下文（长对话截断、历史压缩挂这里）。
- `OnStep`：每步结束回调（runtime 用它做步级检查点）。
- `OnToolDone`：每条工具回执写入轨迹后回调（runtime 用它做调用级检查点；并行模式下仍按调用顺序触发）。

三个都是可选钩子，不设置时行为不变。

## 引用（Citations）

工具返回 `tool.Result{Citations: []tool.Citation{...}}` 时，引用随事件与 tool 消息流转，运行结束时在 `Result.Citations` 汇总去重（按 URL 或 DocID）。字段覆盖互联网来源（URL）与本地知识库来源（DocID/RelPath/Section）。
