package runtime

import (
	"context"
	"errors"
	"sync"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
)

// Options 构造 Runtime 的依赖。三个 Store 都是接口注入，
// 不想持久化某一项时传 nil 即跳过该项。
type Options struct {
	Runs         RunStore
	Events       EventStore
	Checkpoints  CheckpointStore
	// Broker 可选；为空时内部自建（Approve 只对进行中的运行有效）。
	Broker *agent.Broker
}

// Runtime 持久化运行时：包装 agent.Runner，把一次运行变成
// 可登记、可回放、可恢复、可订阅的过程。
//
// 一个 Runtime 实例可并发跑多个运行；实例本身无业务状态，
// 所有跨运行状态都在注入的 Store 里。
type Runtime struct {
	runs        RunStore
	events      EventStore
	checkpoints CheckpointStore
	broker      *agent.Broker
	runner      *agent.Runner

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	subs    map[string]map[chan agent.Event]struct{}
}

// New 构造 Runtime。
func New(opts Options) *Runtime {
	broker := opts.Broker
	if broker == nil {
		broker = agent.NewBroker()
	}
	return &Runtime{
		runs:        opts.Runs,
		events:      opts.Events,
		checkpoints: opts.Checkpoints,
		broker:      broker,
		runner:      agent.New(),
		cancels:     map[string]context.CancelFunc{},
		subs:        map[string]map[chan agent.Event]struct{}{},
	}
}

// BeginRun 生成一个可持久化的运行标识（复用 agent.Broker 的不可猜测 ID）。
func (rt *Runtime) BeginRun() string { return rt.broker.Begin() }

// Run 发起一次持久化运行：登记状态、事件落库、每步存检查点、
// 审批等待期间置为 WaitingApproval。
//
// cfg.RunID 必填（用 BeginRun 生成）；emit 收到的事件与 EventStore
// 落库内容一致。事件落库失败不中断生成——持久化尽力而为，
// 强一致需求由实现方在 Store 里保证。
func (rt *Runtime) Run(ctx context.Context, initial []message.Message, opts *message.Options,
	cfg agent.Config, model agent.ModelFunc, emit agent.Emitter) (agent.Result, error) {

	runID := cfg.RunID
	if runID == "" {
		return agent.Result{}, errors.New("runtime: cfg.RunID 为空，用 BeginRun() 生成")
	}
	runCtx, cancel := context.WithCancel(ctx)
	rt.mu.Lock()
	rt.cancels[runID] = cancel
	rt.mu.Unlock()
	defer func() {
		cancel()
		rt.mu.Lock()
		delete(rt.cancels, runID)
		rt.mu.Unlock()
		rt.broker.End(runID)
	}()

	rt.putRecord(RunRecord{ID: runID, Status: StatusPending, CreatedAt: now(), UpdatedAt: now()})
	rt.setStatus(runID, StatusRunning)

	// 事件包装：落库 + 广播 + 转发
	wrapped := func(e agent.Event) {
		if e.RunID == "" {
			e.RunID = runID
		}
		if rt.events != nil {
			_ = rt.events.Append(runCtx, runID, e)
		}
		rt.broadcast(runID, e)
		if emit != nil {
			emit(e)
		}
	}

	// 检查点：每步结束保存消息轨迹（保留调用方自己的 OnStep）
	if rt.checkpoints != nil {
		inner := cfg.OnStep
		cfg.OnStep = func(step int, messages []message.Message) {
			_ = rt.checkpoints.Save(runCtx, Checkpoint{
				RunID: runID, Step: step,
				Messages: append([]message.Message(nil), messages...),
				Options:  opts, CreatedAt: now(),
			})
			rt.updateRecord(runID, func(r *RunRecord) { r.LastStep = step })
			if inner != nil {
				inner(step, messages)
			}
		}
	}

	// 审批包装：等待前登记待审批调用，决策后清空
	if cfg.Approve != nil {
		inner := cfg.Approve
		cfg.Approve = agent.ApprovalFunc(func(ctx context.Context, req agent.ApprovalRequest) (bool, error) {
			rt.updateRecord(runID, func(r *RunRecord) {
				r.Status = StatusWaitingApproval
				r.PendingApproval = &PendingApproval{
					CallID: req.CallID, ToolName: req.ToolName,
					SourceName: req.SourceName, Arguments: req.Arguments,
				}
			})
			defer rt.updateRecord(runID, func(r *RunRecord) {
				r.Status = StatusRunning
				r.PendingApproval = nil
			})
			return inner.Request(ctx, req)
		})
	}

	result, err := rt.runner.Run(runCtx, initial, opts, cfg, model, wrapped)

	// 终态判定：context 取消 → canceled；其余错误 → failed
	final := StatusSucceeded
	errText := ""
	if err != nil {
		final = StatusFailed
		errText = err.Error()
		if ctx.Err() != nil || runCtx.Err() != nil {
			final = StatusCanceled
			errText = "已取消"
		}
	}
	rt.updateRecord(runID, func(r *RunRecord) {
		r.Status = final
		r.Error = errText
		r.PendingApproval = nil
	})
	return result, err
}

