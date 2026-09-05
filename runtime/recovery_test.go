package runtime_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/runtime/memory"
	"github.com/dingkui/dlz-goai/storage/sqlite"
	"github.com/dingkui/dlz-goai/tool"
)

// ==== 测试辅助 ====

// failingUpdateRunStore 从第 failFrom 次 Update 起返回错误。
type failingUpdateRunStore struct {
	memory.RunStore
	failFrom int
	updates  int
}

func (s *failingUpdateRunStore) Update(ctx context.Context, record runtime.RunRecord) error {
	s.updates++
	if s.updates >= s.failFrom {
		return errors.New("disk full")
	}
	return s.RunStore.Update(ctx, record)
}

// terminalFailingEventStore 仅在写入终态事件（run_done/run_error）时失败。
type terminalFailingEventStore struct {
	memory.EventStore
}

func (s *terminalFailingEventStore) Append(ctx context.Context, runID string, events ...agent.Event) error {
	for _, e := range events {
		if e.Type == agent.EventRunDone || e.Type == agent.EventRunError {
			return errors.New("disk full")
		}
	}
	return s.EventStore.Append(ctx, runID, events...)
}

// flakyCheckpointStore 从第 failFrom 次 Save 起失败（不落盘），
// 用于模拟"工具已执行、事件已落库、检查点未写入"的崩溃窗口。
type flakyCheckpointStore struct {
	runtime.CheckpointStore
	failFrom int
	saves    int
}

func (s *flakyCheckpointStore) Save(ctx context.Context, cp runtime.Checkpoint) error {
	s.saves++
	if s.saves >= s.failFrom {
		return errors.New("disk full")
	}
	return s.CheckpointStore.Save(ctx, cp)
}

func plainModel(content string) func(context.Context, []message.Message, *message.Options, func(message.Delta)) error {
	return func(_ context.Context, _ []message.Message, _ *message.Options, emit func(message.Delta)) error {
		emit(message.Delta{Content: content})
		return nil
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// ==== P1-1：状态登记写入失败必须传到调用方 ===

// 变体一：所有 Update 失败 → 启动即报错，登记停在 pending，
// 不允许带着未落库状态继续执行后返回成功。
func TestStatusPersistFailureAtStartup(t *testing.T) {
	runs := &failingUpdateRunStore{RunStore: *memory.NewRunStore(), failFrom: 1}
	rt := runtime.New(runtime.Options{
		Runs: runs, Events: memory.NewEventStore(), Checkpoints: memory.NewCheckpointStore(),
	})
	runID := rt.BeginRun()

	_, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echoTool()}}, plainModel("ok"), nil)
	if err == nil || !strings.Contains(err.Error(), "running status") {
		t.Fatalf("启动状态写入失败应报错, got %v", err)
	}
	rec, _ := rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusPending {
		t.Fatalf("登记应停在 pending, got %s", rec.Status)
	}
}

// 变体二：终态 Update 失败 → Run 返回错误（此前返回成功、状态停在 running）。
func TestTerminalStatusPersistFailurePropagates(t *testing.T) {
	runs := &failingUpdateRunStore{RunStore: *memory.NewRunStore(), failFrom: 2}
	// Update 序列：#1 = setStatus(running) 成功；#2 = 终态写入失败
	rt := runtime.New(runtime.Options{
		Runs: runs, Events: memory.NewEventStore(), Checkpoints: memory.NewCheckpointStore(),
	})
	runID := rt.BeginRun()

	_, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echoTool()}}, plainModel("ok"), nil)
	if err == nil || !strings.Contains(err.Error(), "terminal status") {
		t.Fatalf("终态写入失败应传到调用方, got %v", err)
	}
	rec, _ := rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusRunning {
		t.Fatalf("登记应停在 running（终态写入失败）, got %s", rec.Status)
	}
}

// ==== P1-2：终态事件（run_done）落库失败纳入返回值 ===

