package wire

import (
	"encoding/json"

	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/tool"
)

// ToolPayload 把通用工具定义转换为 OpenAI / Ollama 都接受的函数工具格式。
func ToolPayload(definitions []tool.Definition) []map[string]any {
	return tool.Payload(definitions)
}

// ApplyToolOptions 向请求体注入工具定义。
// ollama 为 true 时省略 tool_choice——本地 Ollama 不接受该字段，
// 传了会直接返回 400。
func ApplyToolOptions(body map[string]any, opts *message.Options, ollama bool) {
	if opts == nil || len(opts.Tools) == 0 {
		return
	}
	body["tools"] = ToolPayload(opts.Tools)
	if !ollama && opts.ToolChoice != "" {
		body["tool_choice"] = opts.ToolChoice
	}
}

// ApplyMessageToolFields 写入一条消息的工具相关字段。
// 差异点：Ollama 用 tool_name 标识回传的工具，OpenAI 兼容用 tool_call_id。
func ApplyMessageToolFields(item map[string]any, m message.Message, ollama bool) {
	if len(m.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(m.ToolCalls))
		for _, call := range m.ToolCalls {
			callType := call.Type
			if callType == "" {
				callType = "function"
			}
			arguments := any(string(call.Function.Arguments))
			if ollama {
				var decoded any
				if len(call.Function.Arguments) == 0 || json.Unmarshal(call.Function.Arguments, &decoded) != nil {
					decoded = map[string]any{}
				}
				arguments = decoded
			}
			wire := map[string]any{
				"type": callType,
				"function": map[string]any{
					"name":      call.Function.Name,
					"arguments": arguments,
				},
			}
			if call.ID != "" {
				wire["id"] = call.ID
			}
			calls = append(calls, wire)
		}
		item["tool_calls"] = calls
	}
	if m.Role == message.RoleTool {
		if ollama {
			item["tool_name"] = m.ToolName
		} else {
			item["tool_call_id"] = m.ToolCallID
			if m.ToolName != "" {
				item["name"] = m.ToolName
			}
		}
	}
}
