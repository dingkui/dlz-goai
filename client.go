// client.go — 最小嵌入门面。
//
// 把"发起可恢复的 Agent 运行并管理其生命周期"收敛为一个 Client：
// 应用启动时装配一次（模型 + 默认工具 + Store），之后每次交互只需
// Start → Wait/Stream → Approve，无需手工管理运行 goroutine、取消表
// 与事件衔接。这是[嵌入式集成架构设计](docs/embedded-integration-design.md)
// 阶段一的最小实现——Config/Registry/Preset 全体系等真实需求出现后再评估。
//
// 语义要点：
//   - Start/Resume 在运行登记就绪后返回 Run 句柄，执行在 Client 管理的后台 goroutine 中；
//     调用方 ctx 只控制提交过程，不影响运行本身（前端断开 ≠ 取消）；
//   - Wait 的 ctx 取消只停止等待，不取消运行；取消用 Cancel；
//   - 审批经 Client.Approve 在任意 HTTP 请求中提交，与事件流解耦；
//   - Close 取消全部活跃运行并等待其退出；应用注入的 Runtime 与模型
//     资源归应用所有，Close 不会触碰。
//
// 需要钩子（Transform/OnStep）、自选存储粒度等高级能力时，直接使用
// runtime 包——门面只是薄封装，不做能力屏蔽。
package dlzgoai

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/runtime/memory"
	"github.com/dingkui/dlz-goai/tool"
)

// 门面层错误。
var (
	// ErrNoModel NewClient 未提供模型调用函数。
	ErrResumeConfigRequired = errors.New("dlzgoai: resume requires original configuration; use ResumeWith or ResumeResolver")
	ErrNoModel              = errors.New("dlzgoai: model is required")
	// ErrClientClosed Client 已 Close，不能再发起运行。
	ErrClientClosed = errors.New("dlzgoai: client is closed")
	// ErrNoInput Request 既没有 Messages 也没有 Input。
	ErrNoInput = errors.New("dlzgoai: request requires Input or Messages")
	// ErrRunNotManaged Wait/Cancel 目标运行不属于本 Client：
	// 已被 Close 释放，或属于另一个进程（跨进程查状态用 GetRun/Replay/Stream）。
	ErrRunNotManaged = errors.New("dlzgoai: run not managed by this client")
)

// Options Client 装配项。
type Options struct {
	// ResumeResolver reconstructs trusted configuration after restart or cache eviction.
	ResumeResolver func(context.Context, string) (Request, error)
	// MaxCompletedRuns bounds cached results. Zero uses 128; negative disables caching.
	MaxCompletedRuns int
	// Model 默认模型调用函数。可选：多模型应用（按请求选择模型/服务商）
	// 可省略，改为在 Request.Model 中逐请求提供；两者都缺时 Start 报 ErrNoModel。
	// 通常一行适配内置 provider：
	//
	//	func(ctx context.Context, msgs []message.Message, opts *message.Options,
	//		cb func(message.Delta)) error {
	//		return provider.ChatStream(ctx, "qwen3:8b", msgs, opts, cb)
	//	}
	Model agent.ModelFunc
	// Tools 默认工具集：Start 未显式指定、以及 Resume 续跑时使用。
	// Client 持有工具集正是为了免去应用在 Resume 时手工重提供。
	Tools []tool.Tool
	// Runtime 持久化运行时。nil 时自建内存实现（开发调试用，不跨进程）；
	// 需要"重启后恢复"请用 sqlite.OpenRuntime 装配后传入。
	// 所有权归应用：Close 不会关闭它。
	Runtime *runtime.Runtime
}

// Request 一次运行请求。
type Request struct {
	// RunID optionally supplies a unique application-owned ID, allowing configuration to be saved before submission.
	RunID string
	// Input 便捷输入：非空时作为单条 user 消息（Messages 非空时忽略本字段）。
	Input string
	// Messages 完整输入（多轮、带图片、含 system 等）。
	Messages []message.Message
	// Options 推理参数（System/Temperature/MaxTokens 等）。
	Options *message.Options
	// Tools 覆盖 Client 默认工具集；nil 时用默认。
	Tools []tool.Tool
	// Model 覆盖 Client 默认模型——同一应用按请求选择不同模型/服务商时使用；
	// nil 时用 Options.Model。
	Model agent.ModelFunc
	// 以下透传给 agent.Config，语义见 agent 包。
	MaxSteps           int
	ToolTimeout        time.Duration
	MaxToolResultBytes int
	ToolExecution      agent.ToolExecution
	Policies           map[string]tool.Policy
}

