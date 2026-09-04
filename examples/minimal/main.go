// 最小示例：本地 Ollama + 一个进程内工具的完整工具调用循环。
//
// 运行前提：本机 Ollama 已启动且拉取了模型（按需修改 model 变量）。
//	go run ./examples/minimal
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/provider/ollama"
	"github.com/dingkui/dlz-goai/tool"
)

func main() {
	provider := ollama.New("") // 空 = http://127.0.0.1:11434
	model := "qwen3:8b"

	// 工具就是普通 Go 函数；只读工具默认直接执行
	tools := []tool.Tool{
		tool.NewFunc("get_time", "获取当前本地时间", nil, true,
			func(context.Context, map[string]any) (tool.Result, error) {
				return tool.Text(time.Now().Format(time.RFC3339)), nil
			}),
	}

	// ModelFunc 把 provider 适配给 agent——agent 由此不认识任何厂商
	callModel := func(ctx context.Context, msgs []message.Message, opts *message.Options, cb func(message.Delta)) error {
		return provider.ChatStream(ctx, model, msgs, opts, cb)
	}

	result, err := agent.New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "现在几点了？用一句话回答"}},
		nil,
		agent.Config{Tools: tools},
		callModel,
		func(e agent.Event) {
			switch e.Type {
			case agent.EventModelDelta:
				fmt.Print(e.Content)
			case agent.EventToolResult:
				fmt.Printf("\n[工具 %s 返回 %s]\n", e.ToolName, e.Result)
			}
		},
	)
	if err != nil {
		fmt.Println("\n运行失败:", err)
		return
	}
	fmt.Printf("\n\n步骤数: %d, 生成 tokens: %d\n", result.Steps, result.EvalTokens)
}
