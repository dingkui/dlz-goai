package dlzgoai_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dlzgoai "github.com/dingkui/dlz-goai"
	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/tool"
)

// ---- 测试辅助 ----

func plainModel(content string) agent.ModelFunc {
	return func(_ context.Context, _ []message.Message, _ *message.Options, cb func(message.Delta)) error {
		cb(message.Delta{Content: content})
		return nil
	}
}

// scriptedModel 两步脚本：第一步发起一次工具调用，第二步给最终回答。
type scriptedModel struct {
	calls atomic.Int32
	name  string
	args  string
}

func newScriptedModel(name, args string) *scriptedModel {
	return &scriptedModel{name: name, args: args}
}

func (m *scriptedModel) call(ctx context.Context, msgs []message.Message,
	opts *message.Options, cb func(message.Delta)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if m.calls.Add(1) == 1 {
		cb(message.Delta{ToolCalls: []tool.CallDelta{
			{Index: 0, ID: "c1", Name: m.name, Arguments: m.args},
		}})
		return nil
	}
	cb(message.Delta{Content: "脚本回答完成"})
	return nil
}

func waitStatus(t *testing.T, c *dlzgoai.Client, runID string, want runtime.Status) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := c.GetRun(context.Background(), runID)
		if err == nil && rec.Status == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待状态 %s 超时", want)
}

func alwaysApprove() agent.ApprovalHandler {
	return agent.ApprovalFunc(func(context.Context, agent.ApprovalRequest) (bool, error) {
		return true, nil
	})
}

// ---- 装配与校验 ----

func TestNewClientRequiresModel(t *testing.T) {
	// Model 可选：省略时 Client 可创建，但 Start 必须逐请求提供 Request.Model
	c, err := dlzgoai.NewClient(dlzgoai.Options{})
	if err != nil {
		t.Fatalf("Model 可省略: %v", err)
	}
	defer c.Close()
	if _, err := c.Start(context.Background(), dlzgoai.Request{Input: "问"}); !errors.Is(err, dlzgoai.ErrNoModel) {
		t.Fatalf("缺 Model 的 Start 应返回 ErrNoModel, got %v", err)
	}
	// Request.Model 提供即可运行
	run, err := c.Start(context.Background(), dlzgoai.Request{Input: "问", Model: plainModel("ok")})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := run.Wait(context.Background()); err != nil || result.Content != "ok" {
		t.Fatalf("Request.Model 应生效: %+v err=%v", result, err)
	}
}

