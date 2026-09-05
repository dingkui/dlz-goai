package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dingkui/dlz-goai/message"
)

// RequestTimeout 非流式请求的默认超时。流式请求不设超时，
// 由调用方的 context 控制。
const RequestTimeout = 30 * time.Second

// Codec 描述一个模型服务与标准协议之间的差异。
// provider/openai 与 provider/ollama 各实现一份，共享本包的 HTTP 流程。
type Codec interface {
	// ChatURLs 返回候选聊天端点，按优先级依次尝试。
	ChatURLs(baseURL string) []string
	// ModelsURL 返回模型清单端点。
	ModelsURL(baseURL string) string
	// EncodeMessages 把统一消息编码为厂商载荷。
	EncodeMessages(messages []message.Message) []map[string]any
	// ApplyOptions 写入厂商特有的推理参数（温度、上下文长度等）。
	ApplyOptions(body map[string]any, opts *message.Options)
	// DecodeModels 解析模型清单响应体。
	DecodeModels(r io.Reader) ([]string, error)
	// Ollama 为 true 时表示面向本地 Ollama（影响工具字段与 tool_choice）。
	Ollama() bool
}

// Client 封装与单个模型服务的 HTTP 通信。
type Client struct {
	BaseURL    string
	APIKey     string
	Codec      Codec
	HTTP       *http.Client // 可选；nil 时使用默认传输并加 RequestTimeout
	StreamHTTP *http.Client // 可选；nil 时使用默认传输且不设超时
}

// NewClient 构造客户端。apiKey 为空时不发送 Authorization 头（本地 Ollama 即如此）。
func NewClient(baseURL, apiKey string, codec Codec) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Codec: codec}
}

func (c *Client) requestClient(stream bool) *http.Client {
	if stream {
		if c.StreamHTTP != nil {
			return c.StreamHTTP
		}
		if c.HTTP != nil {
			clone := *c.HTTP
			clone.Timeout = 0
			return &clone
		}
		return &http.Client{Transport: Transport()}
	}
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Transport: Transport(), Timeout: RequestTimeout}
}

// Transport 返回调过参数的 HTTP 传输：连接 10s、TLS 10s、响应头 2min。
func Transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 2 * time.Minute
	transport.ExpectContinueTimeout = time.Second
	transport.IdleConnTimeout = 90 * time.Second
	return transport
}

// ChatStream 流式对话。model 为空直接报错——交给模型猜不是好行为。
func (c *Client) ChatStream(ctx context.Context, model string, messages []message.Message,
	opts *message.Options, cb func(message.Delta)) error {
	if model == "" {
		return fmt.Errorf("no model specified")
	}
	finalMessages := messages
	if opts != nil && opts.System != "" {
		finalMessages = make([]message.Message, 0, len(messages)+1)
		finalMessages = append(finalMessages, message.Message{Role: message.RoleSystem, Content: opts.System})
		finalMessages = append(finalMessages, messages...)
	}
	body := map[string]any{
		"model":    model,
		"messages": c.Codec.EncodeMessages(finalMessages),
		"stream":   true,
	}
	ApplyToolOptions(body, opts, c.Codec.Ollama())
	if opts != nil {
		c.Codec.ApplyOptions(body, opts)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}

	var lastErr error
	for _, url := range c.Codec.ChatURLs(c.BaseURL) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		resp, err := c.requestClient(true).Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("chat: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
			continue
		}
		err = ScanStream(resp.Body, cb)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable endpoint")
	}
	return lastErr
}

// ListModels 拉取服务端可用模型名。
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Codec.ModelsURL(c.BaseURL), nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.requestClient(false).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list models: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return c.Codec.DecodeModels(resp.Body)
}

// Ping 探测连通性：向聊天端点发一个最小请求，只要收到任何内容即视为可用。
func (c *Client) Ping(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	done := false
	err := c.ChatStream(probeCtx, "__probe__", []message.Message{
		{Role: message.RoleUser, Content: "ping"},
	}, &message.Options{}, func(d message.Delta) {
		if d.Error != "" {
			return
		}
		done = true
	})
	if err != nil {
		return err
	}
	if !done {
		return fmt.Errorf("endpoint returned no content")
	}
	return nil
}
