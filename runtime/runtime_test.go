package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/runtime/memory"
	"github.com/dingkui/dlz-goai/tool"
)

func echoTool() tool.Tool {
	return tool.NewFunc("echo", "回声", nil, true,
		func(_ context.Context, args map[string]any) (tool.Result, error) {
			return tool.Text("echo:" + args["q"].(string)), nil
		})
}

func newTestRuntime() *runtime.Runtime {
	return runtime.New(runtime.Options{
		Runs:        memory.NewRunStore(),
		Events:      memory.NewEventStore(),
		Checkpoints: memory.NewCheckpointStore(),
	})
}

// 状态机主干：pending → running → succeeded；事件与检查点随行。
func TestRuntimeHappyPath(t *testing.T) {
	rt := newTestRuntime()
	runID := rt.BeginRun()

	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "echo", Arguments: `{"q":"hi"}`},
			}})
			return nil
		}
		emit(message.Delta{Content: "完成"})
		return nil
	}

	result, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echoTool()}}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "完成" || result.Steps != 2 {
		t.Fatalf("结果不符: %+v", result)
	}

	rec, err := rt.Get(context.Background(), runID)
	if err != nil || rec.Status != runtime.StatusSucceeded {
		t.Fatalf("终态应为 succeeded: %+v err=%v", rec, err)
	}
	if rec.LastStep != 1 {
		t.Fatalf("LastStep 应为 1, got %d", rec.LastStep)
	}

	events, _ := rt.Replay(context.Background(), runID)
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Type] = true
		if e.RunID != runID {
			t.Fatalf("事件应带 RunID: %+v", e)
		}
	}
	for _, want := range []string{agent.EventToolResult, agent.EventStepDone, agent.EventFinal} {
		if !seen[want] {
			t.Fatalf("回放缺少 %s", want)
		}
	}
}

// 断点续跑：运行中断后从检查点恢复，轨迹连续。
func TestRuntimeResumeAfterFailure(t *testing.T) {
	rt := newTestRuntime()
	runID := rt.BeginRun()

	// 第一次：第一步执行工具，第二步报错中断
	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "echo", Arguments: `{"q":"go"}`},
			}})
			return nil
		}
		return errors.New("连接中断")
	}
	_, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echoTool()}}, model, nil)
	if err == nil {
		t.Fatal("第一次运行应失败")
	}
	rec, _ := rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusFailed {
		t.Fatalf("应为 failed, got %s", rec.Status)
	}

	// 第二次：Resume，模型基于历史轨迹给最终答案
	resumed := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		// 轨迹应包含：user + assistant(tool_calls) + tool —— 共 3 条历史
		if len(msgs) != 3 || msgs[2].Role != message.RoleTool || msgs[2].Content != "echo:go" {
			t.Fatalf("恢复的轨迹不符: %+v", msgs)
		}
		emit(message.Delta{Content: "恢复后完成"})
		return nil
	}
	result, err := rt.Resume(context.Background(), runID,
		agent.Config{Tools: []tool.Tool{echoTool()}}, resumed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "恢复后完成" {
		t.Fatalf("续跑结果不符: %+v", result)
	}
	rec, _ = rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusSucceeded {
		t.Fatalf("续跑后应为 succeeded, got %s", rec.Status)
	}
}

// 审批持久化：等待期间状态与待审批调用落库，决策后解除并正常结束。
func TestRuntimeApprovalLifecycle(t *testing.T) {
	rt := newTestRuntime()
	runID := rt.BeginRun()

	unsafe := tool.NewFunc("write", "写操作", nil, false,
		func(context.Context, map[string]any) (tool.Result, error) {
			return tool.Text("written"), nil
		})

	var observed atomicRecord
	approve := agent.ApprovalFunc(func(ctx context.Context, req agent.ApprovalRequest) (bool, error) {
		rec, _ := rt.Get(ctx, req.CallID[:0]) // 占位，避免 unused
		_ = rec
		rec2, _ := rt.Get(context.Background(), runID)
		observed.set(rec2)
		return true, nil
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
		emit(message.Delta{Content: "写完了"})
		return nil
	}

	_, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "写"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{unsafe}, Approve: approve}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	waiting := observed.get()
	if waiting.Status != runtime.StatusWaitingApproval || waiting.PendingApproval == nil ||
		waiting.PendingApproval.ToolName != "write" {
		t.Fatalf("等待审批时状态应落库: %+v", waiting)
	}
	rec, _ := rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusSucceeded || rec.PendingApproval != nil {
		t.Fatalf("结束后应清空待审批: %+v", rec)
	}
}

