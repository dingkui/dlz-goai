package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/tool"
)

// Options 构造 Runtime 的依赖。三个 Store 都是接口注入，
// 不想持久化某一项时传 nil 即跳过该项。
type Options struct {
	Runs        RunStore
	Events      EventStore
	Checkpoints CheckpointStore
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
	seqs    map[string]int64
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
		seqs:        map[string]int64{},
	}
}

// BeginRun 生成一个可持久化的运行标识（复用 agent.Broker 的不可猜测 ID）。
func (rt *Runtime) BeginRun() string { return rt.broker.Begin() }

// Run 发起一次持久化运行：登记状态、事件落库、调用级存检查点、
// 审批等待期间置为 WaitingApproval。
//
// cfg.RunID 必填（用 BeginRun() 生成）；emit 收到的事件与 EventStore
// 落库内容一致。任何关键持久化失败（事件、检查点、状态登记）都会
// 中止运行并纳入返回错误（fail-closed）：持久化是恢复语义的前提，
// 宁可让运行失败，也不静默丢失记录后假装可以恢复。
func (rt *Runtime) Run(ctx context.Context, initial []message.Message, opts *message.Options,
	cfg agent.Config, model agent.ModelFunc, emit agent.Emitter) (agent.Result, error) {
	return rt.run(ctx, initial, opts, cfg, model, emit, false)
}

