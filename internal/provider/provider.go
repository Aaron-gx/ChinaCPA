// Package provider 定义 ChinaCPA 的平台 Provider 抽象：
// 登录纳管、凭证导入、token 刷新、额度查询、签到、模型列表、对话转发。
package provider

import (
	"context"
	"errors"
	"io"
	"net/http"

	"chinacpa/internal/model"
)

// ErrKind 错误分类（账号池据此决定冷却策略）。
type ErrKind int

const (
	ErrNone     ErrKind = iota
	ErrAuth             // 凭证失效：需要重新登录，账号标记 expired
	ErrQuota            // 配额耗尽：长冷却（到明日重置）
	ErrRate             // 软限流：短冷却
	ErrClient           // 其他客户端错误
	ErrUpstream         // 上游/网络故障：短冷却，不罚号
)

// Error 携带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Msg
	}
	return e.Msg
}

// AsError 归类 err 为 *Error（非 *Error 的按上游传输故障处理）。
func AsError(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return &Error{Kind: ErrUpstream, Msg: err.Error()}
}

// ErrNotSupported 平台不支持该能力（如 Qwen 无签到）。
var ErrNotSupported = errors.New("此平台不支持该功能")

// ChatResult 一次对话转发的输出。
// OpenAI 兼容流：rc 为已转换成 OpenAI SSE 的字节流（或非流式 JSON 原文），
// 调用方负责 Close。Status/RespBody 仅在错误路径（HTTP >= 400）有值。
type ChatResult struct {
	Body         io.ReadCloser // 成功：响应体（SSE 或 JSON）
	ContentType  string        // text/event-stream 或 application/json
	Status       int           // >=400 时的上游状态码
	RespBody     []byte        // >=400 时的上游响应体
	Err          *Error        // 分类错误
	UsageHook    func(line []byte) // SSE 行回调（网关提取 usage 用）
}

// Provider 平台抽象。实现必须并发安全（账号对象由池串行传给单次调用）。
type Provider interface {
	ID() string
	DisplayName() string

	// StartLogin 发起浏览器登录，返回登录 URL（面板展示给用户打开）。
	StartLogin(ctx context.Context, callbackBase string) (*model.LoginSession, error)
	// PollLogin 轮询登录结果；pending 表示用户尚未完成授权。
	PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error)

	// ImportCredential 粘贴凭证导入（token JSON / refresh token 等）。
	ImportCredential(ctx context.Context, raw string) (*model.Account, error)

	// RefreshToken 刷新访问令牌；changed 表示凭证有更新（调用方负责落盘）。
	RefreshToken(ctx context.Context, a *model.Account) (changed bool, err error)

	// Quota 查询账号额度。
	Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error)

	// Checkin 每日签到；不支持的平台返回 ErrNotSupported。
	Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error)

	// Models 拉取账号可用模型列表。
	Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error)

	// ChatStream 发起对话（OpenAI 兼容请求体），返回 OpenAI 兼容响应。
	// model 参数为去掉 provider 前缀后的裸模型名。
	ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*ChatResult, error)
}

// helpers 共享的小工具（各 provider 实现使用）。

// NewRequest 构造带超时 context 的请求。
func NewRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, url, body)
}
