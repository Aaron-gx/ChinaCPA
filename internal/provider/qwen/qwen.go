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

	userAgent        = "google-api-nodejs-client/9.15.1"
	xGoogAPIClient   = "gl-node/22.17.0"
	clientMetadata   = "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI"
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

// StartLogin 发起设备码授权。
func (p *Provider) StartLogin(ctx context.Context, _ string) (*model.LoginSession, error) {
	verifier, challenge := pkcePair()
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("scope", scope)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceCodeEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发起设备码授权失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("设备码授权失败 http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var df struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &df); err != nil || df.DeviceCode == "" {
		return nil, fmt.Errorf("设备码响应异常: %s", truncate(string(raw), 200))
	}
	u := df.VerificationURIComplete
	if u == "" {
		u = df.VerificationURI + "?user_code=" + df.UserCode
	}
	s := &model.LoginSession{
		Provider:    p.ID(),
		ID:          df.DeviceCode,
		URL:         u,
		UserCode:    df.UserCode,
		DeviceCode:  df.DeviceCode,
		CodeVerifier: verifier,
	}
	p.mu.Lock()
	p.sessions[df.DeviceCode] = s
	p.mu.Unlock()
	return s, nil
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

// ImportCredential 支持粘贴 refresh token / 完整 token JSON。
func (p *Provider) ImportCredential(ctx context.Context, raw string) (*model.Account, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("凭证为空")
	}
	a := &model.Account{Provider: p.ID()}
	if strings.HasPrefix(raw, "{") {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %v", err)
		}
		str := func(k string) string {
			if v, ok := m[k].(string); ok {
				return v
			}
			return ""
		}
		a.AccessToken = pick(str("access_token"), str("accessToken"))
		a.RefreshToken = pick(str("refresh_token"), str("refreshToken"))
		a.ResourceURL = pick(str("resource_url"), str("resourceUrl"))
		if e := pick(str("expiry_date"), str("expire"), str("expired")); e != "" {			if t, err := time.Parse(time.RFC3339, e); err == nil {
				a.ExpiresAt = t.Unix()
			}
		}
	} else {
		a.RefreshToken = raw
	}
	if a.AccessToken == "" && a.RefreshToken == "" {
		return nil, fmt.Errorf("凭证中未找到 access_token / refresh_token")
	}
	if _, err := p.RefreshToken(ctx, a); err != nil && a.AccessToken == "" {
		return nil, err
	}
	if a.UID == "" {
		a.UID = "qwen-" + hashSuffix(pick(a.AccessToken, a.RefreshToken))
	}
	return a, nil
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

// RefreshToken 用 refresh token 换新 access token。
func (p *Provider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
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
	countMu    sync.Mutex
	counters   = map[string]*dayCounter{}
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
func (p *Provider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
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
