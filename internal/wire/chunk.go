// Package wire 封装模型服务的线协议细节：流式帧解析、请求体组装、
// HTTP 传输参数。provider/openai 与 provider/ollama 共享本包，
// 各自只实现差异部分（Codec）。
//
// 本包是内部实现，不对外暴露，API 可能随版本变动。
package wire

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/tool"
)

// Chunk 同时兼容两种流式载荷：
//
//   - Ollama NDJSON：字段在 message 上，结束帧带 done 与用量统计
//   - OpenAI SSE：字段在 choices[0].delta 上，以 data: [DONE] 收尾
//
// 两种格式在同一个结构体里并存，缺字段自然为零值。
type Chunk struct {
	Message struct {
		Content   string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Done  bool   `json:"done"`
	Error string `json:"error"`

	// 用量字段。Ollama 在 done 帧提供，OpenAI 兼容服务通常缺失。
	PromptEvalCount    int   `json:"prompt_eval_count"`
	PromptEvalDuration int64 `json:"prompt_eval_duration"`
	EvalCount          int   `json:"eval_count"`
	EvalDuration       int64 `json:"eval_duration"`
	TotalDuration      int64 `json:"total_duration"`
}

// Delta 把一帧载荷转换为统一的增量。
func (c Chunk) Delta() message.Delta {
	d := message.Delta{
		Done:  c.Done,
		Error: c.Error,
	}
	toolCalls := make([]tool.CallDelta, 0, len(c.Message.ToolCalls))

	// Ollama：message.tool_calls，arguments 可能是对象也可能是字符串
	for index, call := range c.Message.ToolCalls {
		arguments := string(call.Function.Arguments)
		var encoded string
		if json.Unmarshal(call.Function.Arguments, &encoded) == nil {
			arguments = encoded
		}
		toolCalls = append(toolCalls, tool.CallDelta{
			Index: index, ID: call.ID, Name: call.Function.Name, Arguments: arguments,
		})
	}
	d.Content = c.Message.Content

	// OpenAI 兼容：choices[0].delta
	if len(c.Choices) > 0 {
		choice := c.Choices[0]
		if d.Content == "" {
			d.Content = choice.Delta.Content
		}
		d.FinishReason = choice.FinishReason
		for _, call := range choice.Delta.ToolCalls {
			toolCalls = append(toolCalls, tool.CallDelta{
				Index: call.Index, ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments,
			})
		}
	}
	d.ToolCalls = toolCalls

	d.PromptTokens = c.PromptEvalCount
	d.EvalTokens = c.EvalCount
	d.EvalMs = c.EvalDuration / int64(time.Millisecond)
	d.TotalMs = c.TotalDuration / int64(time.Millisecond)
	return d
}

// ScanStream 解析 NDJSON 或 SSE data: 两种流式格式，直到 done 帧或流结束。
// 无法解析的行静默跳过——个别服务端会夹杂心跳注释。
func ScanStream(r io.Reader, cb func(message.Delta)) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "data:") {
				t = strings.TrimSpace(strings.TrimPrefix(t, "data:"))
			}
			if t == "" {
				continue
			}
			if t == "[DONE]" {
				if cb != nil {
					cb(message.Delta{Done: true})
				}
				return nil
			}
			var obj Chunk
			if err := json.Unmarshal([]byte(t), &obj); err != nil {
				continue
			}
			if cb != nil {
				cb(obj.Delta())
			}
			if obj.Done {
				return nil
			}
		}
		if err != nil {
			if err == io.EOF {
				if cb != nil {
					cb(message.Delta{Done: true})
				}
				return nil
			}
			return err
		}
	}
}
