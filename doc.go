// Package dlzgoai 是 dlz-goai 的根文档包，并提供最小嵌入门面（client.go）。
//
// dlzgoai 是一个零第三方依赖的 Go 智能体基础库，提供六块能力：
//
//   - Client：最小嵌入门面——装配模型/工具/存储后，Start/Wait/Approve/
//     Resume/Stream 管理可恢复的 Agent 运行生命周期（见 client.go）
//   - tool / message：与厂商无关的数据契约（工具定义、对话消息），零依赖
//   - llm / provider：统一的模型流式调用抽象，内置 OpenAI 兼容与 Ollama 两种实现
//   - agent：受控的工具调用循环，带步骤上限、超时、结果截断、工具审批
//   - mcp：Model Context Protocol 客户端（stdio 与 Streamable HTTP），可独立使用
//   - runtime / rag / storage：持久化运行（检查点、断点续跑、事件回放）与
//     检索增强（分块、向量化、混合检索），SQLite 持久实现可选引入
//
// 依赖方向严格单向，agent 不依赖任何模型厂商，mcp 不依赖任何其他包：
//
//	tool     ← 零依赖
//	message  ← tool
//	llm      ← message, tool
//	provider/openai, provider/ollama ← llm, message, tool
//	agent    ← message, tool
//	mcp      ← 零依赖
//	runtime  ← agent, message, tool
//	rag      ← tool
//	Client（本包）← runtime + agent + ...
//
// 除 storage/sqlite（modernc.org/sqlite，纯 Go 无 CGO）外全部零第三方依赖；
// 不 import storage/sqlite 就不会引入该依赖。
//
// 本库不提供：图编排 / 多智能体 / 独立 HTTP 服务。
// 它是一个工具调用运行时，不是一个智能体框架。
package dlzgoai
