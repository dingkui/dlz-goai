package mcp

import (
	"context"

	"github.com/dingkui/dlz-goai/tool"
)

// mcpTool 把一个 MCP 工具包装成本库统一的 tool.Tool。
//
// 这是"原生工具与 MCP 工具统一"的接缝：agent 只看到 tool.Tool，
// 完全不知道背后是一次 JSON-RPC 调用还是一次函数调用。
type mcpTool struct {
	fullName string
	source   ToolWithSource
	caller   ToolCaller
}

// ToolCaller 抽象工具调用入口，Manager 自然满足；
// 测试时可以注入桩实现而不必起真实的 MCP 服务端。
type ToolCaller interface {
	CallTool(ctx context.Context, fullName string, args map[string]any) (CallToolResult, error)
}

var _ tool.Tool = mcpTool{}
var _ tool.ReadOnlyTool = mcpTool{}

func (t mcpTool) Definition() tool.Definition {
	return tool.Definition{
		Name:        t.fullName,
		Description: t.source.Tool.Description,
		Parameters:  t.source.Tool.InputSchema,
	}
}

// IsReadOnly 透传服务端的 readOnlyHint 注解。
// 未声明时按有副作用处理，由 agent 侧默认要求审批。
func (t mcpTool) IsReadOnly() bool { return t.source.ReadOnly() }

func (t mcpTool) Execute(ctx context.Context, args map[string]any) (tool.Result, error) {
	result, err := t.caller.CallTool(ctx, t.fullName, args)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: result.Text(), IsError: result.IsError}, nil
}

// Adapt 把单个带来源的 MCP 工具适配成 tool.Tool。
func Adapt(source ToolWithSource, caller ToolCaller) tool.Tool {
	return mcpTool{fullName: source.FullName(), source: source, caller: caller}
}

// AdaptAll 批量适配。
func AdaptAll(sources []ToolWithSource, caller ToolCaller) []tool.Tool {
	out := make([]tool.Tool, 0, len(sources))
	for _, source := range sources {
		out = append(out, Adapt(source, caller))
	}
	return out
}

// Tools 返回指定服务上、命中白名单的工具，已适配为 tool.Tool。
// agent.Config.Tools 可以直接使用返回值——
// 调用方由此获得"从注册表挑工具喂给 agent"的完整闭环，且不产生对 mcp 包的依赖。
// 白名单为空表示不选任何工具，与不做限制语义相反（见 tool.Registry.Select）。
func (m *Manager) Tools(serverIDs, allowList []string) []tool.Tool {
	return AdaptAll(m.ToolsFor(serverIDs, allowList), m)
}

// ToolsOf 返回指定服务的全部已缓存工具（不做白名单过滤）。
// 服务 ID 未配置时返回空。
func (m *Manager) ToolsOf(serverIDs []string) []tool.Tool {
	if len(serverIDs) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(serverIDs))
	for _, id := range serverIDs {
		wanted[id] = struct{}{}
	}
	var sources []ToolWithSource
	for _, source := range m.AllTools() { // AllTools 已按名字排序
		if _, ok := wanted[source.ServerID]; ok {
			sources = append(sources, source)
		}
	}
	return AdaptAll(sources, m)
}
