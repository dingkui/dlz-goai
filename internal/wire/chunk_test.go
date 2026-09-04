package wire

import (
	"strings"
	"testing"

	"github.com/dingkui/dlz-goai/message"
)

// Ollama NDJSON：tool_calls 在 message 上，arguments 是对象（非字符串），
// 结束帧带 done 与用量统计。
func TestScanStreamOllamaNDJSON(t *testing.T) {
	payload := strings.Join([]string{
		`{"message":{"content":"你好"},"done":false}`,
		`{"message":{"tool_calls":[{"function":{"name":"search","arguments":{"q":"go"}}}]},"done":false}`,
		`{"message":{"content":""},"done":true,"prompt_eval_count":12,"eval_count":34,"eval_duration":2000000,"total_duration":900000000}`,
		`{"message":{"content":"不应到达"},"done":false}`,
	}, "\n")

	var deltas []message.Delta
	if err := ScanStream(strings.NewReader(payload), func(d message.Delta) { deltas = append(deltas, d) }); err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 3 {
		t.Fatalf("done 帧后应停止解析, got %d 条", len(deltas))
	}
	if deltas[0].Content != "你好" {
		t.Fatalf("首帧文本不符: %+v", deltas[0])
	}
	if len(deltas[1].ToolCalls) != 1 || deltas[1].ToolCalls[0].Name != "search" {
		t.Fatalf("工具调用帧不符: %+v", deltas[1].ToolCalls)
	}
	if args := deltas[1].ToolCalls[0].Arguments; args != `{"q":"go"}` {
		t.Fatalf("对象参数应原样保留为 JSON: %q", args)
	}
	last := deltas[2]
	if !last.Done || last.PromptTokens != 12 || last.EvalTokens != 34 || last.EvalMs != 2 {
		t.Fatalf("结束帧用量不符: %+v", last)
	}
}

// OpenAI SSE：data: 前缀、choices.delta 分片、[DONE] 收尾。
func TestScanStreamOpenAISSE(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"你"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"好"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		`data: {"choices":[{"delta":{"content":"不应到达"}}]}`,
		``,
	}, "\n")

	var deltas []message.Delta
	if err := ScanStream(strings.NewReader(payload), func(d message.Delta) { deltas = append(deltas, d) }); err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 4 {
		t.Fatalf("[DONE] 后应停止解析, got %d 条", len(deltas))
	}
	if deltas[0].Content != "你" || deltas[1].Content != "好" {
		t.Fatalf("SSE 文本分片不符: %+v", deltas)
	}
	if deltas[2].FinishReason != "stop" {
		t.Fatalf("finish_reason 丢失: %+v", deltas[2])
	}
	if !deltas[3].Done {
		t.Fatalf("[DONE] 应产生结束帧: %+v", deltas[3])
	}
}

// 工具参数分片传输：多个 delta 的 arguments 需要调用方按 Index 拼接。
func TestScanStreamOpenAISToolCallShards(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"sea","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")

	var merged []message.Delta
	if err := ScanStream(strings.NewReader(payload), func(d message.Delta) { merged = append(merged, d) }); err != nil {
		t.Fatal(err)
	}
	if len(merged[0].ToolCalls) == 0 || merged[0].ToolCalls[0].ID != "c1" {
		t.Fatalf("首个分片应携带 id: %+v", merged[0])
	}
	joined := merged[1].ToolCalls[0].Arguments + merged[2].ToolCalls[0].Arguments
	if joined != `{"q":"go"}` {
		t.Fatalf("参数分片拼接不符: %q", joined)
	}
}

// 服务端夹杂的心跳注释与空行应被静默跳过。
func TestScanStreamToleratesNoise(t *testing.T) {
	payload := strings.Join([]string{
		`: keep-alive`,
		``,
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		`data: [DONE]`,
	}, "\n")

	var deltas []message.Delta
	if err := ScanStream(strings.NewReader(payload), func(d message.Delta) { deltas = append(deltas, d) }); err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 2 || deltas[0].Content != "ok" {
		t.Fatalf("噪声行应被跳过: %+v", deltas)
	}
}

// 错误帧透传：Ollama 的 {"error": "..."}。流随后自然结束，EOF 补一个结束帧。
func TestScanStreamErrorFrame(t *testing.T) {
	payload := `{"error":"model not found"}`
	var deltas []message.Delta
	if err := ScanStream(strings.NewReader(payload), func(d message.Delta) { deltas = append(deltas, d) }); err != nil {
		t.Fatal(err)
	}
	if len(deltas) < 1 || deltas[0].Error != "model not found" {
		t.Fatalf("错误帧应透传: %+v", deltas)
	}
}
