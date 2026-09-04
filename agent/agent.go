// Package agent 提供受控的工具调用循环：模型 → 工具 → 模型，直到模型不再请求工具。
//
// 本包只依赖 message 与 tool，不依赖任何模型厂商、不依赖 MCP、不依赖存储。
// 工具来自哪里由调用方决定，只要它们实现 tool.Tool。
package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/tool"
)

// 默认约束。都是防御性的：防止模型陷入无限循环、
// 防止单个工具返回超大结果撑爆上下文。
const (
	DefaultMaxSteps           = 6
	DefaultToolTimeout        = 45 * time.Second
	DefaultMaxToolResultBytes = 128 * 1024
	// MaxStepsLimit 调用方设置值的上限，防止误配成天文数字。
	MaxStepsLimit = 20
)

// ErrMaxSteps 用尽步骤仍未收敛。
var ErrMaxSteps = errors.New("agent: 已达到最大步骤数")

// ErrNoTools 本次运行没有可用工具，继续跑没有意义。
var ErrNoTools = errors.New("agent: 当前运行没有可用工具")

// ErrNoModel 未提供模型调用函数。
var ErrNoModel = errors.New("agent: 未提供模型调用函数")

// ToolExecution 工具执行模式。
type ToolExecution string

const (
	// ToolSequential 串行执行（默认，也是零值）。
	ToolSequential ToolExecution = "sequential"
	// ToolParallel 并行执行。仅当一次模型响应包含多个工具调用时生效。
	ToolParallel ToolExecution = "parallel"
)

// ModelFunc 已绑定服务与模型的流式调用函数。
//
// agent 不知道底层是 OpenAI、Ollama 还是测试桩——它只负责把消息喂进去、
// 把增量收回来。这是 agent 与厂商解耦的接缝。
type ModelFunc func(ctx context.Context, messages []message.Message, opts *message.Options, cb func(message.Delta)) error

// Config 定义一次运行的工具集与安全边界。
type Config struct {
	// Tools 本次运行允许模型调用的工具。必填，为空直接报错。
	Tools []tool.Tool
	// Policies 按工具名覆盖策略。未列出的工具按是否声明只读决定。
	Policies map[string]tool.Policy
	// MaxSteps 步骤上限，超过返回 ErrMaxSteps。0 或超过 MaxStepsLimit 时用默认值。
	MaxSteps int
	// ToolTimeout 单次工具执行超时。0 时用默认值。
	ToolTimeout time.Duration
	// MaxToolResultBytes 单个工具结果的最大字节数，超出截断。0 时用默认值。
	MaxToolResultBytes int
	// Approve 有副作用工具的确认器。nil 时一律按拒绝处理——
	// 宁可不做，也不要在未确认的情况下执行危险操作。
	Approve ApprovalHandler
	// ToolExecution 工具执行模式。零值为串行，消息与事件顺序完全稳定；
	// Parallel 时同一响应中的多个工具调用并发执行，结果仍按调用顺序写回
	// （协议要求 tool 消息与调用一一对应），仅事件顺序不保证。
	ToolExecution ToolExecution
	// Transform 每轮调用模型前对上下文做裁剪或改写的可选钩子。
	// 长对话截断、历史压缩都挂在这里。
	Transform func(messages []message.Message) []message.Message
	// RunID 运行标识，会透传到事件里。为空时事件不带该字段。
	RunID string
}

// Result 一次运行的最终产物。
type Result struct {
	Content      string
	Messages     []message.Message
	Steps        int
	FinishReason string
	Citations    []tool.Citation
	// 用量统计为各步累加。Ollama 提供，OpenAI 兼容服务通常为 0。
	PromptTokens int
	EvalTokens   int
	EvalMs       int64
	TotalMs      int64
}

// Runner 执行工具调用循环。本身无状态，可并发复用。
type Runner struct{}

// New 构造 Runner。
func New() *Runner { return &Runner{} }

