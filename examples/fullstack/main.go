// Fullstack demonstrates Client, simulated approval, cancellation, resumption,
// and replay in one process with in-memory stores. It is not a crash test.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	dlzgoai "github.com/dingkui/dlz-goai"
	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/tool"
)

type assignInput struct {
	TicketID string `json:"ticket_id"`
	Assignee string `json:"assignee"`
}

type noRetry struct{ tool.Tool }

func (noRetry) IsReadOnly() bool              { return false }
func (noRetry) RetryPolicy() tool.RetryPolicy { return tool.RetryPolicyNoRetry }

// The model derives its next action from messages, so resumed calls need no
// hidden step counter. The tool is simulated; no external system is modified.
func model(ctx context.Context, msgs []message.Message, _ *message.Options, emit func(message.Delta)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == message.RoleTool {
		emit(message.Delta{Content: "工具回执：" + msgs[len(msgs)-1].Content})
		return nil
	}
	emit(message.Delta{ToolCalls: []tool.CallDelta{{
		Index: 0, ID: "assign-1", Name: "assign_ticket",
		Arguments: `{"ticket_id":"T-1024","assignee":"zhang"}`,
	}}})
	return nil
}

func waitApproval(ctx context.Context, client *dlzgoai.Client, id string) (*runtime.PendingApproval, error) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		rec, err := client.GetRun(ctx, id)
		if err != nil && !errors.Is(err, runtime.ErrRunNotFound) {
			return nil, err
		}
		if err == nil {
			if rec.Status == runtime.StatusWaitingApproval && rec.PendingApproval != nil {
				return rec.PendingApproval, nil
			}
			// Resume registers asynchronously; the previous canceled attempt may still be visible.
			if rec.Status.Terminal() && rec.Status != runtime.StatusCanceled {
				return nil, fmt.Errorf("run ended before approval: %s %s", rec.Status, rec.Error)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func demo() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	executions := 0
	assign := noRetry{tool.Typed("assign_ticket", "指派工单（模拟写操作）", false,
		func(_ context.Context, in assignInput) (tool.Result, error) {
			if in.TicketID == "" || in.Assignee == "" {
				return tool.Error("工单号和处理人必填"), nil
			}
			executions++
			return tool.Text(fmt.Sprintf("%s 已指派给 %s", in.TicketID, in.Assignee)), nil
		})}
	client, err := dlzgoai.NewClient(dlzgoai.Options{Model: model, Tools: []tool.Tool{assign}})
	if err != nil {
		return err
	}
	defer client.Close()

	run, err := client.Start(ctx, dlzgoai.Request{Input: "把工单 T-1024 指派给 zhang"})
	if err != nil {
		return err
	}
	pending, err := waitApproval(ctx, client, run.ID())
	if err != nil {
		return err
	}
	fmt.Printf("[等待审批] %s\n", pending.ToolName)
	run.Cancel()
	if _, err := run.Wait(ctx); err == nil {
		return errors.New("expected cancellation")
	}
	fmt.Println("[模拟中断] 已显式取消；进程与内存 Store 仍在")

	resumed, err := client.Resume(ctx, run.ID())
	if err != nil {
		return err
	}
	pending, err = waitApproval(ctx, client, resumed.ID())
	if err != nil {
		return err
	}
	// Simulate a user decision. Production code authenticates and authorizes
	// the approver at a separate endpoint, rather than automatically approving.
	for {
		err = client.Approve(resumed.ID(), pending.CallID, true)
		if !errors.Is(err, agent.ErrApprovalNotPending) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	fmt.Println("[模拟审批] 允许本次工单指派")
	result, err := resumed.Wait(ctx)
	if err != nil {
		return err
	}
	fmt.Println("[完成]", result.Content)
	events, err := client.Replay(ctx, run.ID())
	if err != nil {
		return err
	}
	fmt.Printf("[回放] %d 条事件；本次演示工具实际执行 %d 次\n", len(events), executions)
	if executions != 1 {
		return fmt.Errorf("unexpected demo execution count: %d", executions)
	}
	return nil
}

func main() {
	if err := demo(); err != nil {
		log.Fatal(err)
	}
}