// 运行本身成功，但 run_done 写入失败 → 返回错误且登记降级为 failed，
// 不允许"缺终态事件"的同时返回成功（订阅者会因此一直等待）。
func TestTerminalEventPersistFailurePropagates(t *testing.T) {
	events := &terminalFailingEventStore{EventStore: *memory.NewEventStore()}
	rt := runtime.New(runtime.Options{
		Runs: memory.NewRunStore(), Events: events, Checkpoints: memory.NewCheckpointStore(),
	})
	runID := rt.BeginRun()

	_, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echoTool()}}, plainModel("ok"), nil)
	if err == nil || !strings.Contains(err.Error(), "persistence failed") {
		t.Fatalf("终态事件写入失败应报错, got %v", err)
	}
	rec, _ := rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusFailed {
		t.Fatalf("终态事件写入失败时登记应为 failed, got %s", rec.Status)
	}
}

// ==== P1-3：首个审批等待前必须有可恢复检查点 ===

// 首个工具等待审批时进程中断（取消模拟）→ 此时必须已存在检查点；
// Resume 后从中断点继续，工具只执行一次。
func TestResumeAfterApprovalWaitInterrupt(t *testing.T) {
	runs := memory.NewRunStore()
	events := memory.NewEventStore()
	cps := memory.NewCheckpointStore()
	rt := runtime.New(runtime.Options{Runs: runs, Events: events, Checkpoints: cps})
	runID := rt.BeginRun()

	executions := 0
	unsafe := tool.NewFunc("write", "写操作", nil, false,
		func(context.Context, map[string]any) (tool.Result, error) {
			executions++
			return tool.Text("written"), nil
		})

	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "write", Arguments: "{}"},
			}})
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		emit(message.Delta{Content: "不应到达"})
		return nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(),
			[]message.Message{{Role: message.RoleUser, Content: "执行写入"}},
			nil, agent.Config{RunID: runID, Tools: []tool.Tool{unsafe}}, model, nil)
		done <- err
	}()

	waitFor(t, func() bool {
		rec, err := rt.Get(context.Background(), runID)
		return err == nil && rec.Status == runtime.StatusWaitingApproval
	})
	// 回归断言：审批等待期间必须已有可恢复检查点。
	// 修复前首个审批等待发生在任何检查点之前，Resume 会因无检查点失败。
	if _, err := cps.Load(context.Background(), runID); err != nil {
		t.Fatalf("审批等待期间必须存在检查点: %v", err)
	}

	rt.Cancel(runID)
	if err := <-done; err == nil {
		t.Fatal("中断的运行应返回错误")
	}
	rec, _ := rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusCanceled {
		t.Fatalf("应为 canceled, got %s", rec.Status)
	}

	// Resume：自动批准，工具在本轮执行且仅执行一次
	resumedStep := 0
	resumed := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		resumedStep++
		switch resumedStep {
		case 1:
			// 从初始检查点恢复：轨迹只有原始输入
			if len(msgs) != 1 || msgs[0].Role != message.RoleUser {
				t.Fatalf("恢复轨迹应只有原始输入: %+v", msgs)
			}
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "n1", Name: "write", Arguments: "{}"},
			}})
		default:
			if got := msgs[len(msgs)-1].Content; got != "written" {
				t.Fatalf("工具结果不符: %q", got)
			}
			emit(message.Delta{Content: "恢复完成"})
		}
		return nil
	}
	result, err := rt.Resume(context.Background(), runID,
		agent.Config{Tools: []tool.Tool{unsafe}, Approve: alwaysApprove()}, resumed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "恢复完成" || executions != 1 {
		t.Fatalf("续跑不符: content=%q executions=%d", result.Content, executions)
	}
	rec, _ = rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusSucceeded {
		t.Fatalf("续跑后应为 succeeded, got %s", rec.Status)
	}
}

// ==== P1-3 续：调用级检查点停在步中途 → Resume 回退协议安全边界 ===

