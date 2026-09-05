package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/runtime/memory"
	"github.com/dingkui/dlz-goai/tool"
)

// ==== 测试基础设施 ====

// countingCheckpointStore 记录每次保存的检查点，用于验证调用级检查点。
type countingCheckpointStore struct {
	memory.CheckpointStore
	saved []runtime.Checkpoint
}

func newCountingStore() *countingCheckpointStore {
	return &countingCheckpointStore{CheckpointStore: *memory.NewCheckpointStore()}
}

func (s *countingCheckpointStore) Save(_ context.Context, cp runtime.Checkpoint) error {
	s.saved = append(s.saved, cp)
	return s.CheckpointStore.Save(context.Background(), cp)
}

// failingEventStore 第 failAfter 次 Append 起返回错误，模拟落库故障。
type failingEventStore struct {
	memory.EventStore
	failAfter int
	count     int
}

func (s *failingEventStore) Append(ctx context.Context, runID string, events ...agent.Event) error {
	s.count++
	if s.count > s.failAfter {
		return errors.New("disk full")
	}
	return s.EventStore.Append(ctx, runID, events...)
}

// classifiedTool 带 RetryPolicy 分级的工具。
type classifiedTool struct {
	tool.Func
	policy tool.RetryPolicy
}

func (t *classifiedTool) RetryPolicy() tool.RetryPolicy { return t.policy }

func classifiedToolOf(name string, policy tool.RetryPolicy, counter *int) tool.Tool {
	base := tool.NewFunc(name, name+"工具", nil, true,
		func(context.Context, map[string]any) (tool.Result, error) {
			*counter++
			return tool.Text(name + "-executed"), nil
		})
	return &classifiedTool{Func: base, policy: policy}
}

func alwaysApprove() agent.ApprovalHandler {
	return agent.ApprovalFunc(func(context.Context, agent.ApprovalRequest) (bool, error) {
		return true, nil
	})
}

// ==== 恢复对账：崩溃窗口 A（结果已落库、检查点未覆盖）===

// 场景：两个调用并发轨迹中，第二个调用的 tool_result 事件已落库，
// 但进程在调用级检查点保存前崩溃。Resume 后模型重新发起同样调用，
// 应复用记录的结果、不重复执行（"已确认完成的步骤不重做"）；
// 参数不同的调用正常执行。
func TestResumeReusesRecordedToolResult(t *testing.T) {
	runs := memory.NewRunStore()
	events := memory.NewEventStore()
	cps := memory.NewCheckpointStore()
	runID := "run-window-a"

	// 登记：上次运行已失败
	_ = runs.Create(context.Background(), runtime.RunRecord{ID: runID, Status: runtime.StatusFailed})
	// 检查点：只覆盖到第一个调用（user + assistant(两个调用) + tool(c1)）
	_ = cps.Save(context.Background(), runtime.Checkpoint{
		RunID: runID, Step: 1,
		Messages: []message.Message{
			{Role: message.RoleUser, Content: "执行两个操作"},
			{Role: message.RoleAssistant, ToolCalls: []tool.Call{
				{ID: "c1", Function: tool.Function{Name: "boom", Arguments: []byte(`{"q":"a"}`)}},
				{ID: "c2", Function: tool.Function{Name: "boom", Arguments: []byte(`{"q":"b"}`)}},
			}},
			{Role: message.RoleTool, ToolCallID: "c1", ToolName: "boom", Content: "boom:a"},
		},
	})
	// 事件库：两个调用都有完整记录——c2 的结果是检查点未覆盖的孤儿
	seed := []agent.Event{
		{Type: agent.EventRunStart, RunID: runID},
		{Type: agent.EventToolProposed, RunID: runID, CallID: "c1", ToolName: "boom", Arguments: map[string]any{"q": "a"}},
		{Type: agent.EventToolStarted, RunID: runID, CallID: "c1", ToolName: "boom"},
		{Type: agent.EventToolResult, RunID: runID, CallID: "c1", ToolName: "boom", Result: "boom:a"},
		{Type: agent.EventToolProposed, RunID: runID, CallID: "c2", ToolName: "boom", Arguments: map[string]any{"q": "b"}},
		{Type: agent.EventToolStarted, RunID: runID, CallID: "c2", ToolName: "boom"},
		{Type: agent.EventToolResult, RunID: runID, CallID: "c2", ToolName: "boom", Result: "boom:b"},
		{Type: agent.EventRunError, RunID: runID, Error: "进程中断"},
	}
	_ = events.Append(context.Background(), runID, seed...)

	executions := 0
	boom := tool.NewFunc("boom", "测试工具", nil, true,
		func(_ context.Context, args map[string]any) (tool.Result, error) {
			executions++
			return tool.Text("boom:" + args["q"].(string)), nil
		})

	rt := runtime.New(runtime.Options{Runs: runs, Events: events, Checkpoints: cps})

	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		switch step {
		case 1:
			// 模型重新发起与 c2 相同的调用（新 CallID，参数一致）
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "n1", Name: "boom", Arguments: `{"q":"b"}`},
			}})
		case 2:
			// 再发起一个参数不同的新调用——不应被复用
			if got := msgs[len(msgs)-1].Content; got != "boom:b" {
				t.Fatalf("复用结果不符，期望 boom:b，得到 %q", got)
			}
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "n2", Name: "boom", Arguments: `{"q":"fresh"}`},
			}})
		default:
			if got := msgs[len(msgs)-1].Content; got != "boom:fresh" {
				t.Fatalf("新调用结果不符，期望 boom:fresh，得到 %q", got)
			}
			emit(message.Delta{Content: "全部完成"})
		}
		return nil
	}

	result, err := rt.Resume(context.Background(), runID,
		agent.Config{Tools: []tool.Tool{boom}}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "全部完成" {
		t.Fatalf("最终结果不符: %+v", result)
	}
	// 孤儿结果被复用：只有参数不同的新调用真正执行过
	if executions != 1 {
		t.Fatalf("工具应只执行 1 次（新调用），实际 %d 次——孤儿结果未被复用", executions)
	}
	// 恢复事件应包含 run_resumed
	replayed, _ := rt.Replay(context.Background(), runID)
	resumed := false
	for _, e := range replayed {
		if e.Type == agent.EventRunResumed {
			resumed = true
		}
	}
	if !resumed {
		t.Fatal("回放应包含 run_resumed 事件")
	}
}

