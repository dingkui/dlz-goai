package runtime

import (
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/tool"
)

// Checkpoint 一次运行的可恢复快照：到某一步为止的完整消息轨迹、
// 累积引用与初始推理参数。
//
// 工具集（agent.Config.Tools）是运行时对象、不可序列化，
// 因此 Resume 时由调用方重新提供——这是刻意的边界：
// 检查点保存"发生了什么"，不保存"能做什么"。
type Checkpoint struct {
	RunID     string           `json:"runId"`
	Step      int              `json:"step"`
	// CallIndex 该检查点保存时机所处步内的调用序号（调用级检查点）；
	// -1 或缺省表示步级检查点（整步结束）。
	CallIndex int              `json:"callIndex,omitempty"`
	Messages  []message.Message `json:"messages"`
	Citations []tool.Citation  `json:"citations,omitempty"`
	Options   *message.Options `json:"options,omitempty"`
	CreatedAt int64            `json:"createdAt"`
}
