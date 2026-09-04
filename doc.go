// Package dlzgoai 是 dlz-goai 的根文档包，本身不含代码。
//
// dlz-goai 是一个零第三方依赖的 Go 智能体基础库，提供三块能力：
//
//   - tool / message：与厂商无关的数据契约（工具定义、对话消息），零依赖
//   - llm / provider：统一的模型流式调用抽象，内置 OpenAI 兼容与 Ollama 两种实现
//   - agent：受控的工具调用循环，带步骤上限、超时、结果截断、工具审批
//   - mcp：Model Context Protocol 客户端（stdio 与 Streamable HTTP），可独立使用
//
// 依赖方向严格单向，agent 不依赖任何模型厂商，mcp 不依赖任何其他包：
//
//	tool     ← 零依赖
//	message  ← tool
//	llm      ← message, tool
//	provider/openai, provider/ollama ← llm, message, tool
//	agent    ← message, tool
//	mcp      ← 零依赖
//
// 本库不提供：向量检索 / RAG / 图编排 / 多智能体。
// 它是一个工具调用运行时，不是一个智能体框架。
package dlzgoai
