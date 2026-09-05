# 扩展手册：自定义 Provider

内置的 `provider/openai` 与 `provider/ollama` 覆盖不了的服务（自建网关、私有 SDK、非 HTTP 协议），有两条接入路径。选哪条取决于目标服务的协议形态。

## 路径一：实现 llm.Provider（推荐起点）

三个方法，直接实现：

```go
type Provider interface {
	ChatStream(ctx context.Context, model string, messages []message.Message,
		opts *message.Options, cb func(message.Delta)) error
	ListModels(ctx context.Context) ([]string, error)
	Ping(ctx context.Context) error
}
```

契约要点（详见 `llm` 包注释）：

- `ChatStream` 逐帧回调 `message.Delta`：正文增量走 `Content`，工具调用分片走 `ToolCalls`（agent 层负责聚合，你只管透传 `tool.CallDelta{Index, ID, Name, Arguments}`），结束帧 `Done: true`；
- **流内错误**用 `delta.Error` 传递（agent 会把它并入轨迹保留已流出内容）；连接失败直接返回 error；
- `model` 由调用方显式传入，为空时返回明确错误——不要替调用方猜模型；
- `opts` 可能为 nil（零值语义 = 全部交给服务端默认）；
- `Ping` 的标准实现是发一个最小请求，收到任何内容增量即视为可用。

最小骨架（gRPC 服务的例子）：

```go
type grpcProvider struct{ endpoint string }

func (p *grpcProvider) ChatStream(ctx context.Context, model string,
	msgs []message.Message, opts *message.Options, cb func(message.Delta)) error {

	stream, err := p.client.Chat(ctx, toProto(model, msgs, opts))
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			cb(message.Delta{Done: true})
			return nil
		}
		if err != nil {
			return err
		}
		if resp.Text != "" {
			cb(message.Delta{Content: resp.Text})
		}
	}
}
```

适配进 agent 与内置 provider 完全同构：

```go
callModel := func(ctx context.Context, msgs []message.Message, opts *message.Options, cb func(message.Delta)) error {
	return p.ChatStream(ctx, "my-model", msgs, opts, cb)
}
```

**要求接受 `agent.Config` 的 `ModelFunc` 的地方都能用**——这也是测试桩与生产实现可以无缝互换的原因。

## 路径二：实现 wire.Codec（OpenAI 风格 SSE）

目标服务是 HTTP + SSE、只是细节与 OpenAI 不同（端点路径、载荷字段、参数名）时，不必重写 HTTP/SSE 流程——实现六个差异点，HTTP、重试、流解析、端点回退全部复用：

```go
type Codec interface {
	// 候选聊天端点，按优先级依次尝试（openai 实现用它做 /v1 回退）
	ChatURLs(baseURL string) []string
	// 模型清单端点
	ModelsURL(baseURL string) string
	// 统一消息 → 厂商载荷（含图片、工具字段）
	EncodeMessages(messages []message.Message) []map[string]any
	// 厂商特有推理参数写入请求体（温度、num_ctx 等）
	ApplyOptions(body map[string]any, opts *message.Options)
	// 解析模型清单响应体
	DecodeModels(r io.Reader) ([]string, error)
	// 是否面向本地 Ollama（影响工具字段格式与 tool_choice）
	Ollama() bool
}

provider := &openai.Provider{Client: wire.NewClient(baseURL, apiKey, myCodec{})}
```

**警告**：`internal/wire` 是内部包，不承诺兼容。升级版本时 Codec 实现可能需要适配——把你的 Codec 实现放在自己的仓库里并锁版本，或在 dlz-goai 仓库内贡献为官方 provider。

## 验证清单

自定义 Provider 完成后逐项核对：

- [ ] 流式正文增量顺序正确，结束帧发出（或流关闭时收到 EOF）；
- [ ] 工具调用分片透传 `Index/ID/Name/Arguments`，多片聚合后是合法 JSON；
- [ ] 流内错误不会静默（delta.Error 或 return error 二选一）；
- [ ] ctx 取消能立即中断（http request / grpc stream 都要挂 ctx）；
- [ ] `opts.System` 生效（或明确不支持并在文档说明）；
- [ ] 用 `runtime` 的调用桩替换后行为一致（`go test ./runtime/` 的桩就是现成参照）。