// ==== 恢复对账：崩溃窗口 B（已开始执行、无结果记录）===

// 场景：tool_started 已落库但没有 tool_result——副作用可能已发生。
// NoRetry 工具不自动重试，返回提示让模型先核实；RetrySafe 工具正常重执行。
func TestResumeInflightNoRetry(t *testing.T) {
	runID := "run-window-b-noretry"
	runs := memory.NewRunStore()
	events := memory.NewEventStore()
	cps := memory.NewCheckpointStore()

	_ = runs.Create(context.Background(), runtime.RunRecord{ID: runID, Status: runtime.StatusFailed})
	_ = cps.Save(context.Background(), runtime.Checkpoint{
		RunID: runID, Step: 1,
		Messages: []message.Message{
			{Role: message.RoleUser, Content: "扣款"},
			{Role: message.RoleAssistant, ToolCalls: []tool.Call{
				{ID: "c1", Function: tool.Function{Name: "charge", Arguments: []byte(`{"amount":10}`)}},
			}},
		},
	})
	_ = events.Append(context.Background(), runID,
		agent.Event{Type: agent.EventToolProposed, RunID: runID, CallID: "c1", ToolName: "charge", Arguments: map[string]any{"amount": float64(10)}},
		agent.Event{Type: agent.EventToolStarted, RunID: runID, CallID: "c1", ToolName: "charge"},
		agent.Event{Type: agent.EventRunError, RunID: runID, Error: "进程中断"},
	)

	executions := 0
	charge := classifiedToolOf("charge", tool.RetryPolicyNoRetry, &executions)
	rt := runtime.New(runtime.Options{Runs: runs, Events: events, Checkpoints: cps})

	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "n1", Name: "charge", Arguments: `{"amount":10}`},
			}})
			return nil
		}
		last := msgs[len(msgs)-1]
		if !strings.Contains(last.Content, "Recovery notice") {
			t.Fatalf("应收到恢复提示，得到 %q", last.Content)
		}
		emit(message.Delta{Content: "已转人工"})
		return nil
	}

	result, err := rt.Resume(context.Background(), runID,
		agent.Config{Tools: []tool.Tool{charge}, Approve: alwaysApprove()}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "已转人工" || executions != 0 {
		t.Fatalf("NoRetry 工具不应执行，结果不符: executions=%d result=%q", executions, result.Content)
	}
}

