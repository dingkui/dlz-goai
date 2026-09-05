# 使用手册：tool — 工具契约

`tool` 定义与模型厂商无关的工具契约，零依赖。agent 只认 `tool.Tool` 接口——MCP 工具、进程内函数、HTTP 远程工具都实现它，agent 因此不需要知道工具来自哪里。

## Tool 接口

```go
type Tool interface {
	// 给模型看的描述（JSON Schema 风格参数说明）
	Definition() Definition
	// 执行工具
	Execute(ctx context.Context, args map[string]any) (Result, error)
}
```

`Definition{Name, Description, Parameters}`：`Parameters` 是标准 JSON Schema map，直接进模型请求。

## 创建工具的三种方式

### 1. tool.NewFunc — 包装普通函数

```go
t := tool.NewFunc("lookup_ticket", "查询工单状态", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"ticket_id": map[string]any{"type": "string"},
	},
	"required": []string{"ticket_id"},
}, true, // readOnly
	func(ctx context.Context, args map[string]any) (tool.Result, error) {
		return tool.Text("..."), nil
	})
```

`parameters` 传 nil 时自动补一个空对象 schema（部分模型服务不接受缺失的 parameters 字段）。

### 2. tool.Typed — 结构体自动 schema（推荐）

```go
type searchInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"` // omitempty/指针字段不进 required
}

t := tool.Typed("search", "搜索知识库", true,
	func(ctx context.Context, in searchInput) (tool.Result, error) {
		return tool.Text("..."), nil
	})
```

JSON Schema 从结构体反射生成；参数绑定失败时返回 **IsError 结果**（含 schema 原文）回传模型自我纠正，而不是 error 终止本轮。

### 3. 实现接口 — 远程/MCP 工具

实现 `Definition()` + `Execute()` 即可，`mcp` 包的 adapter 就是这样把 MCP 工具接进来的。

## 两种失败，两条通道

| 通道 | 写法 | agent 行为 |
|---|---|---|
| 业务失败 | `return tool.Error("记录不存在"), nil` | 原因写进 tool 消息回传模型，给它自我纠正的机会 |
| 执行失败 | `return tool.Result{}, errors.New("db down")` | 终止当前步骤，错误上抛 |

判定标准：**模型还能为这个失败做什么吗？** 能 → 业务失败；不能 → error。

`Result` 完整字段：`Content`（回传模型的文本）、`IsError`、`Citations`（引用，agent 自动汇总去重）、`Metadata`（调用方自用，不进模型载荷）。

## 可选能力接口

按需实现，agent 与 runtime 会探测：

```go
// 只读声明：默认策略从"需审批"变为"直接执行"
type ReadOnlyTool interface{ IsReadOnly() bool }

// 来源标注：事件与审批请求里区分工具出处（如 MCP 服务名）
type Sourced interface{ SourceID() string; SourceName() string }

// 恢复分级：runtime.Resume 对账时决定"已开始执行但无结果"的调用怎么办
type RetryClassifier interface{ RetryPolicy() tool.RetryPolicy }
```

`RetryPolicy` 三级（默认 `RetrySafe`）：

| 分级 | 语义 | 恢复行为 |
|---|---|---|
| `RetryPolicyRetrySafe` | 重复执行无外部副作用（查询类、带幂等键的写入） | 重新执行 |
| `RetryPolicyNeedsVerify` | 结果不确定，应先核实外部状态 | 本轮持续阻断，返回核实指引 |
| `RetryPolicyNoRetry` | 可能已产生不可重复副作用（发邮件、扣款） | 本轮持续阻断，返回转人工指引 |

## Registry — 集中管理

```go
reg := tool.NewRegistry()
reg.MustRegister(lookup)          // 失败 panic，适合初始化装配
err := reg.Register(assign)       // 同名注册返回错误（名字冲突几乎总是配置错误）
reg.SetPolicy("assign_ticket", tool.PolicyConfirm) // 按名覆盖策略

all := reg.List()                 // 按名字排序（顺序稳定 → 提示词稳定）
some := reg.Select([]string{"lookup_ticket"}) // 白名单选择；空白名单 = 不选任何工具
policy := reg.PolicyFor("assign_ticket")      // 判定生效策略
```

`Policy` 三值：`auto`（直接执行）/ `confirm`（先确认）/ `deny`（禁止，模型收到错误回执）。

## Citation — 引用

```go
tool.Result{
	Content: "...",
	Citations: []tool.Citation{{
		Kind: "web",     // 或 "doc"（本地知识库）、"mcp"
		Title: "来源标题",
		URL:   "https://...",          // 互联网来源
		DocID: 42, RelPath: "a/b.md",  // 本地来源
		Section: "安装", Snippet: "...",
	}},
}
```

agent 把多步工具的引用**汇总去重**（按 URL 或 DocID）放进最终 `Result.Citations`，供前端渲染"回答依据"。