// Resume 从最近检查点继续一次运行。工具集由调用方重新提供
// （检查点只保存"发生了什么"，不保存"能做什么"）；
// 恢复后引用从续跑点重新累积，历史引用可从 EventStore 回放。
func (rt *Runtime) Resume(ctx context.Context, runID string,
	cfg agent.Config, model agent.ModelFunc, emit agent.Emitter) (agent.Result, error) {

	if rt.checkpoints == nil {
		return agent.Result{}, errors.New("runtime: 未配置检查点存储，无法恢复")
	}
	cp, err := rt.checkpoints.Load(ctx, runID)
	if err != nil {
		return agent.Result{}, err
	}
	if rec, rErr := rt.Get(ctx, runID); rErr == nil && rec.Status == StatusRunning {
		return agent.Result{}, ErrRunTerminal
	}
	cfg.RunID = runID
	return rt.Run(ctx, cp.Messages, cp.Options, cfg, model, emit)
}

// Approve 提交审批决策（等待中的运行由审批 Handler 解除阻塞）。
func (rt *Runtime) Approve(runID, callID string, approved bool) error {
	return rt.broker.Resolve(runID, callID, approved)
}

// Cancel 取消一次进行中的运行。返回是否有该运行。
func (rt *Runtime) Cancel(runID string) bool {
	rt.mu.Lock()
	cancel, ok := rt.cancels[runID]
	rt.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Replay 回放一次运行的全部事件（顺序即发生顺序）。
func (rt *Runtime) Replay(ctx context.Context, runID string) ([]agent.Event, error) {
	if rt.events == nil {
		return nil, nil
	}
	return rt.events.List(ctx, runID)
}

// Subscribe 订阅进行中运行的事件流，返回通道与取消订阅函数。
// 通道缓冲有限，慢消费者会丢事件（订阅面向 UI 广播，
// 需要完整事件请用 Replay）。
func (rt *Runtime) Subscribe(runID string) (<-chan agent.Event, func()) {
	ch := make(chan agent.Event, 256)
	rt.mu.Lock()
	if rt.subs[runID] == nil {
		rt.subs[runID] = map[chan agent.Event]struct{}{}
	}
	rt.subs[runID][ch] = struct{}{}
	rt.mu.Unlock()
	unsubscribe := func() {
		rt.mu.Lock()
		delete(rt.subs[runID], ch)
		if len(rt.subs[runID]) == 0 {
			delete(rt.subs, runID)
		}
		rt.mu.Unlock()
		close(ch)
	}
	return ch, unsubscribe
}

// Get 查询一次运行的登记信息。
func (rt *Runtime) Get(ctx context.Context, runID string) (RunRecord, error) {
	if rt.runs == nil {
		return RunRecord{}, ErrRunNotFound
	}
	return rt.runs.Get(ctx, runID)
}

// Recover 进程重启后的恢复：把停留在非终态（运行中/等待审批）的
// 登记统一标记为失败并注明原因，返回受影响数量。
// 之后调用方可按需从检查点 Resume。
func (rt *Runtime) Recover(ctx context.Context) (int, error) {
	if rt.runs == nil {
		return 0, nil
	}
	stuck, err := rt.runs.List(ctx, StatusPending, StatusRunning, StatusWaitingApproval)
	if err != nil {
		return 0, err
	}
	for _, rec := range stuck {
		rec.Status = StatusFailed
		rec.Error = "进程中断，可从检查点恢复"
		rec.PendingApproval = nil
		if err := rt.runs.Update(ctx, rec); err != nil {
			return 0, err
		}
	}
	return len(stuck), nil
}

func (rt *Runtime) broadcast(runID string, e agent.Event) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for ch := range rt.subs[runID] {
		select {
		case ch <- e:
		default: // 慢消费者丢弃
		}
	}
}

func (rt *Runtime) putRecord(record RunRecord) {
	if rt.runs != nil {
		_ = rt.runs.Create(context.Background(), record)
	}
}

func (rt *Runtime) setStatus(runID string, status Status) {
	rt.updateRecord(runID, func(r *RunRecord) { r.Status = status })
}

func (rt *Runtime) updateRecord(runID string, mutate func(*RunRecord)) {
	if rt.runs == nil {
		return
	}
	ctx := context.Background()
	record, err := rt.runs.Get(ctx, runID)
	if err != nil {
		return
	}
	mutate(&record)
	record.UpdatedAt = now()
	_ = rt.runs.Update(ctx, record)
}

