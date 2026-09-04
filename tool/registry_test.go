package tool

import (
	"context"
	"testing"
)

func noop(context.Context, map[string]any) (Result, error) { return Text("ok"), nil }

// 空白名单语义：Select 返回空而非全量——空过滤器和"不过滤"是两回事。
func TestRegistrySelectEmptyAllowList(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(NewFunc("a", "", nil, true, noop))
	r.MustRegister(NewFunc("b", "", nil, true, noop))
	if got := r.Select(nil); got != nil {
		t.Fatalf("空白名单应返回空: %+v", got)
	}
	if got := r.Select([]string{"a"}); len(got) != 1 || got[0].Definition().Name != "a" {
		t.Fatalf("白名单过滤不符: %+v", got)
	}
}

// 同名工具拒绝重复注册，避免静默覆盖配置错误。
func TestRegistryDuplicate(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(NewFunc("a", "", nil, true, noop))
	if err := r.Register(NewFunc("a", "", nil, true, noop)); err == nil {
		t.Fatal("重复注册应报错")
	}
}

// 策略判定：显式配置优先，其次看工具是否声明只读，未声明的默认要求确认。
func TestRegistryPolicyFor(t *testing.T) {
	ro := NewFunc("ro", "", nil, true, noop)
	rw := NewFunc("rw", "", nil, false, noop)
	r := NewRegistry()
	r.MustRegister(ro)
	r.MustRegister(rw)

	if got := r.PolicyFor("ro"); got != PolicyAuto {
		t.Fatalf("只读工具默认应放行, got %s", got)
	}
	if got := r.PolicyFor("rw"); got != PolicyConfirm {
		t.Fatalf("未声明只读默认应确认, got %s", got)
	}
	r.SetPolicy("rw", PolicyDeny)
	if got := r.PolicyFor("rw"); got != PolicyDeny {
		t.Fatalf("显式策略应优先, got %s", got)
	}
}

// 工具执行闭包：参数与结果按契约往返。
func TestFuncExecute(t *testing.T) {
	f := NewFunc("echo", "", nil, true, func(_ context.Context, args map[string]any) (Result, error) {
		return Text(args["q"].(string)), nil
	})
	out, err := f.Execute(context.Background(), map[string]any{"q": "hi"})
	if err != nil || out.Content != "hi" {
		t.Fatalf("执行结果不符: %+v err=%v", out, err)
	}
}