func (rt *Runtime) run(ctx context.Context, initial []message.Message, opts *message.Options,
	cfg agent.Config, model agent.ModelFunc, emit agent.Emitter, resume bool) (agent.Result, error) {

	runID := cfg.RunID
	if runID == "" {
		return agent.Result{}, errors.New("runtime: cfg.RunID is empty; generate one with BeginRun()")
	}
	runCtx, cancel := context.WithCancel(ctx)
	rt.mu.Lock()
	if _, active := rt.cancels[runID]; active {
		rt.mu.Unlock()
		cancel()
		return agent.Result{}, ErrRunActive
	}
	rt.cancels[runID] = cancel
	rt.mu.Unlock()
	defer func() {
		cancel()
		// 运行结束：关闭该 Run 的全部订阅通道。消费者收到关闭信号后
		// 可从 EventStore 补齐慢消费期间丢失的事件（见 Stream）。
		// 只关闭自己从注册表移除的通道，与 unsubscribe 互不重复关闭。
		rt.mu.Lock()
		delete(rt.cancels, runID)
		delete(rt.seqs, runID)
		subs := rt.subs[runID]
		delete(rt.subs, runID)
		rt.mu.Unlock()
		for ch := range subs {
			close(ch)
		}
		rt.broker.End(runID)
	}()

	if resume {
		if rt.events != nil {
			events, err := rt.events.List(ctx, runID)
			if err != nil {
				return agent.Result{}, err
			}
			var last int64
			for _, event := range events {
				if event.Seq > last {
					last = event.Seq
				}
			}
			rt.mu.Lock()
			rt.seqs[runID] = last
			rt.mu.Unlock()
		}
		if err := rt.updateRecord(runID, func(r *RunRecord) {
			r.Status = StatusPending
			r.Error = ""
			r.PendingApproval = nil
		}); err != nil {
			return agent.Result{}, fmt.Errorf("runtime: failed to reset run for resume: %w", err)
		}
	} else if rt.runs != nil {
		existing, err := rt.runs.Get(ctx, runID)
		if err == nil {
			if existing.Status.Terminal() {
				return agent.Result{}, ErrRunTerminal
			}
			return agent.Result{}, ErrRunActive
		}
		if !errors.Is(err, ErrRunNotFound) {
			return agent.Result{}, err
		}
		timestamp := now()
		if err := rt.runs.Create(ctx, RunRecord{
			ID: runID, Status: StatusPending, CreatedAt: timestamp, UpdatedAt: timestamp,
		}); err != nil {
			return agent.Result{}, err
		}
	}
	rt.broker.Bind(runID)
	if err := rt.setStatus(runID, StatusRunning); err != nil {
		return agent.Result{}, fmt.Errorf("runtime: failed to persist running status: %w", err)
	}

	// 持久化失败即中止（fail-closed）：事件与检查点是恢复语义的前提。
	// cancel 停止后续生成；首个错误保留为本次运行的最终错误。
	var persistErr error
	var persistOnce sync.Once
	failClosed := func(err error) {
		persistOnce.Do(func() {
			persistErr = fmt.Errorf("runtime: persistence failed, run aborted: %w", err)
			cancel()
		})
	}

	// 事件包装：落库 + 广播 + 转发
	// Runner 可并行执行多个工具；局部锁保证 Seq 分配、持久化和对外
	// 发送保持同一顺序，避免数据库中出现 seq=2 排在 seq=1 前面。
	// 落库用独立 context：调用方取消的收尾事件（含终态）仍要写进去。
	var eventMu sync.Mutex
	wrapped := func(e agent.Event) {
		eventMu.Lock()
		defer eventMu.Unlock()
		if e.RunID == "" {
			e.RunID = runID
		}
		rt.mu.Lock()
		rt.seqs[runID]++
		e.Seq = rt.seqs[runID]
		rt.mu.Unlock()
		if rt.events != nil {
			if err := rt.events.Append(context.Background(), runID, e); err != nil {
				failClosed(err)
			}
		}
		rt.broadcast(runID, e)
		if emit != nil {
			emit(e)
		}
	}
	if resume {
		wrapped(agent.Event{Type: agent.EventRunResumed, RunID: runID})
	}

	// 检查点：初始（保证首个审批等待前就有可恢复点）+ 调用级（每条
	// 工具回执入轨迹即存）+ 步级（整步收尾再存一次）。
	// 调用级检查点可能停在步中途，Resume 时由 trimDangling 回退到
	// 协议安全边界；被丢弃段中已完成的调用由事件对账复用（见 resume.go）。
	if rt.checkpoints != nil {
		if !resume {
			if err := rt.checkpoints.Save(context.Background(), Checkpoint{
				RunID: runID, Step: 0, CallIndex: -1,
				Messages: append([]message.Message(nil), initial...),
				Options:  opts, CreatedAt: now(),
			}); err != nil {
				return agent.Result{}, fmt.Errorf("runtime: failed to save initial checkpoint: %w", err)
			}
		}
		saveCheckpoint := func(step, callIndex int, messages []message.Message) {
			if err := rt.checkpoints.Save(context.Background(), Checkpoint{
				RunID: runID, Step: step, CallIndex: callIndex,
				Messages: append([]message.Message(nil), messages...),
				Options:  opts, CreatedAt: now(),
			}); err != nil {
				failClosed(err)
			}
		}
		innerStep := cfg.OnStep
		cfg.OnStep = func(step int, messages []message.Message) {
			saveCheckpoint(step, -1, messages)
			if err := rt.updateRecord(runID, func(r *RunRecord) { r.LastStep = step }); err != nil {
				failClosed(err)
			}
			if innerStep != nil {
				innerStep(step, messages)
			}
		}
		innerCall := cfg.OnToolDone
		cfg.OnToolDone = func(step, callIndex int, call tool.Call, result message.Message, messages []message.Message) {
			saveCheckpoint(step, callIndex, messages)
			if innerCall != nil {
				innerCall(step, callIndex, call, result, messages)
			}
		}
	}

	// 审批由 Runtime 的 Broker 默认承接，调用方可通过 Approve 在另一个
	// HTTP 请求中提交决策；显式提供 Approve 时仍尊重调用方实现。
	// 审批前后的状态登记失败会中止运行——带着错误的持久化状态继续
	// 执行比中断更危险（重启后会误判运行状态）。
	if cfg.Approve == nil {
		cfg.Approve = rt.ApprovalHandler(runID)
	}
	innerApprove := cfg.Approve
	cfg.Approve = agent.ApprovalFunc(func(ctx context.Context, req agent.ApprovalRequest) (bool, error) {
		if err := rt.updateRecord(runID, func(r *RunRecord) {
			r.Status = StatusWaitingApproval
			r.PendingApproval = &PendingApproval{
				CallID: req.CallID, ToolName: req.ToolName,
				SourceName: req.SourceName, Arguments: req.Arguments,
			}
		}); err != nil {
			return false, fmt.Errorf("runtime: failed to persist approval wait: %w", err)
		}
		decision, derr := innerApprove.Request(ctx, req)
		if uerr := rt.updateRecord(runID, func(r *RunRecord) {
			r.Status = StatusRunning
			r.PendingApproval = nil
		}); uerr != nil && derr == nil {
			return false, fmt.Errorf("runtime: failed to persist approval resume: %w", uerr)
		}
		return decision, derr
	})

	result, err := rt.runner.Run(runCtx, initial, opts, cfg, model, wrapped)

	// 终态判定：取消 → canceled；其余错误 → failed；
	// 持久化失败优先呈现（fail-closed 的本意就是让故障可见，
	// 即使取消路径上发生的落库失败也按 failed 记录原因）。
	final := StatusSucceeded
	errText := ""
	if err != nil {
		final = StatusFailed
		errText = err.Error()
		if ctx.Err() != nil || runCtx.Err() != nil {
			final = StatusCanceled
			errText = "canceled"
		}
	}
	if persistErr != nil {
		err = persistErr
		final = StatusFailed
		errText = persistErr.Error()
	}
	// 终态提交顺序：先登记状态，再写终态事件；两处失败都纳入返回值——
	// 不允许"数据库停在非终态 / 缺终态事件"的同时向调用方返回成功。
	if uerr := rt.updateRecord(runID, func(r *RunRecord) {
		r.Status = final
		r.Error = errText
		r.PendingApproval = nil
	}); uerr != nil {
		return result, fmt.Errorf("runtime: failed to persist terminal status: %w", uerr)
	}
	terminal := agent.Event{Type: agent.EventRunDone, RunID: runID,
		PromptTokens: result.PromptTokens, EvalTokens: result.EvalTokens,
		EvalMs: result.EvalMs, TotalMs: result.TotalMs}
	if err != nil {
		terminal.Type = agent.EventRunError
		terminal.Error = errText
	}
	persistErrBeforeTerminal := persistErr
	wrapped(terminal)
	if persistErr != persistErrBeforeTerminal {
		// 终态事件落库失败：登记降级为 failed（尽力而为），返回错误。
		_ = rt.updateRecord(runID, func(r *RunRecord) {
			r.Status = StatusFailed
			r.Error = persistErr.Error()
		})
		return result, persistErr
	}
	return result, err
}