// RetrySafe（默认）：窗口 B 调用正常重新执行。
func TestResumeInflightRetrySafe(t *testing.T) {
	runID := "run-window-b-safe"
	runs := memory.NewRunStore()
	events := memory.NewEventStore()
	cps := memory.NewCheckpointStore()

	_ = runs.Create(context.Background(), runtime.RunRecord{ID: runID, Status: runtime.StatusFailed})
	_ = cps.Save(context.Background(), runtime.Checkpoint{
		RunID: runID, Step: 1,
		Messages: []message.Message{
			{Role: message.RoleUser, Content: "查询"},
			{Role: message.RoleAssistant, ToolCalls: []tool.Call{
				{ID: "c1", Function: tool.Function{Name: "lookup", Arguments: []byte(`{"id":1}`)}},
			}},
		},
	})
	_ = events.Append(context.Background(), runID,
		agent.Event{Type: agent.EventToolProposed, RunID: runID, CallID: "c1", ToolName: "lookup", Arguments: map[string]any{"id": float64(1)}},
		agent.Event{Type: agent.EventToolStarted, RunID: runID, CallID: "c1", ToolName: "lookup"},
		agent.Event{Type: agent.EventRunError, RunID: runID, Error: "进程中断"},
	)

	executions := 0
	lookup := classifiedToolOf("lookup", tool.RetryPolicyRetrySafe, &executions)
	rt := runtime.New(runtime.Options{Runs: runs, Events: events, Checkpoints: cps})

	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "n1", Name: "lookup", Arguments: `{"id":1}`},
			}})
			return nil
		}
		if got := msgs[len(msgs)-1].Content; got != "lookup-executed" {
			t.Fatalf("应重新执行，得到 %q", got)
		}
		emit(message.Delta{Content: "完成"})
		return nil
	}

	if _, err := rt.Resume(context.Background(), runID,
		agent.Config{Tools: []tool.Tool{lookup}}, model, nil); err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatalf("RetrySafe 工具应执行 1 次，实际 %d", executions)
	}
}

// ==== 持久化失败 fail-closed ====

// 事件落库失败：运行中止并呈现持久化错误，状态 failed——
// 不静默丢失记录后假装可以恢复。
func TestEventPersistFailClosed(t *testing.T) {
	failStore := &failingEventStore{EventStore: *memory.NewEventStore(), failAfter: 2}
	cps := newCountingStore()
	rt := runtime.New(runtime.Options{
		Runs:        memory.NewRunStore(),
		Events:      failStore,
		Checkpoints: cps,
	})
	runID := rt.BeginRun()

	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		emit(message.Delta{Content: "回答"})
		return nil
	}
	_, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echoTool()}}, model, nil)
	if err == nil || !strings.Contains(err.Error(), "persistence failed") {
		t.Fatalf("应返回持久化失败错误，得到 %v", err)
	}
	rec, _ := rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusFailed {
		t.Fatalf("应为 failed，得到 %s", rec.Status)
	}
}

// ==== 调用级检查点 ====

// 每条工具回执入轨迹后即存检查点：一次含两个调用的步应有
// callIndex=0、callIndex=1 两个调用级检查点，消息数递增。
func TestCallLevelCheckpoint(t *testing.T) {
	cps := newCountingStore()
	rt := runtime.New(runtime.Options{
		Runs:        memory.NewRunStore(),
		Events:      memory.NewEventStore(),
		Checkpoints: cps,
	})
	runID := rt.BeginRun()

	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "echo", Arguments: `{"q":"1"}`},
				{Index: 1, ID: "c2", Name: "echo", Arguments: `{"q":"2"}`},
			}})
			return nil
		}
		emit(message.Delta{Content: "完成"})
		return nil
	}
	if _, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echoTool()}}, model, nil); err != nil {
		t.Fatal(err)
	}

	var callLevel []runtime.Checkpoint
	for _, cp := range cps.saved {
		if cp.CallIndex >= 0 {
			callLevel = append(callLevel, cp)
		}
	}
	if len(callLevel) != 2 {
		t.Fatalf("应保存 2 个调用级检查点，实际 %d 个（共 %d 次保存）", len(callLevel), len(cps.saved))
	}
	// 第一个调用级检查点：1 条 tool 消息；第二个：2 条
	if got := len(callLevel[0].Messages); got != 3 {
		t.Fatalf("callIndex=0 检查点应含 3 条消息（user+assistant+tool），实际 %d", got)
	}
	if got := len(callLevel[1].Messages); got != 4 {
		t.Fatalf("callIndex=1 检查点应含 4 条消息，实际 %d", got)
	}
	if callLevel[0].CallIndex != 0 || callLevel[1].CallIndex != 1 {
		t.Fatalf("CallIndex 应为 0、1，实际 %d、%d", callLevel[0].CallIndex, callLevel[1].CallIndex)
	}
}
