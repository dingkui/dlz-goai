// Package memory 提供 RunStore / EventStore / CheckpointStore 的内存实现。
// 适合开发调试、单元测试与不需要跨进程恢复的场景；
// 数据随进程退出而消失。
//
// 三个接口分开实现（RunStore 与 EventStore 的 List 签名不同，
// Go 无方法重载，无法共用一个结构体）。
package memory

import (
	"context"
	"sync"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/runtime"
)

var _ runtime.RunStore = (*RunStore)(nil)
var _ runtime.EventStore = (*EventStore)(nil)
var _ runtime.CheckpointStore = (*CheckpointStore)(nil)

// RunStore 运行登记的内存实现。
type RunStore struct {
	mu   sync.RWMutex
	runs map[string]runtime.RunRecord
}

// NewRunStore 构造。
func NewRunStore() *RunStore {
	return &RunStore{runs: map[string]runtime.RunRecord{}}
}

func (s *RunStore) Create(_ context.Context, record runtime.RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[record.ID] = record
	return nil
}

func (s *RunStore) Update(_ context.Context, record runtime.RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[record.ID] = record
	return nil
}

func (s *RunStore) Get(_ context.Context, runID string) (runtime.RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.runs[runID]
	if !ok {
		return runtime.RunRecord{}, runtime.ErrRunNotFound
	}
	return record, nil
}

func (s *RunStore) List(_ context.Context, statuses ...runtime.Status) ([]runtime.RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	want := map[runtime.Status]bool{}
	for _, status := range statuses {
		want[status] = true
	}
	var out []runtime.RunRecord
	for _, record := range s.runs {
		if len(want) == 0 || want[record.Status] {
			out = append(out, record)
		}
	}
	return out, nil
}

// EventStore 运行事件的内存实现（append-only）。
type EventStore struct {
	mu     sync.RWMutex
	events map[string][]agent.Event
}

// NewEventStore 构造。
func NewEventStore() *EventStore {
	return &EventStore{events: map[string][]agent.Event{}}
}

func (s *EventStore) Append(_ context.Context, runID string, events ...agent.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[runID] = append(s.events[runID], events...)
	return nil
}

func (s *EventStore) List(_ context.Context, runID string) ([]agent.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]agent.Event(nil), s.events[runID]...), nil
}

// CheckpointStore 检查点的内存实现。
type CheckpointStore struct {
	mu   sync.RWMutex
	cps  map[string]runtime.Checkpoint
}

// NewCheckpointStore 构造。
func NewCheckpointStore() *CheckpointStore {
	return &CheckpointStore{cps: map[string]runtime.Checkpoint{}}
}

func (s *CheckpointStore) Save(_ context.Context, cp runtime.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cps[cp.RunID] = cp
	return nil
}

func (s *CheckpointStore) Load(_ context.Context, runID string) (runtime.Checkpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp, ok := s.cps[runID]
	if !ok {
		return runtime.Checkpoint{}, runtime.ErrNoCheckpoint
	}
	return cp, nil
}
