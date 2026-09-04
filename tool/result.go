package tool

// Citation 标识一次工具调用所依据的来源。
//
// 定义在 tool 包而非 message 包，是为了让 tool 保持零依赖：
// message 需要引用工具调用，而 tool 需要携带引用，二者必须单向。
// Citation 一次工具调用所依据的来源。同时覆盖互联网来源（URL）
// 与本地知识库来源（DocID/RelPath/Section），字段按需填写。
type Citation struct {
	Kind    string `json:"kind,omitempty"` // 来源类别（如 web/mcp/doc），由调用方定义
	DocID   int64  `json:"doc_id,omitempty"`
	Title   string `json:"title"`
	URL     string `json:"url,omitempty"`
	RelPath string `json:"rel_path,omitempty"` // 本地文档相对路径
	Section string `json:"section,omitempty"`  // 文档内章节锚点
	Snippet string `json:"snippet,omitempty"`
	Source  string `json:"source,omitempty"` // 来源显示名（如服务名）
}

// Result 一次工具执行的产出。
//
// IsError 与 error 的区分：error 表示执行本身失败（连接超时、进程崩溃），
// IsError 表示工具成功执行但业务上失败（参数不合法、记录不存在）。
// 后者要把原因写进 Content 交回模型，这样模型有机会自我纠正，
// 而 error 只会终止当前步骤。
type Result struct {
	Content   string
	IsError   bool
	Citations []Citation
	Metadata  map[string]any
}

// Text 便捷构造：把纯文本作为成功结果。
func Text(content string) Result { return Result{Content: content} }

// Error 便捷构造：执行成功但业务失败，原因会回传给模型。
func Error(content string) Result { return Result{Content: content, IsError: true} }
