// fullstack 示例：一个"工单系统"接入 Agent 的最小闭环——
// 工具循环、人工审批、持久化运行、中断续跑、事件回放。
// 全程使用模拟模型，无需任何 API key 或本地服务。
//
//	go run ./examples/fullstack
//
// 真实项目差异只有两点：模型桩换成 provider（ollama/openai），
// 内存 Store 换成 sqlite.OpenRuntime("runs.db")。见 docs/实战案例-接入现有Go项目.md。
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/runtime/memory"
	"github.com/dingkui/dlz-goai/tool"
)

func main() {
	// ---- 应用启动：装配 Runtime（真实项目用 sqlite.OpenRuntime("runs.db")）----
	rt := runtime.New(runtime.Options{
		Runs:        memory.NewRunStore(),
		Events:      memory.NewEventStore(),
		Checkpoints: memory.NewCheckpointStore(),
	})

	// ---- 把现有业务函数包装成工具 ----
	// 只读工具（readOnly=true）：默认直接执行
	lookup := tool.NewFunc("lookup_ticket", "查询工单当前状态", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ticket_id": map[string]any{"type": "string"},
		},
		"required": []string{"ticket_id"},
	}, true, func(_ context.Context, args map[string]any) (tool.Result, error) {
		return tool.Text(`{"id":"T-1024","status":"open","priority":"high"}`), nil
	})
	// 写工具（readOnly=false）：默认需要人工审批
	assign := tool.NewFunc("assign_ticket", "把工单指派给工程师（写操作）", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ticket_id": map[string]any{"type": "string"},
			"assignee":  map[string]any{"type": "string"},
		},
		"required": []string{"ticket_id", "assignee"},
	}, false, func(_ context.Context, args map[string]any) (tool.Result, error) {
		return tool.Text(fmt.Sprintf("工单 %v 已指派给 %v", args["ticket_id"], args["assignee"])), nil
	})
	tools := []tool.Tool{lookup, assign}

	// 模拟模型（真实项目 = provider.ChatStream 的一行适配）：
	// 第一步发起指派调用；被打断时把错误传回去。
	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, cb func(message.Delta)) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		step++
		switch step {
		case 1:
			cb(message.Delta{ToolCalls: []tool.CallDelta{{
				Index: 0, ID: "c1", Name: "assign_ticket",
				Arguments: `{"ticket_id":"T-1024","assignee":"zhang"}`,
			}}})
			return nil
		default:
			cb(message.Delta{Content: "工单已指派完成。"})
			return nil
		}
	}

	runID := rt.BeginRun()

	// ---- 第一次运行：在审批等待时被中断（真实场景 = 进程崩溃 / 用户关掉页面）----
	runDone := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(),
			[]message.Message{{Role: message.RoleUser, Content: "把工单 T-1024 指派给 zhang"}},
			nil, agent.Config{RunID: runID, Tools: tools}, model, nil)
		runDone <- err
	}()

	waitStatus(rt, runID, runtime.StatusWaitingApproval)
	rec, _ := rt.Get(context.Background(), runID)
	fmt.Printf("[中断] 运行在等待审批 %s(%v) 时被终止\n",
		rec.PendingApproval.ToolName, rec.PendingApproval.Arguments)
	rt.Cancel(runID)
	if err := <-runDone; err == nil {
		panic("被取消的运行应返回错误")
	}

	// ---- 模拟进程重启：恢复登记 ----
	// 本示例用 Cancel 中断，登记已是终态，Recover 返回 0；
	// 真实崩溃后（登记停在 running/waiting_approval）Recover 会把它们
	// 标记为 failed 并附"可从检查点恢复"提示。
	n, _ := rt.Recover(context.Background())
	fmt.Printf("[重启] Recover 标记了 %d 条非终态登记\n", n)

	// ---- 续跑：工具由调用方重新提供，这次审批在界面上被批准 ----
	step = 0 // 模型从头开始（轨迹由检查点恢复，仍只有原始用户消息）
	approve := agent.ApprovalFunc(func(context.Context, agent.ApprovalRequest) (bool, error) {
		fmt.Println("[审批] 用户在界面上点击了允许")
		return true, nil
	})
	result, err := rt.Resume(context.Background(), runID,
		agent.Config{RunID: runID, Tools: tools, Approve: approve}, model, nil)
	if err != nil {
		panic(err)
	}
	fmt.Printf("[完成] %s\n", result.Content)

	// ---- 事件回放：完整轨迹（含 run_resumed），可驱动审计或 UI 重放 ----
	events, _ := rt.Replay(context.Background(), runID)
	fmt.Printf("\n[事件回放] 共 %d 条:", len(events))
	for _, e := range events {
		fmt.Printf(" %s", e.Type)
	}
	fmt.Println()
}

// waitStatus 轮询等待运行进入指定状态（真实场景由事件驱动，无需轮询）。
func waitStatus(rt *runtime.Runtime, runID string, want runtime.Status) {
	for i := 0; i < 1000; i++ {
		rec, err := rt.Get(context.Background(), runID)
		if err == nil && rec.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	panic("等待状态超时: " + string(want))
}