// ApprovalHandler 返回指定运行的审批等待器。把它放入 agent.Config.Approve，
// 再通过 Runtime.Approve 提交决策，审批即可跨 HTTP 请求继续同一次运行。
func (rt *Runtime) ApprovalHandler(runID string) agent.ApprovalHandler {
	return rt.broker.For(runID)
}

// Resume 从最近检查点继续一次运行。工具集由调用方重新提供
// （检查点只保存"发生了什么"，不保存"能做什么"）；
// 恢复后引用从续跑点重新累积，历史引用可从 EventStore 回放。
//
// 恢复对账：轨迹先回退到最后一个协议安全边界（trimDangling），
// 之后事件库中已执行且有结果记录的调用，在模型重新发起同样调用时
// 直接复用结果、不重复执行；已开始执行但无结果记录的调用按工具的
// RetryPolicy 分级（见 resume.go）。
func (rt *Runtime) Resume(ctx context.Context, runID string,
	cfg agent.Config, model agent.ModelFunc, emit agent.Emitter) (agent.Result, error) {

	if rt.checkpoints == nil {
		return agent.Result{}, errors.New("runtime: no checkpoint store configured, cannot resume")
	}
	if rt.runs != nil {
		rec, err := rt.Get(ctx, runID)
		if err != nil {
			return agent.Result{}, err
		}
		switch rec.Status {
		case StatusPending, StatusRunning, StatusWaitingApproval:
			return agent.Result{}, ErrRunActive
		case StatusSucceeded:
			return agent.Result{}, ErrRunTerminal
		case StatusFailed, StatusCanceled:
			// 失败与取消都允许从最后一个完整步骤续跑。
		default:
			return agent.Result{}, errors.New("runtime: unknown run status " + string(rec.Status))
		}
	}
	cp, err := rt.checkpoints.Load(ctx, runID)
	if err != nil {
		return agent.Result{}, err
	}
	// 回退到协议安全边界：悬空的 tool_calls 会被部分模型服务拒绝；
	// 被丢弃段中已完成的调用由事件对账复用，不会重复执行。
	messages := trimDangling(cp.Messages)
	cfg.RunID = runID
	if len(cfg.Tools) > 0 {
		idx, idxErr := rt.buildResumeIndex(ctx, runID, messages)
		if idxErr != nil {
			return agent.Result{}, idxErr
		}
		if !idx.empty() {
			tools := make([]tool.Tool, len(cfg.Tools))
			for i, t := range cfg.Tools {
				tools[i] = &resumeTool{Tool: t, idx: idx}
			}
			cfg.Tools = tools
		}
	}
	return rt.run(ctx, messages, cp.Options, cfg, model, emit, true)
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

// Subscribe 订阅运行事件流，返回通道与取消订阅函数。
//
// 通道缓冲有限，慢消费者会丢事件（订阅面向 UI 广播）；运行结束时
// 通道会被关闭。需要完整、不丢的事件序列请用 Stream（自动补读缺口）
// 或 Replay。
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
		if _, stillRegistered := rt.subs[runID][ch]; stillRegistered {
			delete(rt.subs[runID], ch)
			if len(rt.subs[runID]) == 0 {
				delete(rt.subs, runID)
			}
			rt.mu.Unlock()
			close(ch)
			return
		}
		rt.mu.Unlock()
		// 通道已由运行结束路径关闭并移出注册表，此处不重复关闭。
	}
	return ch, unsubscribe
}

