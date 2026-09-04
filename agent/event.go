package agent

import (
	"github.com/dingkui/dlz-goai/tool"
)

// 事件类型。一次运行按时间顺序发出这些事件，
// 调用方可据此驱动 SSE、日志或持久化审计。
const (
	// EventRunStart 运行开始。
	EventRunStart = "run_start"
	// EventModelDelta 模型增量输出（可能是文本，也可能只有结束原因）。
	EventModelDelta = "model_delta"
	// EventToolProposed 模型请求调用某工具，尚未校验策略。
	EventToolProposed = "tool_proposed"
	// EventApprovalRequired 该调用需要人工确认，正在等待。
	EventApprovalRequired = "approval_required"
	// EventToolStarted 工具开始执行。
	EventToolStarted = "tool_started"
	// EventToolResult 工具执行成功并返回内容。
	EventToolResult = "tool_result"
	// EventToolError 工具执行失败或被拒绝。
	EventToolError = "tool_error"
	// EventStepDone 一个步骤结束（模型调用 + 其全部工具调用）。
	EventStepDone = "step_done"
	// EventFinal 运行结束，Content 为最终回答。
	EventFinal = "final"
)

// Event 是 Runner 向调用方发出的结构化运行事件。
type Event struct {
	Type      string          `json:"type"`
	RunID     string          `json:"runId,omitempty"`
	Step      int             `json:"step,omitempty"`
	Content   string          `json:"content,omitempty"`
	CallID    string          `json:"callId,omitempty"`
	ToolName  string          `json:"toolName,omitempty"`
	SourceID   string              `json:"sourceId,omitempty"`
	SourceName string              `json:"sourceName,omitempty"`
	Arguments map[string]any  `json:"arguments,omitempty"`
	Result    string          `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	Approved  *bool           `json:"approved,omitempty"`
	Citations []tool.Citation `json:"citations,omitempty"`
	Finish    string          `json:"finish,omitempty"`
}

// Emitter 运行事件的接收端。为 nil 时 Runner 静默丢弃事件。
// 用函数类型而非接口，是为了让调用方能直接传闭包。
type Emitter func(Event)

// Emit 安全调用：nil 接收端不做任何事。
func (e Emitter) Emit(event Event) {
	if e != nil {
		e(event)
	}
}
