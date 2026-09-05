package tool

// RetryPolicy 描述工具的外部副作用在"执行了但结果未记录"场景下的重试安全性。
//
// 进程在工具开始执行后、结果落库前崩溃时，恢复逻辑无法证明该调用没有
// 执行过。工具通过本分级声明此时的正确行为；未声明的工具视为
// RetryPolicyRetrySafe（保持既有行为）。
//
// 注意：这只影响恢复（Resume）路径。正常运行中的工具调用不受此分级约束。
type RetryPolicy int

const (
	// RetryPolicyRetrySafe 重试安全：重复执行无外部副作用，或副作用幂等
	//（如读取类工具、带业务幂等键的写入）。恢复时直接重新执行。默认值。
	RetryPolicyRetrySafe RetryPolicy = iota
	// RetryPolicyNeedsVerify 结果不确定：恢复时应先核实外部状态
	//（例如查询工单是否已创建）再决定是否重试，由模型用只读工具完成核实。
	RetryPolicyNeedsVerify
	// RetryPolicyNoRetry 禁止自动重试：可能已产生不可重复的副作用
	//（如发邮件、扣款）。恢复时不自动执行，提示模型转人工或先验证。
	RetryPolicyNoRetry
)

// RetryClassifier 工具可选实现的恢复分级接口。
// 进程内工具（Func）与 MCP 适配工具均可按需实现。
type RetryClassifier interface {
	RetryPolicy() RetryPolicy
}

// PolicyOf 探测工具的恢复分级；未声明时返回 RetryPolicyRetrySafe。
func PolicyOf(t Tool) RetryPolicy {
	if c, ok := t.(RetryClassifier); ok {
		return c.RetryPolicy()
	}
	return RetryPolicyRetrySafe
}
