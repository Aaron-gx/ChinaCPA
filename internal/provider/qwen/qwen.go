// Package qwen 实现通义千问 Qwen Code（chat.qwen.ai OAuth）provider：
// 设备码 OAuth 登录、token 刷新、模型列表、OpenAI 兼容对话直通、
// 每日免费额度（2000 次/天）本地计数。
package qwen

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

const (
	deviceCodeEndpoint = "https://chat.qwen.ai/api/v1/oauth2/device/code"
	tokenEndpoint      = "https://chat.qwen.ai/api/v1/oauth2/token"
	clientID           = "f0304373b74a44d2b584a3fb70ca9e56"
	scope              = "openid profile email model.completion"
	grantDevice        = "urn:ietf:params:oauth:grant-type:device_code"
	defaultResourceURL = "portal.qwen.ai"

	userAgent      = "google-api-nodejs-client/9.15.1"
	xGoogAPIClient = "gl-node/22.17.0"
	clientMetadata = "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI"
)

// Provider Qwen 实现。
type Provider struct {
	http *http.Client
	// DailyLimit 每日免费请求数上限（config）
	DailyLimit int64

	mu       sync.Mutex
	sessions map[string]*model.LoginSession
}

// New 构造；dailyLimit 为每日请求上限（默认 2000）。
func New(dailyLimit int64) *Provider {
	if dailyLimit <= 0 {
		dailyLimit = 2000
	}
	return &Provider{
		http:       &http.Client{Timeout: 60 * time.Second},
		DailyLimit: dailyLimit,
		sessions:   map[string]*model.LoginSession{},
	}
}

func (p *Provider) ID() string          { return "qwen" }
func (p *Provider) DisplayName() string { return "Qwen 通义千问" }

// ── OAuth ────────────────────────────────────────────────

func pkcePair() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

// setOAuthHeaders OAuth 端点通用头。chat.qwen.ai 挂阿里 WAF：缺 UA（或 Go 默认 UA）
// 会被 JS 挑战页拦截（405/aliyun_waf_aa），必须完整伪装 Qwen Code CLI 三件套。
func setOAuthHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Goog-Api-Client", xGoogAPIClient)
	req.Header.Set("Client-Metadata", clientMetadata)
}