// Stream 先回放已持久化事件，再追随同一运行的实时事件。
// afterSeq 用于断线续传；传 0 表示从头回放。
//
// 实时通道缓冲有限，消费不及时会丢事件：检测到 Seq 缺口时自动从
// EventStore 补读；运行结束关闭通道后做最终补齐——因此 Stream 交付的
// 事件序列与 Replay 一致（Seq 连续、含终态事件），慢消费者不丢数据。
func (rt *Runtime) Stream(ctx context.Context, runID string, afterSeq int64, emit agent.Emitter) error {
	ch, unsubscribe := rt.Subscribe(runID)
	defer unsubscribe()
	events, err := rt.Replay(ctx, runID)
	if err != nil {
		return err
	}
	record, getErr := rt.Get(ctx, runID)
	if getErr != nil {
		return getErr
	}
	var maxPersistedSeq int64
	for _, event := range events {
		if event.Seq > maxPersistedSeq {
			maxPersistedSeq = event.Seq
		}
	}
	last := afterSeq
	for _, event := range events {
		if event.Seq <= last {
			continue
		}
		last = event.Seq
		terminal := event.Type == agent.EventRunDone || event.Type == agent.EventRunError
		// 同一 RunID 续跑时，历史尝试留下的 run_error 不是当前运行的
		// 终点。它仍保留在 Replay 审计数据中，但重新订阅时跳过，避免
		// UI 把已经恢复的运行误判为结束。
		if terminal && (event.Seq < maxPersistedSeq || !record.Status.Terminal()) {
			continue
		}
		emit.Emit(event)
		if terminal {
			return nil
		}
	}
	if record.Status.Terminal() {
		typ := agent.EventRunDone
		if record.Status != StatusSucceeded {
			typ = agent.EventRunError
		}
		emit.Emit(agent.Event{Type: typ, RunID: runID, Seq: last + 1, Error: record.Error})
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-ch:
			if !ok {
				// 运行已结束且通道关闭：最终补齐，交付慢消费期间
				// 丢掉的事件（含终态事件）。
				return rt.streamFinalBackfill(ctx, runID, last, emit)
			}
			if event.Seq <= last {
				continue
			}
			if event.Seq > last+1 {
				// 缺口：补读 (last, event.Seq) 之间被丢弃的事件。
				filled, err := rt.streamBackfillRange(ctx, runID, last, event.Seq, emit)
				if err != nil {
					return err
				}
				last = filled
			}
			emit.Emit(event)
			last = event.Seq
			if event.Type == agent.EventRunDone || event.Type == agent.EventRunError {
				return nil
			}
		}
	}
}