// 检查点停在 [user, assistant(c1,c2), tool(c1)]（c2 未回执，悬空）。
// Resume 后模型只收到 [user]（悬空段被回退，否则会被部分模型服务拒绝）；
// c1 的已记录结果被对账复用（不重执行），c2 正常执行。
func TestResumeTrimsDanglingCheckpoint(t *testing.T) {
	runID := "run-trim-dangling"
	runs := memory.NewRunStore()
	events := memory.NewEventStore()
	cps := memory.NewCheckpointStore()

	_ = runs.Create(context.Background(), runtime.RunRecord{ID: runID, Status: runtime.StatusFailed})
	_ = cps.Save(context.Background(), runtime.Checkpoint{
		RunID: runID, Step: 1,
		Messages: []message.Message{
			{Role: message.RoleUser, Content: "两个操作"},
			{Role: message.RoleAssistant, ToolCalls: []tool.Call{
				{ID: "c1", Function: tool.Function{Name: "boom", Arguments: []byte(`{"q":"a"}`)}},
				{ID: "c2", Function: tool.Function{Name: "boom", Arguments: []byte(`{"q":"b"}`)}},
			}},
			{Role: message.RoleTool, ToolCallID: "c1", ToolName: "boom", Content: "boom:a"},
		},
	})
	_ = events.Append(context.Background(), runID,
		agent.Event{Type: agent.EventToolProposed, RunID: runID, CallID: "c1", ToolName: "boom", Arguments: map[string]any{"q": "a"}},
		agent.Event{Type: agent.EventToolStarted, RunID: runID, CallID: "c1", ToolName: "boom"},
		agent.Event{Type: agent.EventToolResult, RunID: runID, CallID: "c1", ToolName: "boom", Result: "boom:a"},
		agent.Event{Type: agent.EventToolProposed, RunID: runID, CallID: "c2", ToolName: "boom", Arguments: map[string]any{"q": "b"}},
		agent.Event{Type: agent.EventRunError, RunID: runID, Error: "interrupted"},
	)

	executions := 0
	boom := tool.NewFunc("boom", "测试", nil, true,
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
			// 协议安全边界：悬空段（assistant(c1,c2)+tool(c1)）被回退
			if len(msgs) != 1 || msgs[0].Role != message.RoleUser {
				t.Fatalf("应回退到仅原始输入: %+v", msgs)
			}
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "n1", Name: "boom", Arguments: `{"q":"a"}`},
				{Index: 1, ID: "n2", Name: "boom", Arguments: `{"q":"b"}`},
			}})
		case 2:
			// c1 复用记录结果，c2 新执行
			if msgs[len(msgs)-2].Content != "boom:a" || msgs[len(msgs)-1].Content != "boom:b" {
				t.Fatalf("结果不符: %+v", msgs[len(msgs)-2:])
			}
			emit(message.Delta{Content: "完成"})
		}
		return nil
	}
	result, err := rt.Resume(context.Background(), runID,
		agent.Config{Tools: []tool.Tool{boom}}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "完成" || executions != 1 {
		t.Fatalf("续跑不符: content=%q executions=%d（c1 应复用记录结果）", result.Content, executions)
	}
}

// ==== P1-4：不确定调用持续阻断，不只挡一次 ===

// NoRetry 工具的调用在崩溃前已开始执行且无结果记录。模型第一次请求
// 收到处置指引；无视指引再次请求相同调用——必须仍然被拦截
// （修复前计数已耗尽，第二次会真实执行，产生重复副作用）。
func TestNoRetryBlocksRepeatedRequests(t *testing.T) {
	runID := "run-noretry-repeat"
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
		agent.Event{Type: agent.EventRunError, RunID: runID, Error: "interrupted"},
	)

	executions := 0
	charge := classifiedToolOf("charge", tool.RetryPolicyNoRetry, &executions)
	rt := runtime.New(runtime.Options{Runs: runs, Events: events, Checkpoints: cps})

	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		switch step {
		case 1, 2:
			// 模型无视指引，两次重复请求相同调用
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "n1", Name: "charge", Arguments: `{"amount":10}`},
			}})
		default:
			for _, m := range msgs[len(msgs)-2:] {
				if m.Role == message.RoleTool && !strings.Contains(m.Content, "Recovery notice") {
					t.Fatalf("重复请求应持续收到阻断指引: %q", m.Content)
				}
			}
			emit(message.Delta{Content: "已转人工"})
		}
		return nil
	}
	result, err := rt.Resume(context.Background(), runID,
		agent.Config{Tools: []tool.Tool{charge}, Approve: alwaysApprove()}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "已转人工" || executions != 0 {
		t.Fatalf("NoRetry 重复请求不应执行: executions=%d content=%q", executions, result.Content)
	}
}

// ==== 并行恢复：多工具并发命中索引（-race 验证无数据竞争） ===

