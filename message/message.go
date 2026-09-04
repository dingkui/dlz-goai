// Package message 定义与模型厂商无关的对话数据契约。
//
// 本包只依赖 tool，可被 llm、provider、agent 共享而不产生环。
package message

import (
	"github.com/dingkui/dlz-goai/tool"
)

// Role 取值。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message 一条对话消息。同一个结构体承载四种角色：
// 普通文本、模型发起的工具调用（ToolCalls）、工具回传结果（ToolCallID/ToolName）、
// 以及多模态图片（Images，base64 data URI）。
type Message struct {
	Role       string      `json:"role"`
	Content    string      `json:"content"`
	Images     []string    `json:"images,omitempty"`
	ToolCalls  []tool.Call `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	ToolName   string      `json:"tool_name,omitempty"`
	Citations  []Citation  `json:"-"` // 引用不进模型载荷，由调用方单独持久化
}

// Citation 是 tool.Citation 的别名。
// 引用定义在 tool 包以维持依赖单向（tool 零依赖），此处仅为书写便利。
type Citation = tool.Citation

// Delta 流式增量。Content 与 ToolCalls 可能同时为空（例如纯结束帧）。
type Delta struct {
	Content      string
	ToolCalls    []tool.CallDelta
	FinishReason string
	Done         bool
	Error        string
	// 用量统计。Ollama 在 done 帧提供；OpenAI 兼容服务通常为 0，调用方按需隐藏。
	PromptTokens int
	EvalTokens   int
	EvalMs       int64
	TotalMs      int64
}

// Options 推理可选参数。零值表示全部交给服务端默认。
type Options struct {
	Temperature *float64
	NumCtx      *int
	System      string
	Tools       []tool.Definition
	ToolChoice  string
}
