package runtime

import (
	"context"
	"encoding/json"

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
//     直接复用记录的结果，不重复执行——"已确认完成的步骤不重做"。
//   - 无结果记录（tool_started 后崩溃，副作用可能已发生）→ 按工具的
//     RetryPolicy 分级处理：RetrySafe 重执行，NeedsVerify/NoRetry 返回
//     提示让模型先核实外部状态，不盲目重试。
//
// 这是"checkpointing ≠ durable execution"差距的务实补丁：库无法对外部
// 副作用提供恰好一次保证，但把不确定窗口收缩到单个调用，并让不确定的
// 那一次显式暴露给模型与用户，而不是静默重放。

// recordedCall 事件库中一条已执行调用的记录。
type recordedCall struct {
	message message.Message
	isError bool
}

// resumeIndex 从事件库重建的"检查点之后发生了什么"索引。
// results/inflight 均按"工具名+参数"键排队，消费一次出队一次。
type resumeIndex struct {
	results  map[string][]recordedCall
	inflight map[string]int
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
// checkpointMessages 是检查点里的消息轨迹，其中 tool 消息的 ToolCallID
// 集合即"检查点已覆盖"的判定标准。
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

	// 第一遍：每个调用的参数（来自 tool_proposed）与是否有结果记录。
	arguments := make(map[string]map[string]any)
	hasResult := make(map[string]bool)
	for _, e := range events {
		switch e.Type {
		case agent.EventToolProposed:
			if e.CallID != "" {
				arguments[e.CallID] = e.Arguments
			}
		case agent.EventToolResult, agent.EventToolError:
			hasResult[e.CallID] = true
		}
	}

	// 第二遍：未被检查点覆盖的调用，按有无结果分流。
	idx := &resumeIndex{
		results:  make(map[string][]recordedCall),
		inflight: make(map[string]int),
	}
	for _, e := range events {
		if e.CallID == "" || covered[e.CallID] {
			continue
		}
		switch e.Type {
		case agent.EventToolResult, agent.EventToolError:
			content := e.Result
			if content == "" && e.Error != "" {
				// errOutcome 类事件（未注册/参数非法/被拒绝/审批中断）：
				// 原始回执内容即"错误：原因"。
				content = "错误：" + e.Error
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
	if queue := t.idx.results[key]; len(queue) > 0 {
		recorded := queue[0]
		t.idx.results[key] = queue[1:]
		return tool.Result{
			Content:   recorded.message.Content,
			IsError:   recorded.isError,
			Citations: recorded.message.Citations,
		}, nil
	}
	if t.idx.inflight[key] > 0 {
		t.idx.inflight[key]--
		switch tool.PolicyOf(t.Tool) {
		case tool.RetryPolicyNoRetry:
			return tool.Error("恢复提示：该调用在上次运行中断前已开始执行且没有结果记录，" +
				"工具声明为 NoRetry（可能已产生不可重复的副作用），未自动重试。" +
				"请先用只读工具核实外部状态，或转人工处理。"), nil
		case tool.RetryPolicyNeedsVerify:
			return tool.Error("恢复提示：该调用在上次运行中断前已开始执行且没有结果记录，" +
				"未直接重试。请先核实外部状态（如查询操作是否已生效），确认后再重新发起。"), nil
		}
		// RetrySafe：正常重新执行。
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
