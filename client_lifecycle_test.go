package dlzgoai_test

import (
	"context"
	"errors"
	dlzgoai "github.com/dingkui/dlz-goai"
	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/tool"
	"sync/atomic"
	"testing"
	"time"
)

func TestActiveResumeKeepsOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _ := dlzgoai.NewClient(dlzgoai.Options{})
	defer c.Close()
	run, err := c.Start(ctx, dlzgoai.Request{Input: "hello", Model: func(ctx context.Context, _ []message.Message, _ *message.Options, _ func(message.Delta)) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Get(ctx); err != nil {
		t.Fatalf("Start returned before registration: %v", err)
	}
	for i := 0; i < 20; i++ {
		if _, err := c.Resume(ctx, run.ID()); !errors.Is(err, runtime.ErrRunActive) {
			t.Fatalf("duplicate resume: %v", err)
		}
	}
	if !c.Cancel(run.ID()) {
		t.Fatal("lost owner")
	}
	if _, err := run.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("owner result: %v", err)
	}
}

func TestResumeRetainsRequestAndAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var attempts atomic.Int32
	var executed atomic.Int32
	write := tool.NewFunc("write", "write", nil, true, func(context.Context, map[string]any) (tool.Result, error) {
		executed.Add(1)
		return tool.Text("ok"), nil
	})
	model := func(ctx context.Context, msgs []message.Message, _ *message.Options, cb func(message.Delta)) error {
		if attempts.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		if msgs[len(msgs)-1].Role == message.RoleTool {
			cb(message.Delta{Content: "restored"})
			return nil
		}
		cb(message.Delta{ToolCalls: []tool.CallDelta{{Index: 0, ID: "w", Name: "write", Arguments: "{}"}}})
		return nil
	}
	c, _ := dlzgoai.NewClient(dlzgoai.Options{Model: plainModel("wrong model")})
	defer c.Close()
	policies := map[string]tool.Policy{"write": tool.PolicyConfirm}
	run, err := c.Start(ctx, dlzgoai.Request{Input: "hello", Model: model, Tools: []tool.Tool{write}, Policies: policies})
	if err != nil {
		t.Fatal(err)
	}
	for attempts.Load() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	policies["write"] = tool.PolicyAuto
	run.Cancel()
	if _, err = run.Wait(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
	resumed, err := c.Resume(ctx, run.ID())
	if err != nil {
		t.Fatal(err)
	}
	err = resumed.Stream(ctx, 0, func(e agent.Event) {
		if e.Type == agent.EventApprovalRequired && e.Approved == nil {
			if executed.Load() != 0 {
				t.Error("policy lost")
			}
			if err := resumed.Approve(e.CallID, true); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := resumed.Wait(ctx)
	if err != nil || result.Content != "restored" || executed.Load() != 1 {
		t.Fatalf("resume: %+v %v count=%d", result, err, executed.Load())
	}
	if _, err := run.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("old handle changed: %v", err)
	}
	if run.Cancel() {
		t.Fatal("old handle can cancel new attempt")
	}
}

func TestCompletedCacheAndExplicitRecovery(t *testing.T) {
	c, _ := dlzgoai.NewClient(dlzgoai.Options{Model: plainModel("ok"), MaxCompletedRuns: 1})
	defer c.Close()
	a, _ := c.Start(context.Background(), dlzgoai.Request{Input: "a"})
	a.Wait(context.Background())
	b, _ := c.Start(context.Background(), dlzgoai.Request{Input: "b"})
	b.Wait(context.Background())
	if _, err := c.Wait(context.Background(), a.ID()); !errors.Is(err, dlzgoai.ErrRunNotManaged) {
		t.Fatal(err)
	}
	if result, err := a.Wait(context.Background()); err != nil || result.Content != "ok" {
		t.Fatal(result, err)
	}
	if _, err := c.Resume(context.Background(), a.ID()); !errors.Is(err, dlzgoai.ErrResumeConfigRequired) {
		t.Fatal(err)
	}
	if !c.Forget(b.ID()) {
		t.Fatal("Forget failed")
	}
}

func TestCanceledSubmissionDoesNotStart(t *testing.T) {
	c, _ := dlzgoai.NewClient(dlzgoai.Options{Model: plainModel("ok")})
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Start(ctx, dlzgoai.Request{Input: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(c.ActiveRuns()) != 0 {
		t.Fatal("started a canceled submission")
	}
}