func TestRequestRequiresInput(t *testing.T) {
	c, err := dlzgoai.NewClient(dlzgoai.Options{Model: plainModel("ok")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Start(context.Background(), dlzgoai.Request{}); !errors.Is(err, dlzgoai.ErrNoInput) {
		t.Fatalf("空请求应返回 ErrNoInput, got %v", err)
	}
}

// ---- Start → Wait 主干 ----

func TestStartAndWaitHappyPath(t *testing.T) {
	c, err := dlzgoai.NewClient(dlzgoai.Options{Model: plainModel("你好，我是助手")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	run, err := c.Start(context.Background(), dlzgoai.Request{Input: "你好"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "你好，我是助手" || result.Steps != 1 {
		t.Fatalf("结果不符: %+v", result)
	}

	rec, err := c.GetRun(context.Background(), run.ID())
	if err != nil || rec.Status != runtime.StatusSucceeded {
		t.Fatalf("应为 succeeded: %+v err=%v", rec, err)
	}
	// Wait 迟到调用：结果已缓存，可重复获取
	again, err := c.Wait(context.Background(), run.ID())
	if err != nil || again.Content != result.Content {
		t.Fatalf("Wait 应可重复获取缓存结果: %+v err=%v", again, err)
	}
}

// 工具循环 + 事件回放：模型调工具 → 结果回传 → 最终回答。
func TestStartWithToolLoop(t *testing.T) {
	echo := tool.NewFunc("echo", "回声", nil, true,
		func(_ context.Context, args map[string]any) (tool.Result, error) {
			return tool.Text("echo:" + args["q"].(string)), nil
		})
	m := newScriptedModel("echo", `{"q":"hi"}`)
	c, err := dlzgoai.NewClient(dlzgoai.Options{Model: m.call, Tools: []tool.Tool{echo}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	run, err := c.Start(context.Background(), dlzgoai.Request{Input: "调用回声"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "脚本回答完成" {
		t.Fatalf("最终回答不符: %+v", result)
	}

	events, err := c.Replay(context.Background(), run.ID())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Type] = true
		if e.Seq <= 0 {
			t.Fatalf("事件应有 Seq: %+v", e)
		}
	}
	for _, want := range []string{agent.EventRunStart, agent.EventToolResult, agent.EventFinal, agent.EventRunDone} {
		if !seen[want] {
			t.Fatalf("回放缺少 %s", want)
		}
	}
}

// ---- 审批流：等待 → 经门面批准 → 完成 ----

func TestApprovalViaClient(t *testing.T) {
	executions := 0
	write := tool.NewFunc("write_thing", "写操作", nil, false,
		func(context.Context, map[string]any) (tool.Result, error) {
			executions++
			return tool.Text("written"), nil
		})
	m := newScriptedModel("write_thing", "{}")
	c, err := dlzgoai.NewClient(dlzgoai.Options{Model: m.call, Tools: []tool.Tool{write}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	run, err := c.Start(context.Background(), dlzgoai.Request{Input: "执行写入"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, run.ID(), runtime.StatusWaitingApproval)

	rec, _ := c.GetRun(context.Background(), run.ID())
	if rec.PendingApproval == nil || rec.PendingApproval.ToolName != "write_thing" {
		t.Fatalf("待审批详情不符: %+v", rec.PendingApproval)
	}

	// 经门面提交决策（真实应用 = 独立 HTTP 端点调用这里）
	if err := c.Approve(run.ID(), rec.PendingApproval.CallID, true); err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "脚本回答完成" || executions != 1 {
		t.Fatalf("审批后应完成: content=%q executions=%d", result.Content, executions)
	}
}

// ---- Cancel → Resume：审批等待中断后续跑 ----

func TestCancelThenResume(t *testing.T) {
	executions := 0
	write := tool.NewFunc("write_thing", "写操作", nil, false,
		func(context.Context, map[string]any) (tool.Result, error) {
			executions++
			return tool.Text("written"), nil
		})
	m := newScriptedModel("write_thing", "{}")
	c, err := dlzgoai.NewClient(dlzgoai.Options{Model: m.call, Tools: []tool.Tool{write}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	run, err := c.Start(context.Background(), dlzgoai.Request{Input: "执行写入"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, run.ID(), runtime.StatusWaitingApproval)

	if !run.Cancel() {
		t.Fatal("取消活跃运行应返回 true")
	}
	if _, err := run.Wait(context.Background()); err == nil {
		t.Fatal("被取消的运行 Wait 应返回错误")
	}
	rec, _ := c.GetRun(context.Background(), run.ID())
	if rec.Status != runtime.StatusCanceled {
		t.Fatalf("应为 canceled, got %s", rec.Status)
	}
	// 恢复对账：未执行过的调用（仅审批等待）不进入阻断/复用索引，
	// Resume 后重新走审批并执行恰好一次。

	// 不受管检查：cancel 后运行结束，runState 仍在缓存——Wait 仍可用；
	// 但 Close 释放后即为 ErrRunNotManaged（在 Close 用例中验证）。

	// 模型脚本重置：恢复后轨迹回到原始输入，应重新发起同样的调用
	m.calls.Store(0)
	resumed, err := c.Resume(context.Background(), run.ID())
	if err != nil {
		t.Fatal(err)
	}
	// Resume 后同样进入审批等待，批准后完成
	waitStatus(t, c, resumed.ID(), runtime.StatusWaitingApproval)
	rec2, _ := c.GetRun(context.Background(), resumed.ID())
	if err := c.Approve(resumed.ID(), rec2.PendingApproval.CallID, true); err != nil {
		t.Fatal(err)
	}
	result, err := resumed.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "脚本回答完成" || executions != 1 {
		t.Fatalf("续跑应完成且工具恰好执行一次: content=%q executions=%d", result.Content, executions)
	}
}

// ---- Request.Tools 覆盖默认工具集 ----

func TestRequestToolsOverride(t *testing.T) {
	defaultTool := tool.NewFunc("default_tool", "默认工具", nil, true,
		func(context.Context, map[string]any) (tool.Result, error) {
			return tool.Text("default"), nil
		})
	overr := tool.NewFunc("override_tool", "覆盖工具", nil, true,
		func(_ context.Context, args map[string]any) (tool.Result, error) {
			return tool.Text("override:" + args["q"].(string)), nil
		})
	m := newScriptedModel("override_tool", `{"q":"x"}`)
	c, err := dlzgoai.NewClient(dlzgoai.Options{Model: m.call, Tools: []tool.Tool{defaultTool}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	run, err := c.Start(context.Background(), dlzgoai.Request{
		Input: "调用覆盖工具",
		Tools: []tool.Tool{overr},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Messages[2].Content, "override:x") {
		t.Fatalf("应使用覆盖工具: %+v", result.Messages)
	}
}

// ---- Wait 语义：ctx 取消只停止等待，不影响运行 ----

func TestWaitContextCancelDoesNotStopRun(t *testing.T) {
	// 慢模型：给"Wait 超时但运行继续"留出可观测窗口
	slow := func(ctx context.Context, _ []message.Message, _ *message.Options, cb func(message.Delta)) error {
		select {
		case <-time.After(200 * time.Millisecond):
			cb(message.Delta{Content: "完成"})
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c, err := dlzgoai.NewClient(dlzgoai.Options{Model: slow})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	run, err := c.Start(context.Background(), dlzgoai.Request{Input: "问"})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := run.Wait(waitCtx); err == nil {
		t.Fatal("短超时 Wait 应超时返回")
	}
	// 运行不受影响：随后 Wait 正常拿到结果
	result, err := run.Wait(context.Background())
	if err != nil || result.Content != "完成" {
		t.Fatalf("运行应继续并完成: %+v err=%v", result, err)
	}
}

// ---- 并发 Start ----

func TestConcurrentStarts(t *testing.T) {
	c, err := dlzgoai.NewClient(dlzgoai.Options{Model: plainModel("ok")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var wg sync.WaitGroup
	var fails atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := c.Start(context.Background(), dlzgoai.Request{Input: "问"})
			if err != nil {
				fails.Add(1)
				return
			}
			if _, err := run.Wait(context.Background()); err != nil {
				fails.Add(1)
			}
		}()
	}
	wg.Wait()
	if fails.Load() != 0 {
		t.Fatalf("并发 Start/Wait 有失败: %d", fails.Load())
	}
	if got := len(c.ActiveRuns()); got != 0 {
		t.Fatalf("全部完成后活跃运行应为 0, got %d", got)
	}
}

// ---- Close 语义 ----

func TestCloseCancelsActiveRunsAndIsTerminal(t *testing.T) {
	// 工具里阻塞等待，保证 Close 时运行仍活跃
	release := make(chan struct{})
	blocker := tool.NewFunc("block", "阻塞工具", nil, true,
		func(ctx context.Context, _ map[string]any) (tool.Result, error) {
			select {
			case <-release:
				return tool.Text("released"), nil
			case <-ctx.Done():
				return tool.Result{}, ctx.Err()
			}
		})
	m := newScriptedModel("block", "{}")
	c, err := dlzgoai.NewClient(dlzgoai.Options{Model: m.call, Tools: []tool.Tool{blocker}})
	if err != nil {
		t.Fatal(err)
	}

	run, err := c.Start(context.Background(), dlzgoai.Request{Input: "问"})
	if err != nil {
		t.Fatal(err)
	}
	waitForRunning(t, c, run.ID())

	if err := c.Close(); err != nil {
		t.Fatalf("Close 应返回 nil: %v", err)
	}
	close(release) // 防御：即使取消语义失效也让工具退出，避免测试悬挂

	if _, err := run.Wait(context.Background()); err == nil {
		t.Fatal("Close 取消后的 Wait 应返回错误")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close 应幂等: %v", err)
	}
	if _, err := c.Start(context.Background(), dlzgoai.Request{Input: "问"}); !errors.Is(err, dlzgoai.ErrClientClosed) {
		t.Fatalf("Close 后 Start 应返回 ErrClientClosed, got %v", err)
	}
	if _, err := c.Wait(context.Background(), run.ID()); !errors.Is(err, dlzgoai.ErrRunNotManaged) {
		t.Fatalf("Close 后 Wait 应返回 ErrRunNotManaged, got %v", err)
	}
	if run.Cancel() {
		t.Fatal("Close 后 Cancel 应返回 false")
	}
}

// Close 不等待完成时机的补充：Wait 缓存在正常路径下可用
// （不 Close 的情况下，完成后 Wait 仍拿到结果——TestStartAndWaitHappyPath 已覆盖）。

func waitForRunning(t *testing.T, c *dlzgoai.Client, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := c.GetRun(context.Background(), runID)
		if err == nil && rec.Status == runtime.StatusRunning {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("等待 running 超时")
}