// Run 一次运行的生命周期句柄。轻量值，可安全跨 goroutine 传递。
type Run struct {
	id    string
	c     *Client
	state *runState
}

// ID 运行标识（持久化、跨进程重连时使用）。
func (r *Run) ID() string { return r.id }

// Wait 等待运行结束（见 Client.Wait）。
func (r *Run) Wait(ctx context.Context) (agent.Result, error) { return waitState(ctx, r.state) }

// Cancel 取消运行（见 Client.Cancel）。
func (r *Run) Cancel() bool { return cancelState(r.state) }

// Approve 提交本运行的一次审批决策（见 Client.Approve）。
func (r *Run) Approve(callID string, approved bool) error {
	return r.c.Approve(r.id, callID, approved)
}

// Stream 交付本运行事件流（见 Client.Stream）。
func (r *Run) Stream(ctx context.Context, afterSeq int64, emit agent.Emitter) error {
	return r.c.Stream(ctx, r.id, afterSeq, emit)
}

// Get 查询运行登记状态（见 Client.GetRun）。
func (r *Run) Get(ctx context.Context) (runtime.RunRecord, error) { return r.c.GetRun(ctx, r.id) }

// runState 一次受管运行的内部状态。
// 完成后按缓存上限保留，Run 句柄独立持有自身执行结果。
type runState struct {
	request   Request
	completed uint64
	done      chan struct{}      // 运行结束（含结果写入）后关闭
	cancel    context.CancelFunc // 取消运行（Close/Cancel 调用；完成后由运行 goroutine 自行释放）
	result    agent.Result
	err       error
}

// Client 应用级共享运行入口：装配一次，全部会话复用。
// 可并发使用；全部跨运行状态都在注入的 Runtime 里。
type Client struct {
	resolver      func(context.Context, string) (Request, error)
	maxCompleted  int
	completionSeq uint64
	model         agent.ModelFunc
	tools         []tool.Tool
	rt            *runtime.Runtime

	mu     sync.Mutex
	runs   map[string]*runState
	closed bool
	once   sync.Once
}

// NewClient 装配并校验。Runtime 未提供时自建内存实现。
// Model 可省略（改为逐请求在 Request.Model 提供）。
func NewClient(opts Options) (*Client, error) {
	rt := opts.Runtime
	if rt == nil {
		rt = runtime.New(runtime.Options{
			Runs:        memory.NewRunStore(),
			Events:      memory.NewEventStore(),
			Checkpoints: memory.NewCheckpointStore(),
		})
	}
	limit := opts.MaxCompletedRuns
	if limit == 0 {
		limit = 128
	}
	if limit < 0 {
		limit = 0
	}
	return &Client{
		resolver: opts.ResumeResolver, maxCompleted: limit,
		model: opts.Model,
		tools: append([]tool.Tool(nil), opts.Tools...),
		rt:    rt,
		runs:  map[string]*runState{},
	}, nil
}

// Runtime 暴露底层运行时（诊断、直接调用其高级能力时使用）。
func (c *Client) Runtime() *runtime.Runtime { return c.rt }

// Start 发起一次运行并在登记就绪后返回句柄；执行在 Client 管理的后台 goroutine 中。
// ctx 只控制提交过程。错误在 Wait 中呈现（持久化失败、模型失败等——
// 见 runtime 的 fail-closed 语义）。
func (c *Client) Start(ctx context.Context, req Request) (*Run, error) {
	msgs, err := req.messages()
	if err != nil {
		return nil, err
	}
	if req.Model == nil && c.model == nil {
		return nil, ErrNoModel
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.launch(ctx, req, req.RunID, msgs)
}

// Resume 从检查点恢复一次受管运行，沿用原请求配置。
// 典型场景：进程重启后 Recover，再对用户确认过的 runID 逐个恢复。
func (c *Client) Resume(ctx context.Context, runID string) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClientClosed
	}
	st := c.runs[runID]
	if st != nil {
		select {
		case <-st.done:
		default:
			c.mu.Unlock()
			return nil, runtime.ErrRunActive
		}
	}
	c.mu.Unlock()
	if st != nil {
		return c.ResumeWith(ctx, runID, st.request)
	}
	if c.resolver == nil {
		return nil, ErrResumeConfigRequired
	}
	req, err := c.resolver(ctx, runID)
	if err != nil {
		return nil, err
	}
	return c.ResumeWith(ctx, runID, req)
}

