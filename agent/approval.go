package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
)

var (
	// ErrRunNotFound 运行不存在或已结束。
	ErrRunNotFound = errors.New("agent: run not found or already finished")
	// ErrApprovalNotPending 该调用当前不在等待审批。
	ErrApprovalNotPending = errors.New("agent: tool call is not pending approval")
	// ErrApprovalAborted 等待审批时被中断（上下文取消或运行结束）。
	ErrApprovalAborted = errors.New("agent: tool approval aborted")
)

// ApprovalRequest 描述一次需要确认的工具调用。
type ApprovalRequest struct {
	Step       int
	CallID     string
	ToolName   string
	SourceID   string
	SourceName string
	Arguments  map[string]any
}

// ApprovalHandler 决定一个有副作用的工具调用能否执行。
//
// 实现可以是交互式的（弹窗、HTTP 端点、命令行询问），
// 也可以是静态策略（全部放行、按白名单放行）。
// Runner 不关心是哪种，只关心返回的布尔值。
type ApprovalHandler interface {
	Request(ctx context.Context, req ApprovalRequest) (bool, error)
}

// ApprovalFunc 把普通函数适配成 ApprovalHandler。
type ApprovalFunc func(context.Context, ApprovalRequest) (bool, error)

func (f ApprovalFunc) Request(ctx context.Context, req ApprovalRequest) (bool, error) {
	if f == nil {
		return false, nil
	}
	return f(ctx, req)
}

// AutoApprove 全部放行。用于工具都是只读、或运行在可信环境中的场景。
func AutoApprove() ApprovalHandler {
	return ApprovalFunc(func(context.Context, ApprovalRequest) (bool, error) { return true, nil })
}

// DenyAll 全部拒绝。用于演练运行、或只想观察模型会调用什么工具的场景。
func DenyAll() ApprovalHandler {
	return ApprovalFunc(func(context.Context, ApprovalRequest) (bool, error) { return false, nil })
}

// Broker 把"运行中的等待"与"外部的决策"解耦。
//
// 典型场景：流式运行阻塞在工具调用上等待用户点击，
// 而用户决策通过另一个 HTTP 请求到达。两者通过 runID + callID 相遇。
// runID 随机生成且不可猜测，避免他人替你批准操作。
type Broker struct {
	mu   sync.Mutex
	runs map[string]map[string]chan bool
}

// NewBroker 构造 Broker。
func NewBroker() *Broker {
	return &Broker{runs: map[string]map[string]chan bool{}}
}

// Begin 登记一次新运行，返回不可猜测的标识符。
func (b *Broker) Begin() string {
	for {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			panic("agent: crypto/rand 不可用: " + err.Error())
		}
		id := hex.EncodeToString(raw[:])
		b.mu.Lock()
		if _, exists := b.runs[id]; !exists {
			b.runs[id] = map[string]chan bool{}
			b.mu.Unlock()
			return id
		}
		b.mu.Unlock()
	}
}

// Bind 确保指定运行已登记。Runtime 用它支持调用方自定义的 RunID，
// 以及进程内失败后使用同一 RunID 续跑；重复绑定是幂等的。
func (b *Broker) Bind(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.runs[runID]; !exists {
		b.runs[runID] = map[string]chan bool{}
	}
}

// End 结束运行，并把仍在等待的调用全部按拒绝处理——
// 悬空的 goroutine 是这类设计最容易泄漏的地方。
func (b *Broker) End(runID string) {
	b.mu.Lock()
	pending, ok := b.runs[runID]
	if ok {
		delete(b.runs, runID)
	}
	b.mu.Unlock()
	if !ok {
		return
	}
	for _, decision := range pending {
		close(decision)
	}
}

// For 返回绑定到某次运行的审批器，交给 Runner 使用。
func (b *Broker) For(runID string) ApprovalHandler {
	return ApprovalFunc(func(ctx context.Context, req ApprovalRequest) (bool, error) {
		return b.Wait(ctx, runID, req)
	})
}

// Wait 阻塞直到该调用被批准、被拒绝，或运行结束。
func (b *Broker) Wait(ctx context.Context, runID string, req ApprovalRequest) (bool, error) {
	decision := make(chan bool, 1)

	b.mu.Lock()
	pending, ok := b.runs[runID]
	if !ok {
		b.mu.Unlock()
		return false, ErrRunNotFound
	}
	if _, exists := pending[req.CallID]; exists {
		b.mu.Unlock()
		return false, errors.New("agent: duplicate approval call ID")
	}
	pending[req.CallID] = decision
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		if current, exists := b.runs[runID]; exists {
			if current[req.CallID] == decision {
				delete(current, req.CallID)
			}
		}
		b.mu.Unlock()
	}()

	select {
	case approved, open := <-decision:
		if !open {
			// 通道被关闭意味着运行已结束，按拒绝处理
			return false, ErrApprovalAborted
		}
		return approved, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Resolve 提交用户对某次调用的决策。
func (b *Broker) Resolve(runID, callID string, approved bool) error {
	b.mu.Lock()
	pending, ok := b.runs[runID]
	if !ok {
		b.mu.Unlock()
		return ErrRunNotFound
	}
	decision, ok := pending[callID]
	if ok {
		delete(pending, callID)
	}
	b.mu.Unlock()
	if !ok {
		return ErrApprovalNotPending
	}
	decision <- approved
	return nil
}