// 同一键的两条已记录结果 + 一次响应重发两个相同调用（并行执行）：
// 修复前为无锁 map 并发读写；修复后每次调用各复用一条记录。
func TestParallelResumeHitsIndexConcurrently(t *testing.T) {
	runID := "run-parallel-resume"
	runs := memory.NewRunStore()
	events := memory.NewEventStore()
	cps := memory.NewCheckpointStore()

	_ = runs.Create(context.Background(), runtime.RunRecord{ID: runID, Status: runtime.StatusFailed})
	_ = cps.Save(context.Background(), runtime.Checkpoint{
		RunID: runID, Step: 1,
		Messages: []message.Message{
			{Role: message.RoleUser, Content: "并行操作"},
			{Role: message.RoleAssistant, ToolCalls: []tool.Call{
				{ID: "c1", Function: tool.Function{Name: "boom", Arguments: []byte(`{"q":"x"}`)}},
				{ID: "c2", Function: tool.Function{Name: "boom", Arguments: []byte(`{"q":"x"}`)}},
			}},
			{Role: message.RoleTool, ToolCallID: "c1", ToolName: "boom", Content: "boom:recorded"},
		},
	})
	_ = events.Append(context.Background(), runID,
		agent.Event{Type: agent.EventToolProposed, RunID: runID, CallID: "c1", ToolName: "boom", Arguments: map[string]any{"q": "x"}},
		agent.Event{Type: agent.EventToolStarted, RunID: runID, CallID: "c1", ToolName: "boom"},
		agent.Event{Type: agent.EventToolResult, RunID: runID, CallID: "c1", ToolName: "boom", Result: "boom:recorded"},
		agent.Event{Type: agent.EventToolProposed, RunID: runID, CallID: "c2", ToolName: "boom", Arguments: map[string]any{"q": "x"}},
		agent.Event{Type: agent.EventToolStarted, RunID: runID, CallID: "c2", ToolName: "boom"},
		agent.Event{Type: agent.EventToolResult, RunID: runID, CallID: "c2", ToolName: "boom", Result: "boom:recorded"},
		agent.Event{Type: agent.EventRunError, RunID: runID, Error: "interrupted"},
	)

	executions := 0
	boom := tool.NewFunc("boom", "测试", nil, true,
		func(context.Context, map[string]any) (tool.Result, error) {
			executions++
			return tool.Text("boom:fresh"), nil
		})
	rt := runtime.New(runtime.Options{Runs: runs, Events: events, Checkpoints: cps})

	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		switch step {
		case 1:
			if len(msgs) != 1 {
				t.Fatalf("应回退到原始输入: %+v", msgs)
			}
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "n1", Name: "boom", Arguments: `{"q":"x"}`},
				{Index: 1, ID: "n2", Name: "boom", Arguments: `{"q":"x"}`},
			}})
		default:
			// 两个并发调用应各复用一条记录结果
			for _, m := range msgs[len(msgs)-2:] {
				if m.Content != "boom:recorded" {
					t.Fatalf("并发命中应复用记录结果: %q", m.Content)
				}
			}
			emit(message.Delta{Content: "完成"})
		}
		return nil
	}
	result, err := rt.Resume(context.Background(), runID,
		agent.Config{Tools: []tool.Tool{boom}, ToolExecution: agent.ToolParallel}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "完成" || executions != 0 {
		t.Fatalf("并行恢复应全部复用: executions=%d", executions)
	}
}

// ==== Stream：慢消费者不丢事件（缺口补读 + 终态兜底） ===

