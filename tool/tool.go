// Package tool 定义与模型厂商无关的工具契约：工具如何描述自己，
// 以及模型如何请求调用它。
//
// 本包零依赖，可被 message、llm、agent、mcp 任意组合引用而不产生环。
package tool

import "encoding/json"

// Definition 是提供给模型的函数工具定义（JSON Schema 风格的参数描述）。
type Definition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Call 是模型生成的完整工具调用。
type Call struct {
	ID       string   `json:"id,omitempty"`
	Type     string   `json:"type,omitempty"`
	Function Function `json:"function"`
}

// Function 保存工具名和原始 JSON 参数。
type Function struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// CallDelta 是流式响应中的工具调用片段；Arguments 可能需要按 Index 累积。
type CallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// ParseArguments 把完整工具参数解析成可交给执行器的 map。
// 空参数、字面量 null 都归一化为空 map，调用方无需再判空。
func (c Call) ParseArguments() (map[string]any, error) {
	if len(c.Function.Arguments) == 0 || string(c.Function.Arguments) == "null" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(c.Function.Arguments, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
