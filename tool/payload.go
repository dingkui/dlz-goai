package tool

// Payload 把工具定义转换为 OpenAI 兼容与 Ollama 都接受的函数工具载荷。
// parameters 缺失时补一个空对象 schema，部分模型服务不接受空缺。
func Payload(definitions []Definition) []map[string]any {
	tools := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		parameters := definition.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        definition.Name,
				"description": definition.Description,
				"parameters":  parameters,
			},
		})
	}
	return tools
}
