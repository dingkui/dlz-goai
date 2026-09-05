# 使用手册：mcp — MCP 工具接入

`mcp` 包实现 **MCP Tool Client / Adapter**：连接 MCP 服务端（stdio 或 Streamable HTTP），发现工具，并适配成统一的 `tool.Tool` 喂给 agent。零依赖，可独立于库的其他部分使用。

> 范围声明：实现 `initialize / tools/list / tools/call`，定位是把 MCP 工具接入 agent——resources、prompts、sampling、OAuth 不在当前范围。协议版本 `2024-11-05`。

## 快速上手（stdio）

stdio 传输 = 启动一个子进程，通过 stdin/stdout 交换 JSON-RPC：

```go
manager := mcp.NewManager()
defer manager.Close()

manager.Apply([]mcp.Server{{
	ID: "files", Name: "文件系统", Enabled: true,
	Transport: mcp.TransportStdio,
	Command:   "npx",
	Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", "."},
}})

manager.EnsureReady(ctx) // 等首次工具发现完成，避免启动竞态导致工具列表为空

tools := manager.ToolsOf([]string{"files"}) // []tool.Tool——agent 无感知 MCP 的存在
result, _ := agent.New().Run(ctx, msgs, nil, agent.Config{Tools: tools}, callModel, nil)
```

完整示例：[`examples/mcp`](../examples/mcp/main.go)。

## 服务配置（Server）

```go
type Server struct {
	ID        string            // 命名空间标识：1-64 位字母/数字/下划线/连字符，禁连续 __
	Name      string            // 显示名（≤128 字符）
	Transport string            // "http"（缺省）或 "stdio"
	Enabled   bool
	// HTTP 传输：
	URL     string              // 必须是合法 http/https；禁止内嵌用户名密码（鉴权走 Headers）
	Headers map[string]string   // 自定义请求头（如 Authorization）；≤64 项，保留头不可覆盖
	// stdio 传输：
	Command string              // 启动命令（≤4096，禁 \x00）
	Args    []string
	Env     map[string]string   // 环境变量（名字需合法）
}
```

`Apply` 做全量校验后**增量重建**：配置未变的服务复用现有连接，变更/删除的退役重建。校验失败返回明确错误，不会静默忽略。

`mcp.Store` 把 `[]Server` 持久化到 JSON 文件（`Load`/`Save`/`Replace`），路径由调用方注入。

## Manager 生命周期

| 方法 | 用途 |
|---|---|
| `NewManager()` | 构造并启动后台刷新（每 5 分钟重拉 tools/list） |
| `Apply(servers)` | 应用最新配置（增量） |
| `EnsureReady(ctx)` | 等待首次工具发现完成（默认上限 15s）——启动路径建议调用 |
| `ToolsOf(serverIDs)` | 取指定服务的全部工具（适配为 `tool.Tool`） |
| `ToolsFor(serverIDs, allowList)` | 服务 + 工具名白名单双过滤（allowList 为空 = 不选任何工具，与"不限制"明确区分） |
| `AllTools()` | 全部服务的全部工具 |
| `Statuses()` | 各服务连接状态与最近错误（用于健康展示） |
| `TestConnection(ctx, server)` | 保存前连通性测试（返回 ServerInfo 与耗时） |
| `CallTool(ctx, fullName, args)` | 按全名直接调用（绕过 agent 的场景用） |
| `RefreshOne / RefreshAll` | 手动刷新工具缓存 |
| `Close()` | 停止刷新循环并关闭全部 stdio 子进程 |

配置变更后调用 `Apply` 即可，不要反复 `NewManager`。

## 工具命名空间

跨服务同名工具会冲突，工具名统一加服务前缀：

```
mcp__{serverID}__{toolName}    例：mcp__files__read_file
```

模型看到的名字、事件里的 `ToolName`、`CallTool` 的参数都用全名。

## 只读判定与审批

服务端可通过 MCP annotations 声明 `readOnlyHint: true`。本包的判定规则：**缺失注解按不安全处理**——宁可多问一次，也不静默执行写操作。判定结果经 adapter 透传为 `tool.ReadOnlyTool`，最终由 agent 的策略机制（默认 confirm）执行。

## 客户端标识

initialize 时默认上报 `DefaultClientInfo()`（name 为 `dlz-goai`，version 从构建信息推断）。宿主应用应覆盖为自己的产品名，便于服务端识别调用方：

```go
// HTTP 传输
client := mcp.NewClient(url, headers)
client.Info = mcp.ClientInfo{Name: "MyApp", Version: "2.0"}
// stdio 传输
sc := mcp.NewStdioClient(command, args, env)
sc.Info = mcp.ClientInfo{Name: "MyApp", Version: "2.0"}
```

## 已知边界

- 无 OAuth（仅静态 Headers 鉴权：如 `Authorization: Bearer ...`）；
- 不处理服务端主动推送（GET 挂载流、stdio 通知）；
- 无自动重连退避（HTTP 会话失效 404 时重握手一次；配置变更走 `Apply` 重建）；
- 协议版本固定 `2024-11-05`，无协商回退。

需要以上能力时，`ServerClient` 接口（Initialize/ListTools/CallTool/Close）是扩展点——实现自定义传输后直接交给 Manager 管理。
