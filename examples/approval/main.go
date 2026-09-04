// 审批示例：有副作用的工具在执行前等待确认。
//
// 真实应用里，确认通常来自另一个 HTTP 端点（用户在界面上点"允许"），
// 与流式运行并发。本示例用 goroutine 模拟这一过程。
//	go run ./examples/approval
package main

import (
	"context"
	"fmt"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/provider/ollama"
	"github.com/dingkui/dlz-goai/tool"
)

func main() {
	// 有副作用的工具：不声明只读，默认就要走审批
	deleteTool := tool.NewFunc("send_email", "发送一封邮件", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"to":      map[string]any{"type": "string"},
			"subject": map[string]any{"type": "string"},
		},
		"required": []string{"to"},
	}, false, func(_ context.Context, args map[string]any) (tool.Result, error) {
		return tool.Text(fmt.Sprintf("已发送给 %v", args["to"])), nil
	})

	// Broker 把"运行中的等待"与"外部的决策"解耦
	broker := agent.NewBroker()
	runID := broker.Begin()
	defer broker.End(runID)

	provider := ollama.New("")
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, cb func(message.Delta)) error {
		return provider.ChatStream(ctx, "qwen3:8b", msgs, opts, cb)
	}

	result, err := agent.New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "给 a@example.com 发一封问好邮件"}},
		nil,
		agent.Config{
			Tools:   []tool.Tool{deleteTool},
			Approve: broker.For(runID), // 绑定本次运行的审批器
		},
		model,
		func(e agent.Event) {
			switch e.Type {
			case agent.EventModelDelta:
				fmt.Print(e.Content)
			case agent.EventApprovalRequired:
				fmt.Printf("\n[等待确认] %s %v\n", e.ToolName, e.Arguments)
				// 真实应用在这里把 callID 暴露给界面，等用户决策
				_ = broker.Resolve(runID, e.CallID, true) // 示例直接批准
			}
		},
	)
	if err != nil {
		fmt.Println("\n运行失败:", err)
		return
	}
	fmt.Printf("\n最终回答: %s\n", result.Content)
}
