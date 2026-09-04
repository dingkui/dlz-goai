// MCP 示例：连接一个 stdio 传输的 MCP 服务端，把它的工具喂给 agent。
//
// 本示例假定本地有 npx，可用任何 MCP 服务端替换 Command/Args。
//	go run ./examples/mcp
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/mcp"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/provider/ollama"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 1. 配置并连接 MCP 服务端
	manager := mcp.NewManager()
	defer manager.Close()
	manager.Apply([]mcp.Server{{
		ID: "files", Name: "文件系统", Enabled: true,
		Transport: mcp.TransportStdio,
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", "."},
	}})

	// 2. 等待首次工具发现完成，避免启动竞态导致工具列表为空
	manager.EnsureReady(ctx)

	// 3. 取该服务的全部工具——agent 只看到 []tool.Tool，不知道 MCP 的存在
	tools := manager.ToolsOf([]string{"files"})
	fmt.Printf("已发现 %d 个工具\n", len(tools))
	for _, t := range tools {
		def := t.Definition()
		fmt.Printf("  - %s: %s\n", def.Name, def.Description)
	}

	// 4. 运行
	provider := ollama.New("")
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, cb func(message.Delta)) error {
		return provider.ChatStream(ctx, "qwen3:8b", msgs, opts, cb)
	}
	result, err := agent.New().Run(
		ctx,
		[]message.Message{{Role: message.RoleUser, Content: "列出当前目录下的 go 文件"}},
		nil,
		agent.Config{Tools: tools},
		model,
		func(e agent.Event) {
			switch e.Type {
			case agent.EventModelDelta:
				fmt.Print(e.Content)
			case agent.EventToolResult:
				fmt.Printf("\n[工具 %s 返回]\n%s\n", e.ToolName, e.Result)
			}
		},
	)
	if err != nil {
		fmt.Println("\n运行失败:", err)
		return
	}
	fmt.Printf("\n步骤数: %d\n", result.Steps)
}