// 事件总量（305）远超订阅缓冲（256），且消费端刻意放慢：
// Stream 必须交付与 Replay 完全一致的连续序列（含终态事件）。
func TestStreamDeliversCompleteSequence(t *testing.T) {
	rt := runtime.New(runtime.Options{
		Runs:        memory.NewRunStore(),
		Events:      memory.NewEventStore(),
		Checkpoints: memory.NewCheckpointStore(),
	})
	runID := rt.BeginRun()

	gate := make(chan struct{})
	var gateOnce sync.Once
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		emit(message.Delta{Content: "start"})
		<-gate // 等 Stream 交付首个事件后再放行洪水
		for i := 0; i < 300; i++ {
			emit(message.Delta{Content: "x"})
		}
		emit(message.Delta{Content: "done"})
		return nil
	}
	runDone := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(),
			[]message.Message{{Role: message.RoleUser, Content: "问"}},
			nil, agent.Config{RunID: runID}, model, nil)
		runDone <- err
	}()
	waitFor(t, func() bool {
		rec, err := rt.Get(context.Background(), runID)
		return err == nil && rec.Status == runtime.StatusRunning
	})

	var mu sync.Mutex
	var collected []agent.Event
	streamDone := make(chan error, 1)
	go func() {
		err := rt.Stream(context.Background(), runID, 0, func(e agent.Event) {
			mu.Lock()
			collected = append(collected, e)
			mu.Unlock()
			gateOnce.Do(func() { close(gate) })
			time.Sleep(100 * time.Microsecond) // 慢消费者
		})
		streamDone <- err
	}()

	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-streamDone; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	store, _ := rt.Replay(context.Background(), runID)
	if len(collected) != len(store) {
		t.Fatalf("Stream 交付不完整: got %d, want %d", len(collected), len(store))
	}
	for i := 1; i < len(collected); i++ {
		if collected[i].Seq != collected[i-1].Seq+1 {
			t.Fatalf("Seq 存在缺口: [%d]=%d → [%d]=%d",
				i-1, collected[i-1].Seq, i, collected[i].Seq)
		}
	}
	if tail := collected[len(collected)-1]; tail.Type != agent.EventRunDone {
		t.Fatalf("最后事件应为 run_done, got %s", tail.Type)
	}
}

// ==== 进程中断续跑（SQLite 持久化） ===

// 模拟"执行中崩溃"：工具已执行、事件已落库，检查点写入失败触发
// fail-closed 中止（等价于崩溃窗口 A）。随后用全新 Runtime 重新打开
// 同一数据库 Resume：模型重发同样调用时对账复用结果，不重复执行。
func TestProcessInterruptionResumeSqlite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.db")
	db, opts, err := sqlite.OpenRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	// 检查点存储：第 2 次保存（首个调用级检查点）起失败
	flaky := &flakyCheckpointStore{CheckpointStore: opts.Checkpoints, failFrom: 2}
	rt1 := runtime.New(runtime.Options{Runs: opts.Runs, Events: opts.Events, Checkpoints: flaky})
	runID := rt1.BeginRun()

	var executions atomic.Int64
	boom := tool.NewFunc("boom", "副作用工具", nil, true,
		func(context.Context, map[string]any) (tool.Result, error) {
			executions.Add(1)
			return tool.Text("boom:executed"), nil
		})

	step := 0
	crashed := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "boom", Arguments: `{"q":"x"}`},
			}})
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		emit(message.Delta{Content: "不应到达"})
		return nil
	}
	_, err = rt1.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "执行操作"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{boom}}, crashed, nil)
	if err == nil || !strings.Contains(err.Error(), "persistence failed") {
		t.Fatalf("中断运行应报持久化错误, got %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// "重启"：全新 Runtime 打开同一数据库（健康的检查点存储）
	db2, opts2, err := sqlite.OpenRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	rt2 := runtime.New(opts2)
	rec, err := rt2.Get(context.Background(), runID)
	if err != nil || rec.Status != runtime.StatusFailed {
		t.Fatalf("重启后应为 failed: %+v err=%v", rec, err)
	}

	resumedStep := 0
	resumed := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		resumedStep++
		switch resumedStep {
		case 1:
			// 从初始检查点恢复：轨迹只有原始输入（崩溃前只保存了初始检查点）
			if len(msgs) != 1 || msgs[0].Role != message.RoleUser {
				t.Fatalf("恢复轨迹应只有原始输入: %+v", msgs)
			}
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "n1", Name: "boom", Arguments: `{"q":"x"}`},
			}})
		default:
			if got := msgs[len(msgs)-1].Content; got != "boom:executed" {
				t.Fatalf("应复用崩溃前已落库的结果: %q", got)
			}
			emit(message.Delta{Content: "重启后完成"})
		}
		return nil
	}
	result, err := rt2.Resume(context.Background(), runID,
		agent.Config{Tools: []tool.Tool{boom}}, resumed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "重启后完成" {
		t.Fatalf("续跑结果不符: %+v", result)
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("崩溃前已执行的工具不应重执行: executions=%d", got)
	}
	rec, _ = rt2.Get(context.Background(), runID)
	if rec.Status != runtime.StatusSucceeded {
		t.Fatalf("续跑后应为 succeeded, got %s", rec.Status)
	}
}
