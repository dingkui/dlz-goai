# 使用手册：llm — 模型调用

`llm` 定义与厂商无关的模型能力接口，`provider/openai` 与 `provider/ollama` 提供两个内置实现，`factory` 按配置构造。本篇覆盖：如何调用模型、两种实现的差异、多服务配置管理。

## Provider 接口

```go
type Provider interface {
	// 流式对话。delta 为空且 Done 表示结束；delta.Error 表示中断。
	ChatStream(ctx context.Context, model string, messages []message.Message,
		opts *message.Options, cb func(message.Delta)) error
	// 拉取服务可用模型名。
	ListModels(ctx context.Context) ([]string, error)
	// 探测连通性（向聊天端点发一个最小请求）。
	Ping(ctx context.Context) error
}
```

`model` 参数由调用方显式指定，实现不会自行猜测；model 为空直接报错。

## 两个内置实现

### provider/ollama — Ollama 原生接口

```go
provider := ollama.New("")            // 默认 http://127.0.0.1:11434
provider := ollama.New("http://gpu-1:11434") // 远程服务
```

为什么单独实现而不复用 OpenAI 兼容端点：原生接口能拿到 **token 用量与生成耗时**（delta 的 `PromptTokens`/`EvalTokens`/`EvalMs`/`TotalMs`），且 Ollama 的工具参数格式不同（对象而非字符串）。

### provider/openai — 任何 OpenAI 兼容接口

```go
provider := openai.New("https://api.deepseek.com", "sk-...")
provider := openai.New("https://api.openai.com/v1", "sk-...")
provider := openai.New("http://localhost:8080", "") // 本地 vLLM，无鉴权
```

baseURL 支持三种写法，自动适配：

| 写法 | 请求端点 |
|---|---|
| 以 `/chat/completions` 结尾 | 直接使用 |
| 以 `/v1` 结尾 | 追加 `/chat/completions` |
| 其他（根路径） | 依次尝试 `/v1/chat/completions` 与 `/chat/completions`，首个成功的生效 |

APIKey 为空时不发送 `Authorization` 头。

## factory — 按配置构造

应用把服务配置存在数据库/文件里、运行时切换服务商时，用 factory 避免硬编码：

```go
svc := llm.Service{
	ID: "deepseek", Name: "DeepSeek",
	Kind:         llm.KindOpenAI,        // 或 llm.KindOllama
	BaseURL:      "https://api.deepseek.com",
	APIKey:       "sk-...",
	Models:       []string{"deepseek-chat"},
	DefaultModel: "deepseek-chat",
	Enabled:      true,
}
provider := factory.NewProvider(svc)
```

## message.Options — 推理参数

| 字段 | 说明 | openai | ollama |
|---|---|---|---|
| `System` | 系统提示词，自动插入为首条 system 消息 | ✓ | ✓ |
| `Temperature *float64` | 采样温度 | ✓ `temperature` | ✓ `options.temperature` |
| `MaxTokens int` | 单次生成上限，0 交给服务端默认 | ✓ `max_tokens` | ✓ `options.num_predict` |
| `NumCtx *int` | 上下文窗口长度（Ollama 专属概念） | — | ✓ `options.num_ctx` |
| `Tools` / `ToolChoice` | 工具定义与选择策略；由 agent 自动填写，一般不手填 | ✓ | ✓ |

## 流式与用量统计

`ChatStream` 通过回调逐帧交付 `message.Delta`：

```go
err := provider.ChatStream(ctx, "qwen3:8b", msgs, &message.Options{System: "你是一个助手"},
	func(d message.Delta) {
		if d.Error != "" { /* 流内错误（如服务端中途失败） */ }
		if d.Content != "" { /* 正文增量 */ }
		if len(d.ToolCalls) > 0 { /* 工具调用分片，agent 负责聚合 */ }
		if d.Done { /* 结束帧 */ }
	})
```

用量差异：**Ollama 在结束帧提供完整用量**（prompt/eval tokens 与耗时）；OpenAI 兼容服务当前实现通常返回 0（stream_options 的 usage 透传未实现）——需要精确计费时以服务端账单为准。

## 多服务配置：llm.Store

`Store` 把服务配置持久化到 JSON 文件，适合桌面应用或需要用户自配服务的场景：

```go
store := llm.NewStore("llm.json")
_ = store.Load() // 文件不存在时返回空配置，不报错

// 原子替换（内部深拷贝，改入参不影响已存配置）
store.Replace([]llm.Service{svc}, "deepseek")
_ = store.Save()

svc, ok := store.Get("deepseek")      // 按 ID 查
enabled := store.EnabledList()        // 仅启用的服务
models := store.AllModels()           // 全部启用服务的模型名（去重、含默认模型）
services, def := store.Snapshot()     // 完整副本（可安全修改）
```

服务配置由应用负责装配：注册哪些服务、前端能选哪些，是应用层的安全边界（前端只传名称，不传 BaseURL/APIKey）。

## 连通性与预检

```go
if err := provider.Ping(ctx); err != nil { /* 服务不可达 */ }
models, err := provider.ListModels(ctx) // 校验模型名是否可用
```

启动时做一次预检可以把"配置错误"暴露在部署阶段而不是第一次对话时。

## 自定义实现

实现公开 llm.Provider 接口，详见 [自定义 Provider](../扩展手册/自定义Provider.md)。外部项目不能导入 internal/wire；该包仅供库内开发。
