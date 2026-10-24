// Package model 定义 ChinaCPA 全局公共类型：账号凭证、额度、签到结果、模型信息。
package model

// Account 一个已纳管的账号凭证（跨 provider 归一化，落盘 data/accounts/<provider>-<uid>.json）。
type Account struct {
	Provider string `json:"provider"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Email    string `json:"email,omitempty"`

	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // Unix 秒，0 = 未知

	// WorkBuddy 专属
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Domain       string `json:"domain,omitempty"`
	Realm        string `json:"realm,omitempty"` // cn / global
	DeviceToken  string `json:"device_token,omitempty"`

	// Trae 专属
	MachineID string `json:"machine_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`

	// Qwen 专属（OAuth resource_url，如 portal.qwen.ai）
	ResourceURL string `json:"resource_url,omitempty"`

	// Trae 专属（PKCE 设备密钥 + 刷新客户端绑定 + 区域 host 覆盖）
	DevicePrivateKey string `json:"device_private_key,omitempty"`
	RefreshClientID  string `json:"refresh_client_id,omitempty"`
	AuthHost         string `json:"auth_host,omitempty"` // OAuth host 覆盖（回调 host 参数）

	// Source 凭证类型（qoder：pat / oauth），影响会话构建方式
	Source string `json:"source,omitempty"`

	Disabled bool  `json:"disabled,omitempty"`
	CreatedAt int64 `json:"created_at,omitempty"`
	LastUsedAt int64 `json:"last_used_at,omitempty"`
}

// QuotaInfo 账号额度快照（各 provider 归一化 + 平台原始字段保留）。
type QuotaInfo struct {
	Provider string `json:"provider"`
	UID      string `json:"uid"`

	// 归一化剩余额度：Used/Limit/Remaining 用各自平台主单位
	Used      float64 `json:"used"`
	Limit     float64 `json:"limit"`    // <=0 = 未知/无固定上限
	Remaining float64 `json:"remaining"`
	Unit      string  `json:"unit"`     // credits / requests/day / ide_credits
	Daily     bool    `json:"daily"`    // 是否每日重置
	Healthy   bool    `json:"healthy"`  // Remaining > 0 或未知

	// WorkBuddy：积分余额
	Credits int64 `json:"credits,omitempty"`
	// Trae：SOLO 通道 ide_credits / work 包 credits
	IdeCredits  float64 `json:"ide_credits,omitempty"`
	WorkCredits float64 `json:"work_credits,omitempty"`
	// Qwen：本地计数的今日请求数（对照每日上限）
	RequestsUsedToday int64 `json:"requests_used_today,omitempty"`

	Detail   map[string]any `json:"detail,omitempty"`
	FetchedAt int64          `json:"fetched_at"`
}

// CheckinResult 签到结果。
type CheckinResult struct {
	OK            bool   `json:"ok"`
	Already       bool   `json:"already,omitempty"` // 今日已签
	Message       string `json:"message"`
	CreditsEarned int64  `json:"credits_earned,omitempty"`
}

// ModelInfo 模型条目。
type ModelInfo struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Name          string `json:"name,omitempty"`
	Description   string `json:"description,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	MaxTokens     int64  `json:"max_tokens,omitempty"`
	SupportsTools bool   `json:"supports_tools,omitempty"`
	SupportsImages bool  `json:"supports_images,omitempty"`
	Credits       string `json:"credits,omitempty"` // WorkBuddy 计价倍率原文
}

// LoginSession 一次登录会话（面板发起，浏览器完成，面板轮询结果）。
type LoginSession struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	URL      string `json:"url"`                  // 用户在浏览器打开的授权地址
	UserCode string `json:"user_code,omitempty"`  // Qwen 设备码展示

	// 内部轮询上下文（不序列化到面板响应）
	DeviceCode   string `json:"-"`
	CodeVerifier string `json:"-"`
	State        string `json:"-"`
	CallbackURL  string `json:"-"` // Trae 回调地址
}

// LoginStatus 轮询登录状态。
const (
	LoginPending = "pending"
	LoginDone    = "success"
	LoginFailed  = "failed"
)

// PollResult 登录轮询结果。
type PollResult struct {
	Status  string   `json:"status"`
	Message string   `json:"message,omitempty"`
	Account *Account `json:"account,omitempty"`
}
