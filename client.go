// client.go — 最小嵌入门面。
//
// 把"发起可恢复的 Agent 运行并管理其生命周期"收敛为一个 Client：
// 应用启动时装配一次（模型 + 默认工具 + Store），之后每次交互只需
// Start → Wait/Stream → Approve，无需手工管理运行 goroutine、取消表
// 与事件衔接。这是[嵌入式集成架构设计](docs/embedded-integration-design.md)
// 阶段一的最小实现——Config/Registry/Preset 全体系等真实需求出现后再评估。
//
// 语义要点：
//   - Start/Resume 立即返回 Run 句柄，执行在 Client 管理的后台 goroutine 中；
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
	ErrNoModel = errors.New("dlzgoai: model is required")
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
	// Model 模型调用函数（必填）。通常一行适配内置 provider：
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
	// Input 便捷输入：非空时作为单条 user 消息（Messages 非空时忽略本字段）。
	Input string
	// Messages 完整输入（多轮、带图片、含 system 等）。
	Messages []message.Message
	// Options 推理参数（System/Temperature/MaxTokens 等）。
	Options *message.Options
	// Tools 覆盖 Client 默认工具集；nil 时用默认。
	Tools []tool.Tool
	// 以下透传给 agent.Config，语义见 agent 包。
	MaxSteps           int
	ToolTimeout        time.Duration
	MaxToolResultBytes int
	ToolExecution      agent.ToolExecution
	Policies           map[string]tool.Policy
}

// Run 一次运行的生命周期句柄。轻量值，可安全跨 goroutine 传递。
type Run struct {
	id string
	c  *Client
}

// ID 运行标识（持久化、跨进程重连时使用）。
func (r *Run) ID() string { return r.id }

// Wait 等待运行结束（见 Client.Wait）。
func (r *Run) Wait(ctx context.Context) (agent.Result, error) { return r.c.Wait(ctx, r.id) }

// Cancel 取消运行（见 Client.Cancel）。
func (r *Run) Cancel() bool { return r.c.Cancel(r.id) }

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
// 完成后保留在 Client 中（缓存最终结果供 Wait 迟到调用），Close 时统一释放。
type runState struct {
	done   chan struct{}      // 运行结束（含结果写入）后关闭
	cancel context.CancelFunc // 取消运行（Close/Cancel 调用；完成后由运行 goroutine 自行释放）
	result agent.Result
	err    error
}

// Client 应用级共享运行入口：装配一次，全部会话复用。
// 可并发使用；全部跨运行状态都在注入的 Runtime 里。
type Client struct {
	model agent.ModelFunc
	tools []tool.Tool
	rt    *runtime.Runtime

	mu     sync.Mutex
	runs   map[string]*runState
	closed bool
	once   sync.Once
}

// NewClient 装配并校验。Runtime 未提供时自建内存实现。
func NewClient(opts Options) (*Client, error) {
	if opts.Model == nil {
		return nil, ErrNoModel
	}
	rt := opts.Runtime
	if rt == nil {
		rt = runtime.New(runtime.Options{
			Runs:        memory.NewRunStore(),
			Events:      memory.NewEventStore(),
			Checkpoints: memory.NewCheckpointStore(),
		})
	}
	return &Client{
		model: opts.Model,
		tools: append([]tool.Tool(nil), opts.Tools...),
		rt:    rt,
		runs:  map[string]*runState{},
	}, nil
}

// Runtime 暴露底层运行时（诊断、直接调用其高级能力时使用）。
func (c *Client) Runtime() *runtime.Runtime { return c.rt }

// Start 发起一次运行并立即返回句柄；执行在 Client 管理的后台 goroutine 中。
// ctx 只控制提交过程。错误在 Wait 中呈现（持久化失败、模型失败等——
// 见 runtime 的 fail-closed 语义）。
func (c *Client) Start(ctx context.Context, req Request) (*Run, error) {
	msgs, err := req.messages()
	if err != nil {
		return nil, err
	}
	_ = ctx // 提交过程当前无阻塞操作；保留参数以稳定签名（未来校验/装配可阻塞）
	return c.launch(ctx, req, "", msgs)
}

// Resume 从检查点恢复一次受管运行（工具用 Client 默认工具集）。
// 典型场景：进程重启后 Recover，再对用户确认过的 runID 逐个恢复。
func (c *Client) Resume(ctx context.Context, runID string) (*Run, error) {
	return c.launch(ctx, Request{}, runID, nil)
}

// launch 组装配置、登记运行状态并启动后台执行。
// runID 为空表示新运行（内部生成）；非空表示按该 ID 恢复（Resume）。
func (c *Client) launch(ctx context.Context, req Request, runID string,
	msgs []message.Message) (*Run, error) {

	bgCtx, cancel := context.WithCancel(context.Background())
	st := &runState{done: make(chan struct{}), cancel: cancel}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return nil, ErrClientClosed
	}
	if runID == "" {
		runID = c.rt.BeginRun()
	}
	c.runs[runID] = st
	c.mu.Unlock()

	cfg := c.config(req, runID)
	resume := req.Input == "" && len(req.Messages) == 0
	go func() {
		defer func() {
			cancel() // 释放 bgCtx 资源（对已结束运行无副作用）
			close(st.done)
		}()
		var res agent.Result
		var rerr error
		if resume {
			res, rerr = c.rt.Resume(bgCtx, runID, cfg, c.model, nil)
		} else {
			res, rerr = c.rt.Run(bgCtx, msgs, req.Options, cfg, c.model, nil)
		}
		// happens-before：先写结果再关 done，Wait 侧读到关闭即见结果
		st.result, st.err = res, rerr
	}()
	return &Run{id: runID, c: c}, nil
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
	st.cancel()
	return true
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
