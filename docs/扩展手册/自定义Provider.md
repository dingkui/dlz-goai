# 扩展手册：自定义 Provider

内置的 `provider/openai` 与 `provider/ollama` 覆盖不了的服务（自建网关、私有 SDK、非 HTTP 协议），可实现公开的 llm.Provider 接口。

## 实现公开 llm.Provider 接口

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
- Ping 仅检查连通性，不代表验证了模型工具调用能力。

gRPC 适配思路片段，ChatClient、协议转换和另外两个方法由应用实现：

```go
type grpcProvider struct{ client ChatClient } // 应用自己的客户端接口

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

**可传入 Client.Options.Model 或 Agent.Run 的 model 参数**——这也是测试桩与生产实现可以无缝互换的原因。

内部 wire.Codec 仅供库内开发，外部项目不能导入 internal/wire，锁版本也不能解除限制。见 [贡献指南](../贡献指南.md)。

## 验证清单

自定义 Provider 完成后逐项核对：

- [ ] 流式正文增量顺序正确，结束帧发出（或流关闭时收到 EOF）；
- [ ] 工具调用分片透传 `Index/ID/Name/Arguments`，多片聚合后是合法 JSON；
- [ ] 流内错误不会静默（delta.Error 或 return error 二选一）；
- [ ] ctx 取消能立即中断（http request / grpc stream 都要挂 ctx）；
- [ ] `opts.System` 生效（或明确不支持并在文档说明）；
- [ ] 用 `runtime` 的调用桩替换后行为一致（`go test ./runtime/` 的桩就是现成参照）。
