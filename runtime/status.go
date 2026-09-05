// Package runtime 在 agent.Runner 之上提供持久化运行：
// 状态登记、事件落库、调用级检查点、断点续跑（含事件对账）、
// 事件回放与订阅。
//
// 存储全部走接口注入（RunStore/EventStore/CheckpointStore），
// 本包自带内存实现（runtime/memory），SQLite 等持久实现由调用方提供。
//
// 恢复语义：已确认完成的调用不重复执行（事件对账复用）；
// 已开始执行但结果未记录的调用按工具的 tool.RetryPolicy 分级处理。
// 外部副作用的恰好一次需要工具与业务系统配合，本库不承诺。
//
// 首版为单进程模型：多实例并发恢复同一 RunID 需外部协调（租约/执行权），
// 本库不提供。
//
// 依赖方向：runtime → agent + message + tool，不反向依赖任何应用。
package runtime

import (
	"errors"
	"time"
)

// Status 一次持久化运行的生命周期状态。
type Status string

const (
	// StatusPending 已登记未开始。
	StatusPending Status = "pending"
	// StatusRunning 运行中。
	StatusRunning Status = "running"
	// StatusWaitingApproval 阻塞在工具审批上。
	StatusWaitingApproval Status = "waiting_approval"
	// StatusSucceeded 正常结束。
	StatusSucceeded Status = "succeeded"
	// StatusFailed 出错终止（Error 字段有原因）。
	StatusFailed Status = "failed"
	// StatusCanceled 被调用方取消。
	StatusCanceled Status = "canceled"
)

// Terminal 终态判定：终态的运行不会再变化。
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled:
		return true
	}
	return false
}

// ErrNoCheckpoint 指定运行没有检查点（从未跑过或未配置检查点存储）。
var ErrNoCheckpoint = errors.New("runtime: checkpoint not found")

// ErrRunNotFound 指定运行不存在。
var ErrRunNotFound = errors.New("runtime: run not found")

// ErrRunTerminal 运行已成功结束，不能再次发起或续跑。
var ErrRunTerminal = errors.New("runtime: run already finished")

// ErrRunActive 运行仍处于 pending/running/waiting_approval，不能重复发起或续跑。
var ErrRunActive = errors.New("runtime: run still in progress")

// PendingApproval 等待审批的调用信息（WaitingApproval 状态时有值）。
// 进程重启后调用方可据此重建“中断前曾等待审批”的界面；原等待器
// 已随进程消失，不能直接 Approve，需由用户确认后通过 Resume 重新执行。
type PendingApproval struct {
	CallID     string         `json:"callId"`
	ToolName   string         `json:"toolName"`
	SourceName string         `json:"sourceName,omitempty"`
	Arguments  map[string]any `json:"arguments,omitempty"`
}

// RunRecord 一次运行的登记信息。Model 等标注字段由调用方解释，
// Runtime 只负责状态机的推进与落库。
type RunRecord struct {
	ID              string           `json:"id"`
	Status          Status           `json:"status"`
	CreatedAt       int64            `json:"createdAt"`
	UpdatedAt       int64            `json:"updatedAt"`
	Error           string           `json:"error,omitempty"`
	PendingApproval *PendingApproval `json:"pendingApproval,omitempty"`
	LastStep        int              `json:"lastStep,omitempty"`
	Model           string           `json:"model,omitempty"`
}

func now() int64 { return time.Now().UnixMilli() }
