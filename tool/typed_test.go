package tool

import (
	"context"
	"strings"
	"testing"
)

type searchInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
	Token *int   `json:"token,omitempty"`
}

// 非指针且未 omitempty 的字段进 required；指针与 omitempty 不进。
func TestSchemaForStruct(t *testing.T) {
	schema := SchemaFor(searchInput{})
	if schema["type"] != "object" {
		t.Fatalf("顶层应为 object: %+v", schema)
	}
	props := schema["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Fatalf("缺 query 属性: %+v", props)
	}
	if props["limit"].(map[string]any)["type"] != "integer" {
		t.Fatalf("limit 应为 integer: %+v", props["limit"])
	}
	required := schema["required"].([]string)
	if len(required) != 1 || required[0] != "query" {
		t.Fatalf("required 应只有 query, got %v", required)
	}
}

// Typed 端到端：schema 生成 + 参数绑定 + 执行。
func TestTypedToolEndToEnd(t *testing.T) {
	var got searchInput
	tt := Typed("search", "搜索", true,
		func(ctx context.Context, in searchInput) (Result, error) {
			got = in
			return Text("found:" + in.Query), nil
		})

	def := tt.Definition()
	if def.Name != "search" || def.Description != "搜索" {
		t.Fatalf("定义不符: %+v", def)
	}
	if !tt.(ReadOnlyTool).IsReadOnly() {
		t.Fatal("readOnly 未透传")
	}

	out, err := tt.Execute(context.Background(), map[string]any{"query": "go", "limit": float64(5)})
	if err != nil || out.Content != "found:go" {
		t.Fatalf("执行结果不符: %+v err=%v", out, err)
	}
	if got.Query != "go" || got.Limit != 5 {
		t.Fatalf("绑定结果不符: %+v", got)
	}
}

// 参数与 schema 不符时返回 IsError 结果（回传模型自我纠正），而非 error。
func TestTypedToolInvalidArgs(t *testing.T) {
	tt := Typed("search", "搜索", true,
		func(ctx context.Context, in searchInput) (Result, error) {
			t.Error("参数非法时不应调用 handler")
			return Text(""), nil
		})
	out, err := tt.Execute(context.Background(), map[string]any{"query": 12345})
	if err != nil {
		t.Fatalf("参数错误不应作为执行错误: %v", err)
	}
	if !out.IsError || !strings.Contains(out.Content, "do not match the schema") {
		t.Fatalf("应返回业务错误结果: %+v", out)
	}
}