// streamBackfillRange 补读 (last, upper) 开区间内已持久化的事件并交付。
// 终态事件不在补读范围——当前运行的终点由实时通道或最终补齐交付，
// 历史尝试的终点事件不应中断本次订阅。
func (rt *Runtime) streamBackfillRange(ctx context.Context, runID string, last, upper int64, emit agent.Emitter) (int64, error) {
	events, err := rt.Replay(ctx, runID)
	if err != nil {
		return last, err
	}
	for _, event := range events {
		if event.Seq <= last || event.Seq >= upper {
			continue
		}
		if event.Type == agent.EventRunDone || event.Type == agent.EventRunError {
			continue
		}
		last = event.Seq
		emit.Emit(event)
	}
	return last, nil
}

// streamFinalBackfill 运行结束后的最终补齐：交付 last 之后全部已持久化
// 事件。Store 中最后一个事件即本次终点；若终态事件因落库失败缺失，
// 用登记状态合成一个兜底，保证消费者能看到明确的结束信号。
func (rt *Runtime) streamFinalBackfill(ctx context.Context, runID string, last int64, emit agent.Emitter) error {
	events, err := rt.Replay(ctx, runID)
	if err != nil {
		return err
	}
	sawTerminal := false
	if n := len(events); n > 0 {
		maxSeq := events[n-1].Seq
		for _, event := range events {
			if event.Seq <= last {
				continue
			}
			terminal := event.Type == agent.EventRunDone || event.Type == agent.EventRunError
			// 历史尝试的终点事件跳过；本次终点是 Store 中最后一个事件。
			if terminal && event.Seq != maxSeq {
				continue
			}
			last = event.Seq
			emit.Emit(event)
			if terminal {
				sawTerminal = true
			}
		}
	}
	if !sawTerminal {
		record, getErr := rt.Get(ctx, runID)
		if getErr == nil && record.Status.Terminal() {
			typ := agent.EventRunDone
			if record.Status != StatusSucceeded {
				typ = agent.EventRunError
			}
			emit.Emit(agent.Event{Type: typ, RunID: runID, Seq: last + 1, Error: record.Error})
		}
	}
	return nil
}

// Get 查询一次运行的登记信息。
func (rt *Runtime) Get(ctx context.Context, runID string) (RunRecord, error) {
	if rt.runs == nil {
		return RunRecord{}, ErrRunNotFound
	}
	return rt.runs.Get(ctx, runID)
}

// Recover 进程重启后的恢复：把停留在非终态（运行中/等待审批）的
// 登记统一标记为失败并注明原因，返回受影响数量。等待审批的调用信息
// 作为审计和 UI 提示保留，但原等待器已不存在；之后需从检查点 Resume。
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
		rec.Error = "process interrupted; resumable from checkpoint"
		if rec.PendingApproval != nil {
			rec.Error = "process interrupted while waiting for approval; confirm and resume from checkpoint"
		}
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

func (rt *Runtime) setStatus(runID string, status Status) error {
	return rt.updateRecord(runID, func(r *RunRecord) { r.Status = status })
}

func (rt *Runtime) updateRecord(runID string, mutate func(*RunRecord)) error {
	if rt.runs == nil {
		return nil
	}
	ctx := context.Background()
	record, err := rt.runs.Get(ctx, runID)
	if err != nil {
		return err
	}
	mutate(&record)
	record.UpdatedAt = now()
	return rt.runs.Update(ctx, record)
}
