package runtime

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/tool"
)

// 本文件实现恢复对账（reconciliation）：
//
// 检查点在每条工具回执写入轨迹后保存（调用级），事件在每次发生时落库。
// 两者非原子——进程在"事件已落库、检查点未保存"之间崩溃时，事件库里
// 存在检查点未覆盖的已执行调用（孤儿结果）。Resume 时重建这些记录：
//
//   - 孤儿结果（tool_result/tool_error 有记录）→ 模型重新发起同一调用时
//     直接复用记录的结果，不重复执行——已确认完成的步骤不重做。
//   - 无结果记录（tool_started 后崩溃，副作用可能已发生）→ 按工具的
//     RetryPolicy 分级：RetrySafe 重执行；NeedsVerify/NoRetry 判定为
//     "不确定调用"，在本轮运行内持续阻断（不只挡一次），重复请求一律
//     返回处置指引，直到人工显式处置——否则模型重试即可绕过。
//
// 这是"checkpointing ≠ durable execution"差距的务实补丁：库无法对外部
// 副作用提供恰好一次保证，但把不确定窗口收缩到单个调用，并让不确定的
// 那一次显式暴露给模型与用户，而不是静默重放。
//
// 协议安全边界：调用级检查点可能停在步中途（assistant 携带多个调用、
// 仅部分回执）。悬空的 tool_calls 会被部分模型服务拒绝，因此 Resume
// 先用 trimDangling 把轨迹回退到最后一个完整边界，被丢弃段中已完成的
// 调用由本索引复用结果。

// recordedCall 事件库中一条已执行调用的记录。
type recordedCall struct {
	message message.Message
	isError bool
}

// resumeIndex 从事件库重建的"检查点之后发生了什么"索引。
// results/inflight/blocked 均按"工具名+参数"键组织；恢复期间可能被
// 并行工具执行并发命中，全部访问在 mu 内完成。
type resumeIndex struct {
	mu sync.Mutex
	// results 已执行且有结果记录、未被检查点覆盖的调用（孤儿结果），
	// 按到达顺序排队消费。
	results map[string][]recordedCall
	// inflight 已开始执行但没有结果记录的调用次数（副作用可能已发生）。
	inflight map[string]int
	// blocked 已判定的不确定调用：本轮运行内持续阻断，重复请求不再执行。
	blocked map[string]bool
}

func (idx *resumeIndex) empty() bool {
	if idx == nil {
		return true
	}
	for _, queue := range idx.results {
		if len(queue) > 0 {
			return false
		}
	}
	for _, n := range idx.inflight {
		if n > 0 {
			return false
		}
	}
	for _, blocked := range idx.blocked {
		if blocked {
			return false
		}
	}
	return true
}

// callKey 生成"工具名+参数"的稳定匹配键。
// json.Marshal 对 map 按键排序，同一参数集合（含嵌套）生成相同键。
func callKey(name string, args map[string]any) string {
	if len(args) == 0 {
		return name + "\x00{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		// 参数不可序列化（理论上不会发生，来源是 JSON 反序列化的 map）：
		// 退化为仅按名字匹配，宁可多复用也不重复执行副作用。
		return name + "\x00?"
	}
	return name + "\x00" + string(b)
}

// buildResumeIndex 扫描事件库，索引"已执行但未被检查点覆盖"的调用。
// checkpointMessages 是回退到协议安全边界后的消息轨迹（见 trimDangling），
// 其中 tool 消息的 ToolCallID 集合即"检查点已覆盖"的判定标准。
func (rt *Runtime) buildResumeIndex(ctx context.Context, runID string,
	checkpointMessages []message.Message) (*resumeIndex, error) {

	if rt.events == nil {
		return nil, nil
	}
	events, err := rt.events.List(ctx, runID)
	if err != nil {
		return nil, err
	}

	covered := make(map[string]bool)
	for _, m := range checkpointMessages {
		if m.Role == message.RoleTool && m.ToolCallID != "" {
			covered[m.ToolCallID] = true
		}
	}

	// 第一遍：每个调用的参数（来自 tool_proposed）、是否真正开始执行
	// （tool_started）与是否有结果记录。注意 tool_error 不一定意味着
	// 执行过——未注册/参数非法/被拒绝/审批中断同样产生错误回执；
	// 只有开始执行过的调用，其结果才可作为"已完成"复用。
	arguments := make(map[string]map[string]any)
	started := make(map[string]bool)
	hasResult := make(map[string]bool)
	for _, e := range events {
		switch e.Type {
		case agent.EventToolProposed:
			if e.CallID != "" {
				arguments[e.CallID] = e.Arguments
			}
		case agent.EventToolStarted:
			started[e.CallID] = true
		case agent.EventToolResult, agent.EventToolError:
			hasResult[e.CallID] = true
		}
	}

	// 第二遍：未被检查点覆盖的调用，按有无结果分流。
	idx := &resumeIndex{
		results:  make(map[string][]recordedCall),
		inflight: make(map[string]int),
		blocked:  make(map[string]bool),
	}
	for _, e := range events {
		if e.CallID == "" || covered[e.CallID] {
			continue
		}
		switch e.Type {
		case agent.EventToolResult, agent.EventToolError:
			// 未开始执行就产生回执（拒绝/中断/参数错误）不是执行结果：
			// 恢复时按未发生处理，让模型重新发起并走正常策略（含重新审批）。
			if !started[e.CallID] {
				continue
			}
			content := e.Result
			if content == "" && e.Error != "" {
				// errOutcome 类事件（未注册/参数非法/被拒绝/审批中断）：
				// 原始回执内容即"error: 原因"。
				content = "error: " + e.Error
			}
			idx.results[callKey(e.ToolName, arguments[e.CallID])] = append(
				idx.results[callKey(e.ToolName, arguments[e.CallID])],
				recordedCall{
					message: message.Message{
						Role: message.RoleTool, ToolCallID: e.CallID,
						ToolName: e.ToolName, Content: content, Citations: e.Citations,
					},
					isError: e.Type == agent.EventToolError && e.Result != "",
				})
		case agent.EventToolStarted:
			if !hasResult[e.CallID] {
				idx.inflight[callKey(e.ToolName, arguments[e.CallID])]++
			}
		}
	}
	return idx, nil
}