// ResumeWith restores execution using application-verified original configuration.
// Input/Messages/Options come from the checkpoint. Model is required and Tools
// is the complete authorized set (nil means no tools, not Client defaults).
func (c *Client) ResumeWith(ctx context.Context, runID string, req Request) (*Run, error) {
	if runID == "" {
		return nil, runtime.ErrRunNotFound
	}
	if req.Model == nil {
		return nil, ErrNoModel
	}
	if req.Tools == nil {
		req.Tools = []tool.Tool{}
	}
	return c.launch(ctx, req, runID, nil)
}

func (c *Client) launch(ctx context.Context, req Request, runID string, msgs []message.Message) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resume := msgs == nil
	if req.Model == nil {
		req.Model = c.model
	}
	if req.Tools == nil {
		req.Tools = c.tools
	}
	req.Tools = append([]tool.Tool{}, req.Tools...)
	req.Policies = maps.Clone(req.Policies)
	saved := req
	saved.Input, saved.Messages, saved.Options = "", nil, nil
	bgCtx, cancel := context.WithCancel(context.Background())
	st := &runState{done: make(chan struct{}), cancel: cancel, request: saved}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return nil, ErrClientClosed
	}
	if previous := c.runs[runID]; previous != nil {
		select {
		case <-previous.done:
		default:
			c.mu.Unlock()
			cancel()
			return nil, runtime.ErrRunActive
		}
	}
	if !resume && runID == "" {
		runID = c.rt.BeginRun()
	}
	c.runs[runID] = st
	c.mu.Unlock()
	cfg := c.config(req, runID)
	ready := make(chan struct{})
	go func() {
		defer func() {
			cancel()
			c.mu.Lock()
			c.completionSeq++
			st.completed = c.completionSeq
			close(st.done)
			c.pruneLocked()
			c.mu.Unlock()
		}()
		if resume {
			st.result, st.err = c.rt.ResumeReady(bgCtx, runID, cfg, req.Model, nil, func() { close(ready) })
		} else {
			st.result, st.err = c.rt.RunReady(bgCtx, msgs, req.Options, cfg, req.Model, nil, func() { close(ready) })
		}
	}()
	handle := &Run{id: runID, c: c, state: st}
	select {
	case <-ready:
		return handle, nil
	case <-st.done:
		select {
		case <-ready:
			return handle, nil
		default:
			return nil, st.err
		}
	case <-ctx.Done():
		cancel()
		<-st.done
		return nil, ctx.Err()
	}
}

// Forget releases a completed result/configuration while retaining persistent history.
// Existing Run handles still return results for their own execution attempt.
func (c *Client) Forget(runID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.runs[runID]
	if st == nil {
		return false
	}
	select {
	case <-st.done:
		delete(c.runs, runID)
		return true
	default:
		return false
	}
}

func (c *Client) pruneLocked() {
	for {
		count := 0
		var oldest string
		var seq uint64
		for id, st := range c.runs {
			if st.completed == 0 {
				continue
			}
			count++
			if oldest == "" || st.completed < seq {
				oldest, seq = id, st.completed
			}
		}
		if count <= c.maxCompleted {
			return
		}
		delete(c.runs, oldest)
	}
}

func waitState(ctx context.Context, st *runState) (agent.Result, error) {
	select {
	case <-st.done:
		return st.result, st.err
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}
}

func cancelState(st *runState) bool {
	if st == nil {
		return false
	}
	select {
	case <-st.done:
		return false
	default:
		st.cancel()
		return true
	}
}

