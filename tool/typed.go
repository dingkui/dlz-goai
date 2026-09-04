package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// Typed 用 Go 结构体定义工具输入，自动生成 JSON Schema 并完成参数绑定。
//
// 相比 NewFunc，调用方不再手写 schema、不再解析 map[string]any：
//
//	type searchInput struct {
//	    Query string `json:"query"`
//	    Limit int    `json:"limit,omitempty"`
//	}
//	tool.Typed("search", "搜索知识库", true,
//	    func(ctx context.Context, in searchInput) (tool.Result, error) {
//	        return tool.Text("..."), nil
//	    })
//
// 参数绑定失败时返回 IsError 结果而非 error——原因会回传给模型，
// 给它一次自我纠正的机会；这与执行失败的语义（error，终止步骤）不同。
func Typed[In any](name, description string, readOnly bool, handler func(context.Context, In) (Result, error)) Tool {
	var zero In
	parameters := SchemaFor(zero)
	return Func{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		ReadOnly:    readOnly,
		Handler: func(ctx context.Context, args map[string]any) (Result, error) {
			encoded, err := json.Marshal(args)
			if err != nil {
				return Error("工具参数无法编码: " + err.Error()), nil
			}
			var input In
			if err := json.Unmarshal(encoded, &input); err != nil {
				return Error(fmt.Sprintf("参数与定义不符: %v；schema: %s", err, encoded)), nil
			}
			return handler(ctx, input)
		},
	}
}
