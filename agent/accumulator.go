package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dingkui/dlz-goai/tool"
)

// 流式响应把一次工具调用拆成许多片段：名字分段传、参数分片传、
// id 可能只在首个片段出现。累积器负责按 Index 还原成完整调用。
//
// 这是所有 OpenAI 兼容服务共有的行为，不是某个厂商的怪癖。
type accumulatedCall struct {
	id        string
	name      strings.Builder
	arguments strings.Builder
}

type callAccumulator struct {
	step  int
	calls map[int]*accumulatedCall
}

func newCallAccumulator(step int) *callAccumulator {
	return &callAccumulator{step: step, calls: map[int]*accumulatedCall{}}
}

// Add 追加增量片段。
func (a *callAccumulator) Add(deltas []tool.CallDelta) {
	for _, delta := range deltas {
		call := a.calls[delta.Index]
		if call == nil {
			call = &accumulatedCall{}
			a.calls[delta.Index] = call
		}
		if delta.ID != "" {
			call.id = delta.ID
		}
		if delta.Name != "" {
			call.name.WriteString(delta.Name)
		}
		if delta.Arguments != "" {
			call.arguments.WriteString(delta.Arguments)
		}
	}
}

// Build 还原完整调用列表，按 Index 排序保证顺序稳定。
//
// 两处容错：参数为空时补空对象（模型偶发只给名字不给参数）；
// 参数不是合法 JSON 时整段转义成字符串，避免后续解析直接失败——
// 让模型在下一轮看到自己输出了什么，比让它收到一句"参数错误"更有用。
func (a *callAccumulator) Build() []tool.Call {
	indexes := make([]int, 0, len(a.calls))
	for index := range a.calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	out := make([]tool.Call, 0, len(indexes))
	for _, index := range indexes {
		call := a.calls[index]
		arguments := strings.TrimSpace(call.arguments.String())
		if arguments == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			arguments = fmt.Sprintf("%q", arguments)
		}
		id := call.id
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", a.step, index)
		}
		out = append(out, tool.Call{
			ID:   id,
			Type: "function",
			Function: tool.Function{
				Name:      call.name.String(),
				Arguments: json.RawMessage(arguments),
			},
		})
	}
	return out
}