// trimDangling 把消息轨迹回退到最后一个协议安全边界：若末尾的
// assistant(tool_calls) 存在未回执的调用（调用级检查点在步中途保存的
// 痕迹），则连同其后的 tool 回执一并丢弃。直接把悬空 tool_calls 发给
// 模型会被部分服务端拒绝；被丢弃段中已完成的调用由 Resume 事件对账
// 复用结果，不会重复执行。
func trimDangling(messages []message.Message) []message.Message {
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].Role {
		case message.RoleTool:
			continue // 末尾的 tool 回执属于最后一个 assistant 段
		case message.RoleAssistant:
			if len(messages[i].ToolCalls) == 0 {
				return messages // 纯文本 assistant，安全
			}
			answered := make(map[string]bool)
			for j := i + 1; j < len(messages) && messages[j].Role == message.RoleTool; j++ {
				answered[messages[j].ToolCallID] = true
			}
			for _, call := range messages[i].ToolCalls {
				if !answered[call.ID] {
					return messages[:i] // 存在未回执调用：丢弃整个悬空段
				}
			}
			return messages // 全部回执齐全，安全
		default:
			return messages // user/system 等结尾，安全
		}
	}
	return messages
}

// recoveryNotice 不确定调用的处置指引（IsError 结果，回传模型）。
func recoveryNotice(policy tool.RetryPolicy) tool.Result {
	if policy == tool.RetryPolicyNoRetry {
		return tool.Error("Recovery notice: this call was started before the previous interruption " +
			"but produced no recorded result, and the tool declares NoRetry (possibly non-idempotent " +
			"side effects). It will not be re-executed in this run. Verify the external state with a " +
			"read-only tool, report to the user, and let a human decide whether to redo the operation.")
	}
	return tool.Error("Recovery notice: this call was started before the previous interruption but " +
		"produced no recorded result. It will not be re-executed automatically in this run. Verify the " +
		"external state with read-only tools (e.g. whether the operation already took effect) and report; " +
		"only explicit human action can redo it.")
}

// resumeTool 恢复期间对工具的透明包装：命中已记录结果时复用，
// 碰上无结果记录的"疑似已执行"调用时按 RetryPolicy 分级。
// 恢复完成后（新调用不再命中索引）行为与原工具完全一致。
type resumeTool struct {
	tool.Tool
	idx *resumeIndex
}

var (
	_ tool.Tool         = (*resumeTool)(nil)
	_ tool.ReadOnlyTool = (*resumeTool)(nil)
	_ tool.Sourced      = (*resumeTool)(nil)
)

func (t *resumeTool) Execute(ctx context.Context, arguments map[string]any) (tool.Result, error) {
	key := callKey(t.Definition().Name, arguments)
	// 命中判定全部在锁内完成（并行工具会并发命中同一索引）；
	// 真正的执行在锁外进行——工具可能长时间阻塞。
	t.idx.mu.Lock()
	if queue := t.idx.results[key]; len(queue) > 0 {
		recorded := queue[0]
		t.idx.results[key] = queue[1:]
		t.idx.mu.Unlock()
		return tool.Result{
			Content:   recorded.message.Content,
			IsError:   recorded.isError,
			Citations: recorded.message.Citations,
		}, nil
	}
	blocked := t.idx.blocked[key]
	if !blocked && t.idx.inflight[key] > 0 {
		t.idx.inflight[key]--
		if tool.PolicyOf(t.Tool) != tool.RetryPolicyRetrySafe {
			// 首次命中不确定调用：登记阻断。后续相同调用（同键）一律拦截，
			// 不因计数耗尽而放行——模型重试不能替代人工处置。
			t.idx.blocked[key] = true
			blocked = true
		}
	}
	t.idx.mu.Unlock()
	if blocked {
		return recoveryNotice(tool.PolicyOf(t.Tool)), nil
	}
	return t.Tool.Execute(ctx, arguments)
}

// IsReadOnly / SourceID / SourceName 转发内层工具的可选能力；
// 内层未实现时返回零值（策略判定会走默认需确认的保守方向）。
func (t *resumeTool) IsReadOnly() bool {
	if ro, ok := t.Tool.(tool.ReadOnlyTool); ok {
		return ro.IsReadOnly()
	}
	return false
}

func (t *resumeTool) SourceID() string {
	if s, ok := t.Tool.(tool.Sourced); ok {
		return s.SourceID()
	}
	return ""
}

func (t *resumeTool) SourceName() string {
	if s, ok := t.Tool.(tool.Sourced); ok {
		return s.SourceName()
	}
	return ""
}