// 订阅：进行中运行的事件实时到达订阅端。
func TestRuntimeSubscribe(t *testing.T) {
	rt := newTestRuntime()
	runID := rt.BeginRun()
	ch, unsubscribe := rt.Subscribe(runID)
	defer unsubscribe()

	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		emit(message.Delta{Content: "答"})
		return nil
	}
	if _, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echoTool()}}, model, nil); err != nil {
		t.Fatal(err)
	}
	var got []agent.Event
	deadline := time.After(time.Second)
	for {
		select {
		case e := <-ch:
			got = append(got, e)
			if e.Type == agent.EventFinal {
				if got[0].Type != agent.EventRunStart {
					t.Fatalf("首事件应为 run_start: %+v", got[0])
				}
				return
			}
		case <-deadline:
			t.Fatalf("未等到 final 事件: %+v", got)
		}
	}
}

// 进程恢复：非终态登记统一标记失败。
func TestRuntimeRecover(t *testing.T) {
	rt := newTestRuntime()
	runs := memory.NewRunStore()
	rt2 := runtime.New(runtime.Options{Runs: runs})

	ctx := context.Background()
	_ = runs.Create(ctx, runtime.RunRecord{ID: "r1", Status: runtime.StatusRunning})
	_ = runs.Create(ctx, runtime.RunRecord{ID: "r2", Status: runtime.StatusWaitingApproval, PendingApproval: &runtime.PendingApproval{CallID: "c1"}})
	_ = runs.Create(ctx, runtime.RunRecord{ID: "r3", Status: runtime.StatusSucceeded})

	n, err := rt2.Recover(ctx)
	if err != nil || n != 2 {
		t.Fatalf("应恢复 2 条, got %d err=%v", n, err)
	}
	rec, _ := runs.Get(ctx, "r2")
	if rec.Status != runtime.StatusFailed || rec.PendingApproval != nil || !strings.Contains(rec.Error, "恢复") {
		t.Fatalf("恢复状态不符: %+v", rec)
	}
	ok, _ := runs.Get(ctx, "r3")
	if ok.Status != runtime.StatusSucceeded {
		t.Fatal("终态不应被恢复改写")
	}
	_ = rt
}

// 取消：进行中的运行被取消后状态为 canceled。
func TestRuntimeCancel(t *testing.T) {
	rt := newTestRuntime()
	runID := rt.BeginRun()

	release := make(chan struct{})
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		<-release // 挂起等待被取消
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		_, _ = rt.Run(context.Background(),
			[]message.Message{{Role: message.RoleUser, Content: "问"}},
			nil, agent.Config{RunID: runID, Tools: []tool.Tool{echoTool()}}, model, nil)
		close(done)
	}()
	// 等运行真正开始（登记为 running）
	deadline := time.After(2 * time.Second)
	for {
		rec, err := rt.Get(context.Background(), runID)
		if err == nil && rec.Status == runtime.StatusRunning {
			break
		}
		select {
		case <-deadline:
			t.Fatal("运行未进入 running 状态")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !rt.Cancel(runID) {
		t.Fatal("Cancel 应返回 true")
	}
	close(release)
	<-done
	rec, _ := rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusCanceled {
		t.Fatalf("应为 canceled, got %s", rec.Status)
	}
}

type atomicRecord struct {
	mu sync.Mutex
	r  runtime.RunRecord
}

func (a *atomicRecord) set(r runtime.RunRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.r = r
}

func (a *atomicRecord) get() runtime.RunRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.r
}