// config 组装 agent.Config。Approve 不设置——runtime 自动绑定内部
// Broker，决策经 Client.Approve 提交；钩子类字段不透传（高级场景直接用 runtime）。
func (c *Client) config(req Request, runID string) agent.Config {
	tools := req.Tools
	if tools == nil {
		tools = c.tools
	}
	return agent.Config{
		RunID:              runID,
		Tools:              tools,
		MaxSteps:           req.MaxSteps,
		ToolTimeout:        req.ToolTimeout,
		MaxToolResultBytes: req.MaxToolResultBytes,
		ToolExecution:      req.ToolExecution,
		Policies:           req.Policies,
	}
}

// Wait 等待运行结束，返回最终结果。
// ctx 取消只停止等待，不影响运行；取消运行用 Cancel。
// 运行不属于本 Client（已 Close 释放 / 跨进程）返回 ErrRunNotManaged——
// 那种情况用 GetRun 查状态、Replay/Stream 取事件。
func (c *Client) Wait(ctx context.Context, runID string) (agent.Result, error) {
	st, ok := c.lookup(runID)
	if !ok {
		return agent.Result{}, ErrRunNotManaged
	}
	select {
	case <-st.done:
		return st.result, st.err
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}
}

// Cancel 取消一次进行中的运行。返回是否属于本 Client 且已提交取消。
// 取消是异步的：Wait 会随后返回（错误为取消原因），状态登记为 canceled。
func (c *Client) Cancel(runID string) bool {
	c.mu.Lock()
	st, ok := c.runs[runID]
	c.mu.Unlock()
	if !ok {
		return false
	}
	return cancelState(st)
}

// Approve 提交一次审批决策（解除运行中阻塞的审批等待）。
func (c *Client) Approve(runID, callID string, approved bool) error {
	return c.rt.Approve(runID, callID, approved)
}

// GetRun 查询运行登记（含跨进程的历史运行）。
func (c *Client) GetRun(ctx context.Context, runID string) (runtime.RunRecord, error) {
	return c.rt.Get(ctx, runID)
}

// Subscribe 订阅运行实时事件（缓冲有限会丢事件；运行结束通道关闭）。
// 需要完整序列用 Stream 或 Replay。
func (c *Client) Subscribe(runID string) (<-chan agent.Event, func()) {
	return c.rt.Subscribe(runID)
}

// Replay 回放运行全部已持久化事件。
func (c *Client) Replay(ctx context.Context, runID string) ([]agent.Event, error) {
	return c.rt.Replay(ctx, runID)
}

// Stream 交付完整事件流：按 afterSeq 补齐历史，再追实时（缺口自动补读）。
// 前端断线重连的标准入口。
func (c *Client) Stream(ctx context.Context, runID string, afterSeq int64, emit agent.Emitter) error {
	return c.rt.Stream(ctx, runID, afterSeq, emit)
}

// ActiveRuns 返回当前受管且尚未结束的运行 ID 列表（诊断/管理用）。
func (c *Client) ActiveRuns() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.runs))
	for id, st := range c.runs {
		select {
		case <-st.done:
		default:
			out = append(out, id)
		}
	}
	return out
}

// Close 取消全部活跃运行并等待其退出，释放受管运行缓存。
// 幂等；不关闭应用注入的 Runtime / 模型资源（所有权归应用）。
// Close 后 Start/Resume 返回 ErrClientClosed，Wait/Cancel 返回 ErrRunNotManaged。
func (c *Client) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		states := make([]*runState, 0, len(c.runs))
		for _, st := range c.runs {
			states = append(states, st)
		}
		c.runs = map[string]*runState{}
		c.mu.Unlock()
		for _, st := range states {
			st.cancel()
		}
		for _, st := range states {
			<-st.done
		}
	})
	return nil
}

func (c *Client) lookup(runID string) (*runState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.runs[runID]
	return st, ok
}

func (req Request) messages() ([]message.Message, error) {
	if len(req.Messages) > 0 {
		return req.Messages, nil
	}
	if req.Input != "" {
		return []message.Message{{Role: message.RoleUser, Content: req.Input}}, nil
	}
	return nil, ErrNoInput
}
