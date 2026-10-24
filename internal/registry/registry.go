// Package registry 是 CPA 模型注册表的最小垫片：只保留 thinking/signature 翻译层
// 依赖的类型与查询入口。ChinaCPA 不维护静态模型注册表，LookupModelInfo 恒返回
// nil（"未知模型"），调用方（internal/thinking）对 nil 均有优雅降级路径。
package registry

// ThinkingSupport 模型思考预算支持信息（与 CPA internal/registry 同构）。
type ThinkingSupport struct {
	Min          int    `json:"min,omitempty"`
	Max          int    `json:"max,omitempty"`
	ZeroAllowed  bool   `json:"zero_allowed,omitempty"`
	DynamicAllowed bool `json:"dynamic_allowed,omitempty"`
	DisableType  string `json:"disable_type,omitempty"`
	Levels       []string `json:"levels,omitempty"`
	Default      string `json:"default,omitempty"`
	CanDisable   bool   `json:"can_disable,omitempty"`
	Summary      string `json:"summary,omitempty"`
}

// ModelInfo 模型元信息（仅保留翻译层访问的字段）。
type ModelInfo struct {
	ID            string           `json:"id"`
	Type          string           `json:"type"`
	UserDefined   bool             `json:"-"`
	SupportConfigurationUpdate bool `json:"-"`
	Thinking      *ThinkingSupport `json:"thinking,omitempty"`
	InputTokenLimit  int          `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit int          `json:"outputTokenLimit,omitempty"`
}

// LookupModelInfo 按模型 ID 查询注册表；无静态注册表，恒返回 nil。
// thinking 层拿到 nil 走"无思考配置"默认路径（与 CPA 对未知模型的行为一致）。
func LookupModelInfo(modelID string, provider ...string) *ModelInfo {
	_ = modelID
	_ = provider
	return nil
}
