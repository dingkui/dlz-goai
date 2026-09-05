# 实战案例：接入现有 Go 项目

本篇不讲包 API（那是各使用手册的事），讲**你手里有一个正在运行的 Go 服务，如何一步步加上"可审批、可恢复的 Agent 执行能力"**。全程贯穿一个虚构但典型的场景，配套可运行示例见 [`examples/fullstack`](../examples/fullstack/main.go)（模拟模型，无需任何 API key，`go run ./examples/fullstack` 直接跑）。

> 生产级参考：[ModelBox](https://github.com/dingkui/ollama-manager)（桌面应用）已用本库跑通模型调用、工具循环、MCP 接入与 Runtime 持久化——本篇的接入路径就是从那里提炼的。

## 1. 场景与起点

假设你维护一个内部工单服务：

```go
// 已有的业务代码：HTTP API + service 层
func (s *TicketService) GetTicket(ctx context.Context, id string) (*Ticket, error)
func (s *TicketService) AssignTicket(ctx context.Context, id, assignee string) error
```

目标：加一个"AI 助理"端点——它能查询工单、在用户确认后执行指派，指派请求等待确认期间服务重启也能续跑，前端断线重连不丢事件。

**不目标**：换掉现有 HTTP 框架、引入消息队列、部署独立 AI 平台。

## 2. 第一步：接入模型调用（约 10 分钟）

```bash
go get github.com/dingkui/dlz-goai
```

在现有服务里加一个包 `internal/ai`，把模型调用收敛到一处：

```go
package ai

import (
	"github.com/dingkui/dlz-goai/factory"
	"github.com/dingkui/dlz-goai/llm"
)

func NewProvider() llm.Provider {
	return factory.NewProvider(llm.Service{
		Kind:    llm.KindOpenAI, // 或 llm.KindOllama
		BaseURL: "https://api.deepseek.com",
		APIKey:  apiKeyFromEnv(), // 从配置/环境变量取，不进代码
	})
}
```

挂到现有 HTTP handler，流式返回（`text/event-stream`）：

```go
func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	var req struct{ Content string `json:"content"` }
	_ = json.NewDecoder(r.Body).Decode(&req)

	flusher := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	err := h.provider.ChatStream(r.Context(), "deepseek-chat",
		[]message.Message{{Role: message.RoleUser, Content: req.Content}}, nil,
		func(d message.Delta) {
			if d.Content != "" {
				fmt.Fprintf(w, "data: %s\n\n", d.Content)
				flusher.Flush()
			}
		})
	_ = err
}
```

到这里模型已经能说话了。但注意：`r.Context()` 意味着**前端断开 = 生成终止**——对简单对话这是正确行为，第 6 步会讲长任务怎么处理。

## 3. 第二步：把业务函数变成工具

工具就是普通 Go 函数。**只读查询**声明 `readOnly=true`，默认直接执行：

```go
lookup := tool.NewFunc("lookup_ticket", "查询工单当前状态", map[string]any{
	"type": "object",
	"properties": map[string]any{"ticket_id": map[string]any{"type": "string"}},
	"required":   []string{"ticket_id"},
}, true, func(ctx context.Context, args map[string]any) (tool.Result, error) {
	ticket, err := h.tickets.GetTicket(ctx, args["ticket_id"].(string))
	if err != nil {
		return tool.Result{}, err // 执行失败：终止本轮
	}
	if ticket == nil {
		return tool.Error("工单不存在"), nil // 业务失败：回传模型，它会向用户解释
	}
	return tool.Text(ticket.JSON()), nil
})
```

**写操作**不声明只读——它会默认进入审批流程（第 5 步）。偏好结构体参数时用 `tool.Typed`，schema 自动生成、参数自动绑定。

判定"业务失败 vs 执行失败"的标准：**模型还能为这个失败做什么吗？** 能（换个工单号重查）→ `tool.Error`；不能（数据库连不上）→ `error`。

## 4. 第三步：挂上 Agent 循环

```go
callModel := func(ctx context.Context, msgs []message.Message, opts *message.Options, cb func(message.Delta)) error {
	return h.provider.ChatStream(ctx, "deepseek-chat", msgs, opts, cb)
}

result, err := agent.New().Run(ctx, msgs, nil,
	agent.Config{Tools: []tool.Tool{lookup, assign}},
	callModel,
	func(e agent.Event) { /* 事件 → SSE（见下） */ })
```

事件转 SSE 的要点（ModelBox 的做法）：

- `model_delta` → `data:` 帧实时转发；
- **写 ResponseWriter 要互斥**（如果有心跳 goroutine 并发写）；
- 每 15 秒发一行 `: ping` 注释防代理掐断空闲连接——审批等待时连接可能长时间无数据；
- `tool_proposed`/`tool_result` 转成结构化事件，前端渲染"AI 正在调用 X"卡片。

## 5. 第四步：加人工审批

写操作默认 `confirm` 策略。**不要**在 SSE 连接里等用户点击——正确姿势是 Broker 解耦：

```go
// 运行 goroutine：cfg.Approve 留空，runtime 会自动绑定内部 Broker（见第 6 步）；
// 裸用 agent 时：broker := agent.NewBroker(); runID := broker.Begin(); cfg.Approve = broker.For(runID)
```

事件流里出现 `approval_required` 时，把 `runID + callID` 推给前端渲染确认卡片；用户点击后，**另一个 HTTP 端点**提交决策：

```go
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID    string `json:"runId"`
		CallID   string `json:"callId"`
		Approved bool   `json:"approved"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.rt.Approve(req.RunID, req.CallID, req.Approved); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
	}
}
```

这样设计的原因：审批等待可能持续几分钟，SSE 连接随时会断；把"等待"与"决策"解耦后，断线重连不影响审批进行。运行结束（或被取消）时所有未决审批自动按拒绝关闭，不会泄漏。

安全默认值要记住：**Approve 未配置时非只读工具一律拒绝**；`Policies` 可按工具覆盖为 `auto`/`confirm`/`deny`；`deny` 的调用模型会收到错误回执并改道。

## 6. 第五步：持久化与恢复

到第 4 步为止，运行还是进程内一次性的。工单指派这类长任务需要：审批等待期间崩溃能续跑、前端断线重连能补事件。换用 `runtime`：

```go
// 启动时装配一次（WAL 模式，纯 Go 无 CGO）
db, opts, err := sqlite.OpenRuntime("runs.db")
if err != nil { log.Fatal(err) }
defer db.Close()
rt := runtime.New(opts)

