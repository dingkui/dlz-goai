package runtime

import (
	"context"

	"github.com/dingkui/dlz-goai/agent"
)

// RunStore 运行登记的持久化接口。
// Update 是全量覆盖语义：调用方先 Get 再改字段，避免部分更新的并发歧义。
type RunStore interface {
	// Create 登记一次新运行。ID 重复由实现决定是否报错。
	Create(ctx context.Context, record RunRecord) error
	// Update 全量更新一条登记。
	Update(ctx context.Context, record RunRecord) error
	// Get 按 ID 查找。不存在时返回 ErrRunNotFound。
	Get(ctx context.Context, runID string) (RunRecord, error)
	// List 按状态过滤；不传状态返回全部。
	List(ctx context.Context, statuses ...Status) ([]RunRecord, error)
}

// EventStore 运行事件的 append-only 持久化。
// 事件顺序即到达顺序，回放按此顺序还原过程。
type EventStore interface {
	// Append 追加事件（允许批量）。
	Append(ctx context.Context, runID string, events ...agent.Event) error
	// List 回放某次运行的全部事件。运行不存在时返回空切片而非错误。
	List(ctx context.Context, runID string) ([]agent.Event, error)
}

// CheckpointStore 检查点持久化。每次保存覆盖同一 RunID 的旧检查点——
// 只需最新可恢复点，保留历史属于实现方的自由。
type CheckpointStore interface {
	// Save 保存（覆盖）检查点。
	Save(ctx context.Context, cp Checkpoint) error
	// Load 读取最新检查点。没有检查点时返回 ErrNoCheckpoint。
	Load(ctx context.Context, runID string) (Checkpoint, error)
}