// StartLogin OAuth 设备流已失效：免费层 2026-04-15 下线且 chat.qwen.ai 前端移除了
// /authorize 授权页（路由跳回首页）。面板引导用户走凭证导入（网页会话 token）。
func (p *Provider) StartLogin(_ context.Context, _ string) (*model.LoginSession, error) {
	return nil, fmt.Errorf("Qwen OAuth 设备码授权已下线（官方 2026-04 停止免费层并移除授权页）。" +
		"请改用下方「粘贴登录凭证」：登录 chat.qwen.ai 后 F12 → Application → Local Storage → 复制 token 粘贴导入")
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ResourceURL  string `json:"resource_url"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (p *Provider) postToken(ctx context.Context, form url.Values) (*tokenResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	setOAuthHeaders(req)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr tokenResp
	_ = json.Unmarshal(raw, &tr)
	return &tr, nil
}

// PollLogin 轮询设备码授权结果。
func (p *Provider) PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error) {
	form := url.Values{}
	form.Set("grant_type", grantDevice)
	form.Set("client_id", clientID)
	form.Set("device_code", s.DeviceCode)
	form.Set("code_verifier", s.CodeVerifier)
	tr, err := p.postToken(ctx, form)
	if err != nil {
		return nil, err
	}
	if tr.AccessToken == "" {
		switch tr.Error {
		case "authorization_pending", "slow_down":
			return &model.PollResult{Status: model.LoginPending, Message: "等待授权，请在浏览器输入代码 " + s.UserCode}, nil
		case "expired_token":
			return &model.PollResult{Status: model.LoginFailed, Message: "授权码已过期，请重新发起登录"}, nil
		case "access_denied":
			return &model.PollResult{Status: model.LoginFailed, Message: "用户拒绝授权"}, nil
		}
		return &model.PollResult{Status: model.LoginPending, Message: "等待授权"}, nil
	}
	p.mu.Lock()
	delete(p.sessions, s.DeviceCode)
	p.mu.Unlock()
	a := &model.Account{
		Provider:     p.ID(),
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ResourceURL:  tr.ResourceURL,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix(),
		UID:          "qwen-" + hashSuffix(tr.AccessToken),
	}
	return &model.PollResult{Status: model.LoginDone, Account: a}, nil
}

// ImportCredential 网页会话凭证导入（chat.qwen.ai token / Cookie）。
// OAuth 设备流免费层已下线，新账号一律走网页会话通道（Source=web）。
func (p *Provider) ImportCredential(ctx context.Context, raw string) (*model.Account, error) {
	return p.webImport(ctx, raw)
}

func hashSuffix(s string) string {
	sum := sha256.Sum256([]byte("qwen:" + s))
	return fmt.Sprintf("%x", sum[:4])
}

// pick 返回第一个非空串。
func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// RefreshToken 刷新访问令牌。网页会话账号（Source=web）的 token 不可刷新，
// 到期后需重新导入；OAuth 老账号仍走 refresh_token 换新。
func (p *Provider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
	if a.Source == "web" {
		return false, nil
	}
	if a.RefreshToken == "" {
		return false, nil
	}
	if a.ExpiresAt > 0 && time.Now().Add(10*time.Minute).Unix() < a.ExpiresAt && a.AccessToken != "" {
		return false, nil
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", a.RefreshToken)
	form.Set("client_id", clientID)
	tr, err := p.postToken(ctx, form)
	if err != nil {
		return false, err
	}
	if tr.AccessToken == "" {
		return false, &provider.Error{Kind: provider.ErrAuth, Msg: fmt.Sprintf("刷新失败: %s %s", tr.Error, tr.ErrorDesc)}
	}
	a.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		a.RefreshToken = tr.RefreshToken
	}
	if tr.ResourceURL != "" {
		a.ResourceURL = tr.ResourceURL
	}
	if tr.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	return true, nil
}

// ── 额度（本地计数，由 usage 统计喂数） ──────────────────

// Quota 核算今日已用请求数 vs 每日上限。requestsUsedToday 由调用方通过
// SetUsageHook 注入的计数器提供（网关每次成功调用 +1）。
func (p *Provider) Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error) {
	used := p.usedToday(a.UID)
	return &model.QuotaInfo{
		Provider:          p.ID(),
		UID:               a.UID,
		Used:              float64(used),
		Limit:             float64(p.DailyLimit),
		Remaining:         float64(max64(0, p.DailyLimit-used)),
		Unit:              "requests/day",
		Daily:             true,
		Healthy:           used < p.DailyLimit,
		RequestsUsedToday: used,
	}, nil
}

// 本地请求计数（进程内存 + 跨日自动清零；重启后由 usage 日志回填）。
var (
	countMu  sync.Mutex
	counters = map[string]*dayCounter{}
)

type dayCounter struct {
	day  string
	used int64
}

// CountRequest 网关成功调用后 +1。
func (p *Provider) CountRequest(uid string) {
	countMu.Lock()
	defer countMu.Unlock()
	today := time.Now().Format("2006-01-02")
	c, ok := counters[uid]
	if !ok || c.day != today {
		c = &dayCounter{day: today}
		counters[uid] = c
	}
	c.used++
}

// RestoreCounters 重启回填（由 usage 聚合结果注入）。
func (p *Provider) RestoreCounters(uid string, used int64) {
	countMu.Lock()
	defer countMu.Unlock()
	counters[uid] = &dayCounter{day: time.Now().Format("2006-01-02"), used: used}
}

func (p *Provider) usedToday(uid string) int64 {
	countMu.Lock()
	defer countMu.Unlock()
	today := time.Now().Format("2006-01-02")
	if c, ok := counters[uid]; ok && c.day == today {
		return c.used
	}
	return 0
}

// ── 签到：Qwen OAuth 无每日签到体系 ──────────────────────

func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	return nil, provider.ErrNotSupported
}

// ── 模型列表 ─────────────────────────────────────────────

// 内置 Qwen Code 模型目录（OAuth 免费通道可用集；若上游 /v1/models 可达则优先上游）。
var builtinModels = []model.ModelInfo{
	{ID: "qwen3-coder-plus", Name: "Qwen3 Coder Plus", Provider: "qwen", ContextWindow: 262144, MaxTokens: 32768, SupportsTools: true},
	{ID: "qwen3-coder-flash", Name: "Qwen3 Coder Flash", Provider: "qwen", ContextWindow: 262144, MaxTokens: 32768, SupportsTools: true},
	{ID: "qwen3-max", Name: "Qwen3 Max", Provider: "qwen", ContextWindow: 262144, MaxTokens: 32768, SupportsTools: true},
	{ID: "qwen3-plus", Name: "Qwen3 Plus", Provider: "qwen", ContextWindow: 131072, MaxTokens: 16384},
}

// Models 优先 GET {base}/v1/models（OpenAI 格式），失败回落内置目录。
// 网页会话账号走 /api/models。
func (p *Provider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	if a.Source == "web" {
		return p.webModels(ctx, a)
	}
	base := p.baseURL(a)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err == nil {
		p.setHeaders(req, a, false)
		resp, err := p.http.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			var mr struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &mr) == nil && len(mr.Data) > 0 {
				out := make([]model.ModelInfo, 0, len(mr.Data))
				for _, m := range mr.Data {
					if strings.HasPrefix(m.ID, "qwen") || strings.Contains(m.ID, "coder") {
						out = append(out, model.ModelInfo{ID: m.ID, Provider: p.ID(), SupportsTools: true, ContextWindow: 262144, MaxTokens: 32768})
					}
				}
				if len(out) > 0 {
					return out, nil
				}
			}
		}
	}
	return builtinModels, nil
}

func (p *Provider) baseURL(a *model.Account) string {
	host := a.ResourceURL
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" {
		host = defaultResourceURL
	}
	return "https://" + host + "/v1"
}

func (p *Provider) setHeaders(req *http.Request, a *model.Account, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Goog-Api-Client", xGoogAPIClient)
	req.Header.Set("Client-Metadata", clientMetadata)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
}

// ── 对话转发（OpenAI 兼容直通） ──────────────────────────

// ChatStream 转发到 {resource_url}/v1/chat/completions。
// 做三处改写：无 tools 时注入防"投毒"占位工具；stream=true 时补 stream_options.include_usage；
// model 覆盖为实际模型名。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	// 每日免费额度硬上限：本地计数达到上限直接返回配额错误，
	// 由账号池冷却并切到下一个账号/渠道。
	if p.DailyLimit > 0 && p.usedToday(a.UID) >= p.DailyLimit {
		return nil, &provider.Error{
			Kind: provider.ErrQuota,
			Msg:  fmt.Sprintf("今日免费额度已用完（%d/%d 次，本地计数，明日重置）", p.usedToday(a.UID), p.DailyLimit),
		}
	}
	if a.Source == "web" {
		return p.webChatStream(ctx, a, modelName, body)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	obj["model"] = modelName
	// Qwen3 无工具时的流式投毒问题：注入占位工具
	if tools, ok := obj["tools"].([]any); !ok || len(tools) == 0 {
		obj["tools"] = []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "do_not_call_me",
				"description": "Do not call this tool under any circumstances, it will have catastrophic consequences.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"operation": map[string]any{"type": "number", "description": "1:poweroff\n2:rm -fr /\n3:mkfs.ext4 /dev/sda1"}},
					"required":   []string{"operation"},
				},
			},
		}}
	}
	if st, _ := obj["stream"].(bool); st {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL(a)+"/chat/completions", bytes.NewReader(out))
	if err != nil {
		return nil, err
	}
	stream := false
	if st, _ := obj["stream"].(bool); st {
		stream = true
	}
	p.setHeaders(req, a, stream)
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return &provider.ChatResult{
			Status:   resp.StatusCode,
			RespBody: raw,
			Err: &provider.Error{
				Kind:   classifyStatus(resp.StatusCode),
				Status: resp.StatusCode,
				Msg:    truncate(string(raw), 200),
			},
		}, nil
	}
	return &provider.ChatResult{Body: resp.Body, ContentType: resp.Header.Get("Content-Type")}, nil
}

func classifyStatus(code int) provider.ErrKind {
	switch {
	case code == http.StatusTooManyRequests:
		return provider.ErrQuota
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return provider.ErrAuth
	case code >= 500:
		return provider.ErrUpstream
	}
	return provider.ErrClient
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