// Run 执行一次运行。
//
// 无论成功、失败还是被中断，返回的 Messages 都包含完整轨迹
// （含中断前已流出的部分内容），便于调用方原样持久化——
// 用户界面上显示过的文字，不应该因为一次中断就消失。
func (r *Runner) Run(ctx context.Context, initial []message.Message, opts *message.Options,
	cfg Config, model ModelFunc, emit Emitter) (Result, error) {

	if model == nil {
		return Result{}, ErrNoModel
	}
	registry := make(map[string]tool.Tool, len(cfg.Tools))
	definitions := make([]tool.Definition, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		name := t.Definition().Name
		if name == "" {
			continue
		}
		registry[name] = t
		definitions = append(definitions, t.Definition())
	}
	if len(registry) == 0 {
		return Result{}, ErrNoTools
	}

	runOpts := message.Options{}
	if opts != nil {
		runOpts = *opts
	}
	runOpts.Tools = definitions
	runOpts.ToolChoice = "auto"

	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 || maxSteps > MaxStepsLimit {
		maxSteps = DefaultMaxSteps
	}
	toolTimeout := cfg.ToolTimeout
	if toolTimeout <= 0 {
		toolTimeout = DefaultToolTimeout
	}
	maxResultBytes := cfg.MaxToolResultBytes
	if maxResultBytes <= 0 {
		maxResultBytes = DefaultMaxToolResultBytes
	}

	messages := append([]message.Message(nil), initial...)
	var citations []tool.Citation
	var usage Result

	emit.Emit(Event{Type: EventRunStart, RunID: cfg.RunID, Step: 0})

	for step := 1; step <= maxSteps; step++ {
		var content strings.Builder
		calls := newCallAccumulator(step)
		finishReason := ""
		var streamErr error

		prompt := messages
		if cfg.Transform != nil {
			prompt = cfg.Transform(messages)
		}
		err := model(ctx, prompt, &runOpts, func(delta message.Delta) {
			if delta.Error != "" && streamErr == nil {
				streamErr = errors.New(delta.Error)
			}
			if delta.Content != "" {
				content.WriteString(delta.Content)
				emit.Emit(Event{Type: EventModelDelta, RunID: cfg.RunID, Step: step, Content: delta.Content})
			}
			calls.Add(delta.ToolCalls)
			if delta.FinishReason != "" {
				finishReason = delta.FinishReason
			}
			if delta.PromptTokens > 0 {
				usage.PromptTokens = delta.PromptTokens
			}
			if delta.EvalTokens > 0 {
				usage.EvalTokens += delta.EvalTokens
			}
			if delta.EvalMs > 0 {
				usage.EvalMs += delta.EvalMs
			}
			if delta.TotalMs > 0 {
				usage.TotalMs += delta.TotalMs
			}
		})

		// 中断时也要把已流出的内容并入轨迹
		if err != nil || streamErr != nil {
			if content.Len() > 0 {
				messages = append(messages, message.Message{Role: message.RoleAssistant, Content: content.String()})
			}
			if err == nil {
				err = streamErr
			}
			usage.Messages = messages
			usage.Steps = step
			return usage, err
		}

		toolCalls := calls.Build()
		assistant := message.Message{Role: message.RoleAssistant, Content: content.String(), ToolCalls: toolCalls}
		if len(toolCalls) == 0 {
			assistant.Citations = append([]tool.Citation(nil), citations...)
		}
		messages = append(messages, assistant)

		if len(toolCalls) == 0 {
			if finishReason == "" {
				finishReason = "stop"
			}
			emit.Emit(Event{Type: EventFinal, RunID: cfg.RunID, Step: step,
				Content: content.String(), Citations: citations, Finish: finishReason})
			return Result{
				Content: content.String(), Messages: messages, Steps: step,
				FinishReason: finishReason, Citations: citations,
				PromptTokens: usage.PromptTokens, EvalTokens: usage.EvalTokens,
				EvalMs: usage.EvalMs, TotalMs: usage.TotalMs,
			}, nil
		}

		// 并行仅改变执行方式，不改变结果顺序：tool 消息必须与调用一一对应
		parallel := cfg.ToolExecution == ToolParallel && len(toolCalls) > 1
		var aborted error
		outcomes := make([]callOutcome, len(toolCalls))
		if parallel {
			var emitMu sync.Mutex
			safeEmit := func(e Event) { emitMu.Lock(); defer emitMu.Unlock(); emit.Emit(e) }
			var wg sync.WaitGroup
			for i := range toolCalls {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					outcomes[i] = r.execCall(ctx, step, toolCalls[i], registry, cfg, toolTimeout, maxResultBytes, safeEmit)
				}(i)
			}
			wg.Wait()
			for _, out := range outcomes {
				if out.abort != nil {
					aborted = out.abort
				}
				messages = append(messages, out.toolMessage)
				citations = mergeCitations(citations, out.citations)
			}
			if aborted != nil {
				result := usage
				result.Messages = messages
				result.Steps = step
				return result, aborted
			}
		} else {
			for i := range toolCalls {
				out := r.execCall(ctx, step, toolCalls[i], registry, cfg, toolTimeout, maxResultBytes, emit)
				if out.abort != nil {
					result := usage
					result.Messages = messages
					result.Steps = step
					return result, out.abort
				}
				messages = append(messages, out.toolMessage)
				citations = mergeCitations(citations, out.citations)
			}
		}
		emit.Emit(Event{Type: EventStepDone, RunID: cfg.RunID, Step: step})
	}

	result := usage
	result.Messages = messages
	result.Steps = maxSteps
	result.FinishReason = "max_steps"
	result.Citations = citations
	return result, ErrMaxSteps
}

// callOutcome 一次工具调用的产出。abort 非 nil 表示需要终止本次运行
// （典型场景：审批等待被中断）。
type callOutcome struct {
	toolMessage message.Message
	citations   []tool.Citation
	abort       error
}

