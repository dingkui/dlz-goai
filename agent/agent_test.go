package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/tool"
)

func stubTool() tool.Tool {
	return tool.NewFunc("stub_tool", "测试工具", nil, true,
		func(context.Context, map[string]any) (tool.Result, error) {
			return tool.Text("ok"), nil
		})
}

func stubConfig() Config {
	return Config{Tools: []tool.Tool{stubTool()}}
}

// 流式中断（用户停止/连接断开）时，本步已流出的部分内容应保留在轨迹里。
func TestRunKeepsPartialContentOnStreamError(t *testing.T) {
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		emit(message.Delta{Content: "我先查"})
		emit(message.Delta{Content: "一下相关资料"})
		return context.Canceled // 模拟用户停止
	}
	result, err := New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, stubConfig(), model, nil,
	)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if len(result.Messages) != 2 {
		t.Fatalf("中断的部分内容应保留为 assistant 消息: got %d 条", len(result.Messages))
	}
	last := result.Messages[len(result.Messages)-1]
	if last.Role != message.RoleAssistant || last.Content != "我先查一下相关资料" {
		t.Fatalf("末尾应为含部分内容的 assistant 消息, got %+v", last)
	}
}

// 流内 error 事件（err==nil 但 delta.Error 非空）同样保留部分内容。
func TestRunKeepsPartialContentOnDeltaError(t *testing.T) {
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		emit(message.Delta{Content: "半句话"})
		emit(message.Delta{Error: "boom"})
		return nil
	}
	result, err := New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, stubConfig(), model, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("应透传流内错误: %v", err)
	}
	if len(result.Messages) != 2 || result.Messages[1].Content != "半句话" {
		t.Fatalf("部分内容应保留: %+v", result.Messages)
	}
}

// 中断且无任何内容时不追加空消息。
func TestRunNoEmptyPartialOnImmediateCancel(t *testing.T) {
	cancelErr := errors.New("canceled")
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		return cancelErr
	}
	result, err := New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, stubConfig(), model, nil,
	)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if len(result.Messages) != 1 {
		t.Fatalf("无内容时不应追加消息: got %d 条", len(result.Messages))
	}
}

// 正常完成路径回归：最终回答与用量正常返回。
func TestRunNormal(t *testing.T) {
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		emit(message.Delta{Content: "答"})
		emit(message.Delta{Done: true, EvalTokens: 42, EvalMs: 1200})
		return nil
	}
	result, err := New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, stubConfig(), model, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "答" || result.EvalTokens != 42 || result.EvalMs != 1200 {
		t.Fatalf("正常结果不符: %+v", result)
	}
}

// 工具调用闭环：模型请求工具 → 执行 → 结果回传 → 模型给最终答案。
func TestRunToolLoop(t *testing.T) {
	step := 0
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "stub_tool", Arguments: "{}"},
			}})
			return nil
		}
		emit(message.Delta{Content: "工具结果已收到"})
		return nil
	}
	var events []Event
	result, err := New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, stubConfig(), model, func(e Event) { events = append(events, e) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "工具结果已收到" || result.Steps != 2 {
		t.Fatalf("循环结果不符: %+v", result)
	}
	// 轨迹应为: user, assistant(tool_calls), tool, assistant
	if len(result.Messages) != 4 {
		t.Fatalf("消息轨迹应为 4 条, got %d", len(result.Messages))
	}
	if result.Messages[2].Role != message.RoleTool || result.Messages[2].Content != "ok" {
		t.Fatalf("工具回执不符: %+v", result.Messages[2])
	}
	// 事件序列应包含完整的工具生命周期
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Type] = true
	}
	for _, want := range []string{EventToolProposed, EventToolStarted, EventToolResult, EventStepDone, EventFinal} {
		if !seen[want] {
			t.Fatalf("缺少事件 %s, got %v", want, events)
		}
	}
}

// 策略为 deny 时，工具不执行，模型收到禁止回执后仍可继续作答。
func TestRunPolicyDeny(t *testing.T) {
	step := 0
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "stub_tool", Arguments: "{}"},
			}})
			return nil
		}
		emit(message.Delta{Content: "好的"})
		return nil
	}
	cfg := Config{
		Tools:    []tool.Tool{stubTool()},
		Policies: map[string]tool.Policy{"stub_tool": tool.PolicyDeny},
	}
	var events []Event
	result, err := New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, cfg, model, func(event Event) { events = append(events, event) },
	)
	if err != nil {
		t.Fatal(err)
	}
	toolMsg := result.Messages[2]
	if toolMsg.Role != message.RoleTool || !strings.Contains(toolMsg.Content, "禁止") {
		t.Fatalf("应收到策略禁止回执: %+v", toolMsg)
	}
	var found bool
	for _, event := range events {
		if event.Type == EventToolError && event.CallID == "c1" &&
			event.ToolName == "stub_tool" && strings.Contains(event.Error, "禁止") {
			found = true
		}
	}
	if !found {
		t.Fatalf("策略拒绝必须发出 tool_error: %+v", events)
	}
}

// 有副作用工具（未声明只读）默认需要审批；拒绝后模型收到未批准回执。
func TestRunDefaultRequiresApproval(t *testing.T) {
	unsafe := tool.NewFunc("write_thing", "写操作", nil, false,
		func(context.Context, map[string]any) (tool.Result, error) {
			t.Error("未批准的工具不应被执行")
			return tool.Text("不应到达"), nil
		})
	step := 0
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "write_thing", Arguments: "{}"},
			}})
			return nil
		}
		emit(message.Delta{Content: "了解"})
		return nil
	}
	cfg := Config{Tools: []tool.Tool{unsafe}}
	result, err := New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, cfg, model, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Messages[2].Content, "未批准") {
		t.Fatalf("应收到未批准回执: %+v", result.Messages[2])
	}
}

// Broker：决策到达前阻塞，决策后放行；
// 等待中途运行结束按中断处理，结束后再请求则报运行不存在。
func TestBrokerApprovalFlow(t *testing.T) {
	b := NewBroker()
	runID := b.Begin()
	handler := b.For(runID)

	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = b.Resolve(runID, "c1", true)
	}()
	ok, err := handler.Request(context.Background(), ApprovalRequest{CallID: "c1"})
	if err != nil || !ok {
		t.Fatalf("应批准: ok=%v err=%v", ok, err)
	}

	// 等待中途 End：悬挂的等待应按中断处理，而不是永远阻塞
	go func() {
		time.Sleep(20 * time.Millisecond)
		b.End(runID)
	}()
	_, err = handler.Request(context.Background(), ApprovalRequest{CallID: "c2"})
	if !errors.Is(err, ErrApprovalAborted) {
		t.Fatalf("等待中途结束应返回 ErrApprovalAborted, got %v", err)
	}

	// 运行结束后再请求：运行已不存在
	_, err = handler.Request(context.Background(), ApprovalRequest{CallID: "c3"})
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("运行结束后应返回 ErrRunNotFound, got %v", err)
	}
}