// 运行改走 runtime；ctx 用应用生命周期而不是请求生命周期——
// 这是"前端断开 ≠ 取消"的关键
runCtx := context.Background() // 或从 srv.BaseContext 派生
go func() {
	runID := rt.BeginRun()
	_, _ = rt.Run(runCtx, msgs, nil,
		agent.Config{RunID: runID, Tools: tools, RunID: runID}, callModel, eventForwarder)
}()
```

进程启动时恢复遗留登记，并提供两个运维端点：

```go
func (h *Handler) Startup(ctx context.Context) {
	rt.Recover(ctx) // 停留在非终态的登记标记为 failed（附恢复提示）
}

// 断线重连：afterSeq = 客户端最后收到的事件序号，补齐后接实时流
func (h *Handler) Stream(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runId")
	lastSeq, _ := strconv.ParseInt(r.URL.Query().Get("afterSeq"), 10, 64)
	rt.Stream(r.Context(), runID, lastSeq, sseEmitter(w)) // Seq 连续、含终态事件
}

// 续跑端点：人工确认后从中断点继续（工具由调用方重新提供）
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runId")
	_, err := h.rt.Resume(r.Context(), runID,
		agent.Config{Tools: h.tools, Approve: h.approver}, h.callModel, nil)
	_ = err
}
```

## 7. 进程重启演练（完整时序）

以"指派工单"为例，`examples/fullstack` 可直接跑出这条时间线：

```
用户: "把工单 T-1024 指派给 zhang"
  → 模型发起 assign_ticket
  → 策略判定 confirm → runtime 登记 waiting_approval，事件落库
  → 【进程崩溃 / 用户关页面】
重启后:
  → Recover：非终态登记标记 failed（"可从检查点恢复"）
  → 用户在界面看到中断提示，点"继续"
  → Resume：从检查点恢复轨迹（运行开始即有初始检查点，审批等待前必可恢复）
  → 模型重新发起 assign_ticket → 审批通过 → 工具执行（恰好一次）
  → run_resumed 事件落库 → 前端 Stream 补齐中断期间的全部事件
```

对账语义（为什么不会重复执行或丢失）见 [runtime 手册](使用手册/runtime.md)：已确认完成的调用复用记录结果；已开始但无结果记录的调用按工具的 `tool.RetryPolicy` 分级——指派这类写操作声明 `RetryPolicyNoRetry` 后，恢复时**持续阻断**并要求人工处置，模型重试不能绕过。

## 8. 反模式清单（都是真实踩过的坑）

| 反模式 | 后果 | 正确做法 |
|---|---|---|
| `rt.Run` 直接用 `r.Context()` | 前端断开 = 任务终止，恢复形同虚设 | 运行用应用生命周期 ctx；请求 ctx 只给 SSE 转发用 |
| 自己维护 `map[runID]context.CancelFunc` 管理运行 | 与 runtime 内部状态漂移，Cancel 失灵 | 用 `rt.Cancel(runID)`，活跃运行由 runtime 管理 |
| 审批决策从 SSE 连接里读 | 断线即死锁 | 决策走独立 HTTP 端点（Broker 模式） |
| 所有工具都声明只读图省事 | 危险操作被静默放行 | 只读声明留给真无副作用的查询；拿不准就让它走审批 |
| 吞掉 `Run` 返回的持久化错误 | 数据库断连后状态与事实漂移 | fail-closed 错误必须呈现给用户/日志 |
| 每次请求 new 一个 Runtime | 连接与刷新循环堆积 | 应用启动装配一次，进程内共享 |
| 前端断线后从头重放 | 大流量浪费 + UI 闪烁 | 记住最后 Seq，`Stream(ctx, runID, afterSeq, emit)` 增量续传 |

## 9. 验收清单

接入完成后逐项勾选：

- [ ] 杀掉进程再启动，`Recover` 能列出中断的运行，`Resume` 从检查点续跑且已完成的工具调用不重复执行
- [ ] 审批等待期间杀进程，重启后续跑时该调用重新走审批（不会静默执行）
- [ ] 前端断开 SSE 再重连（带 afterSeq），事件连续无缺口，终态事件必达
- [ ] 未配置 Approve 时，非只读工具全部被拒绝（而不是报错崩溃）
- [ ] 声明了 `RetryPolicyNoRetry` 的工具，恢复后模型反复重试也无法绕过阻断
- [ ] 模型不支持工具调用时，报错信息可读且不影响普通对话（空工具集退化为普通流式调用）

---

**下一步**：需要接入外部 MCP 工具（文件系统、数据库、第三方服务）→ [mcp 手册](使用手册/mcp.md)；需要知识库检索 → [rag 手册](使用手册/rag.md)；把运行状态换成自己的存储 → [storage 手册](使用手册/storage.md)。