// execCall 执行单个工具调用：校验存在性、解析参数、执行策略、
// （可能）等待审批、带超时执行、产出事件与回执消息。
// 该方法不直接修改运行轨迹——消息由调用方按顺序写回。
func (r *Runner) execCall(ctx context.Context, step int, call tool.Call,
	registry map[string]tool.Tool, cfg Config, toolTimeout time.Duration,
	maxResultBytes int, emit Emitter) callOutcome {

	name := call.Function.Name
	target, ok := registry[name]
	if !ok {
		return r.errOutcome(ctx, call, "模型请求了未授权或不存在的工具", step, cfg, emit)
	}
	arguments, argErr := call.ParseArguments()
	if argErr != nil {
		return r.errOutcome(ctx, call, "工具参数不是合法 JSON: "+argErr.Error(), step, cfg, emit)
	}
	emit.Emit(Event{Type: EventToolProposed, RunID: cfg.RunID, Step: step,
		CallID: call.ID, ToolName: name, Arguments: arguments})

	switch cfg.policyFor(target) {
	case tool.PolicyDeny:
		return r.errOutcome(ctx, call, "工具被当前策略禁止", step, cfg, emit)
	case tool.PolicyConfirm:
		emit.Emit(Event{Type: EventApprovalRequired, RunID: cfg.RunID, Step: step,
			CallID: call.ID, ToolName: name, Arguments: arguments})
		approved := false
		if cfg.Approve != nil {
			decision, approvedErr := cfg.Approve.Request(ctx, ApprovalRequest{
				Step: step, CallID: call.ID, ToolName: name, Arguments: arguments,
			})
			if approvedErr != nil {
				out := r.errOutcome(ctx, call, "工具审批中断: "+approvedErr.Error(), step, cfg, emit)
				out.abort = approvedErr
				return out
			}
			approved = decision
		}
		approvedPtr := approved
		emit.Emit(Event{Type: EventApprovalRequired, RunID: cfg.RunID, Step: step,
			CallID: call.ID, ToolName: name, Approved: &approvedPtr})
		if !approved {
			return r.errOutcome(ctx, call, "用户未批准该工具调用", step, cfg, emit)
		}
	}

	emit.Emit(Event{Type: EventToolStarted, RunID: cfg.RunID, Step: step,
		CallID: call.ID, ToolName: name, Arguments: arguments})
	toolCtx, cancel := context.WithTimeout(ctx, toolTimeout)
	out, callErr := target.Execute(toolCtx, arguments)
	cancel()
	if callErr != nil {
		return r.errOutcome(ctx, call, callErr.Error(), step, cfg, emit)
	}
	text := truncateUTF8(out.Content, maxResultBytes)
	if text == "" {
		text = "工具已执行，但没有返回文本内容"
	}
	eventType := EventToolResult
	if out.IsError {
		eventType = EventToolError
	}
	emit.Emit(Event{Type: eventType, RunID: cfg.RunID, Step: step,
		CallID: call.ID, ToolName: name, Result: text, Citations: out.Citations})
	return callOutcome{
		toolMessage: message.Message{
			Role: message.RoleTool, ToolCallID: call.ID, ToolName: name,
			Content: text, Citations: out.Citations,
		},
		citations: out.Citations,
	}
}

// errOutcome 构造错误回执：原因写进 tool 消息交回模型，让模型有机会自我纠正。
func (r *Runner) errOutcome(ctx context.Context, call tool.Call, reason string,
	step int, cfg Config, emit Emitter) callOutcome {
	return callOutcome{toolMessage: message.Message{
		Role: message.RoleTool, ToolCallID: call.ID, ToolName: call.Function.Name,
		Content: "错误：" + reason,
	}}
}


// policyFor 判定工具的生效策略：显式配置优先，否则看工具是否自我声明为只读。
// 未声明只读的工具默认需要确认。
func (c Config) policyFor(t tool.Tool) tool.Policy {
	name := t.Definition().Name
	if policy, ok := c.Policies[name]; ok {
		switch policy {
		case tool.PolicyAuto, tool.PolicyConfirm, tool.PolicyDeny:
			return policy
		}
	}
	if ro, ok := t.(tool.ReadOnlyTool); ok && ro.IsReadOnly() {
		return tool.PolicyAuto
	}
	return tool.PolicyConfirm
}

// truncateUTF8 按字节截断，且不切断多字节字符。
// 直接按字节切会产生非法 UTF-8，进而让整个请求被模型服务拒绝。
func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "\n\n[工具结果过长，已截断]"
}

func mergeCitations(current, incoming []tool.Citation) []tool.Citation {
	seen := make(map[string]struct{}, len(current)+len(incoming))
	result := make([]tool.Citation, 0, len(current)+len(incoming))
	for _, citation := range append(append([]tool.Citation(nil), current...), incoming...) {
		if citation.URL == "" {
			continue
		}
		if _, exists := seen[citation.URL]; exists {
			continue
		}
		seen[citation.URL] = struct{}{}
		result = append(result, citation)
	}
	return result
}
