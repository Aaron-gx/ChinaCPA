// Package workbuddy 实现腾讯 WorkBuddy / CodeBuddy（CN copilot.tencent.com +
// codebuddy.cn / 国际 workbuddy.ai）provider：OAuth 登录、token 刷新、积分签到、
// 任务中心、额度查询、模型列表、对话转发。
//
// 接口口径来自开源参考实现（workbuddy2api-panel）的实测结论：
//   - 信封 {code, msg, data}；code != 0 为业务错误；12153 = 会话/RT 失效
//   - 所有请求必带 X-CodeBuddy-Request: 1
//   - UA 对齐官方桌面端 WorkBuddy/<ver> <platform>/<ver> CLI/<cli>
package workbuddy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

const (
	chatBaseCN    = "https://copilot.tencent.com"
	billingBaseCN = "https://www.codebuddy.cn"
	webBaseCN     = "https://www.workbuddy.cn"
	globalBase    = "https://www.workbuddy.ai"
	clientVersion = "5.5.4"
	cliVersion    = "2.137.1"
	originCN      = "https://www.codebuddy.cn"
	originGlobal  = "https://www.workbuddy.ai"
)

// globalModelDisplay 国际版抽象模型 id → 直白中文档位名（展示用）。
// 官方用抽象 id 掩盖底座模型；据 descriptionZh/供应商等情况归纳为通俗称谓。
var globalModelDisplay = map[string]string{
	"fast-model":              "快速模型 Fast",
	"balanced-model":          "均衡模型 Balanced",
	"primary-model":           "旗舰模型 Primary",
	"auto-chat":               "智能自动 Auto",
	"nes-1.2":                 "自动选择 auto",
	"enhance-1.0":             "增强模型 Enhance",
	"nes-1.1":                 "增强版 NES",
	"completion-1.0":          "文本补全 completion",
	"o4-mini":                 "GPT-4o-Mini",
	"codewise-jump":           "代码跳转 codewise",
	"gpt-image-2.5-sunburst":  "GPT 图像生成",
	"seedance-2.5":            "Seedance 视频生成",
	"hy-image-3.5":            "Hy 图像生成",
	"kling-video-v3":          "可灵视频生成",
	"gpt-6-astra":             "GPT-6-Astra",
	"gpt-6-sol":               "GPT-6-Sol",
	"gpt-6-luna":              "GPT-6-Luna",
	"gpt-6.1-sol":             "GPT-6.1-Sol",
	"grok-4.7":                "Grok-4.7",
	"gemini-3.8-flash":        "Gemini-3.8-Flash",
	"glm-5.3-flash":           "GLM-5.3-Flash",
	"minimax-m3.1-flash":      "MiniMax-M3.1-Flash",
	"kimi-k3.1-pro":           "Kimi-K3.1-Pro",
	"kimi-k3.1-pro-highspeed": "Kimi-K3.1-Pro-HighSpeed",
	"deepseek-v4.1-flash":     "DeepSeek-V4.1-Flash",
	"deepseek-v4.1-flash-sg":  "DeepSeek-V4.1-Flash-SG",
}

// Provider WorkBuddy 实现（realm: "cn" / "global" → id workbuddy / workbuddy-intl）。
type Provider struct {
	http *http.Client
	// realm 实例区域：cn = 国内（codebuddy.cn），global = 国际（workbuddy.ai）
	realm string
	// loginSessions 面板登录会话（state -> session）
	mu       sync.Mutex
	sessions map[string]*model.LoginSession
	// creditMu 实测模型倍率缓存（由每次对话 usage.credit 反推，账号级）。
	creditMu      sync.Mutex
	measuredCreds map[string]float64           // uid → credit 基准值（该账号实测 glm 基准等）
	measuredRates map[string]map[string]string // uid → model → "x0.34" 展示倍率
}

// New 构造（realm "cn" 或 "global"）。
func New(realm string) *Provider {
	if realm != "global" {
		realm = "cn"
	}
	return &Provider{
		http:          &http.Client{Timeout: 60 * time.Second},
		realm:         realm,
		sessions:      map[string]*model.LoginSession{},
		measuredCreds: map[string]float64{},
		measuredRates: map[string]map[string]string{},
	}
}

func (p *Provider) ID() string {
	if p.realm == "global" {
		return "workbuddy-intl"
	}
	return "workbuddy"
}

func (p *Provider) DisplayName() string {
	if p.realm == "global" {
		return "WorkBuddy AI 国际版（workbuddy.ai）"
	}
	return "WorkBuddy / CodeBuddy 国内版"
}

// loginBase 登录态接口基址（国内 copilot.tencent.com，国际 workbuddy.ai 同构 API）。
func (p *Provider) loginBase() string {
	if p.realm == "global" {
		return globalBase
	}
	return chatBaseCN
}

// webLoginBase 登录页兜底地址（authUrl 缺失时拼 state 跳转用）。
func (p *Provider) webLoginBase() string {
	if p.realm == "global" {
		return globalBase
	}
	return webBaseCN
}

// ── realm / base 选择 ────────────────────────────────────

func isGlobal(a *model.Account) bool {
	if a == nil {
		return false
	}
	if strings.EqualFold(a.Realm, "global") {
		return true
	}
	d := strings.ToLower(a.Domain)
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

func chatBase(a *model.Account) string {
	if isGlobal(a) {
		return globalBase
	}
	return chatBaseCN
}

func billingBase(a *model.Account) string {
	if isGlobal(a) {
		return globalBase
	}
	return billingBaseCN
}

func originFor(a *model.Account) string {
	if isGlobal(a) {
		return originGlobal
	}
	return originCN
}

// ── 请求头 ───────────────────────────────────────────────

// stableID 按 uid 派生稳定设备/会话指纹（跨重启不变，账号间互异）。
func stableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

func (p *Provider) commonHeaders(req *http.Request, a *model.Account) {
	platform := "WorkBuddy"
	lang := "zh-CN"
	if (a != nil && isGlobal(a)) || (a == nil && p.realm == "global") {
		platform = "WorkBuddy AI"
		lang = "en-US"
	}
	origin := originFor(a)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", "WorkBuddy/"+clientVersion+" "+platform+"/"+clientVersion+" CLI/"+cliVersion)
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", lang)
	if a != nil && a.UID != "" {
		req.Header.Set("X-Machine-ID", stableID(a.UID, "machine"))
		req.Header.Set("X-Session-ID", stableID(a.UID, "session"))
	}
}

// billingHeaders billing 域头（余额/签到）。
func (p *Provider) billingHeaders(req *http.Request, a *model.Account) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CodeBuddy-Request", "1")
	if isGlobal(a) {
		req.Header.Set("Accept-Language", "en-US")
	} else {
		req.Header.Set("Accept-Language", "zh-CN")
	}
	req.Header.Set("User-Agent", "WorkBuddy/"+clientVersion)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// chatHeaders chat 出站头（含会话头族）。
func (p *Provider) chatHeaders(req *http.Request, a *model.Account) {
	p.commonHeaders(req, a)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if isGlobal(a) {
		req.Header.Set("X-No-Enterprise-Id", "1")
		req.Header.Set("X-Domain", "www.workbuddy.ai")
	} else {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if a.Domain != "" {
			req.Header.Set("X-Domain", a.Domain)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	}
	// 用量归属：伪造官方桌面端指纹
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", clientVersion)
	req.Header.Set("X-Product", "WorkBuddy")
	// 会话头族（32 hex）
	msgID := randHex(16)
	req.Header.Set("X-Conversation-ID", randHex(16))
	req.Header.Set("X-Conversation-Request-ID", msgID)
	req.Header.Set("X-Conversation-Message-ID", msgID)
	req.Header.Set("X-Request-ID", msgID)
	req.Header.Set("X-Root-Request-ID", msgID)
	req.Header.Set("X-Trace-ID", msgID)
	req.Header.Set("X-B3-TraceId", msgID)
	req.Header.Set("X-B3-SpanId", msgID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── 信封解析 ─────────────────────────────────────────────

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发请求解信封；HTTP >= 400 或 code != 0 返回 *provider.Error。
func (p *Provider) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, classifyHTTP(resp.StatusCode, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		return nil, classifyBiz(env.Code, env.Msg)
	}
	return env.Data, nil
}

func classifyHTTP(status int, raw []byte) *provider.Error {
	msg := truncate(string(raw), 200)
	kind := provider.ErrClient
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = provider.ErrAuth
		msg = "凭证无效或已过期（HTTP " + strconv.Itoa(status) + "），请重新登录纳管"
	case status == http.StatusTooManyRequests:
		kind = provider.ErrRate
		msg = "请求过于频繁（429），稍后自动重试"
	case status >= 500:
		kind = provider.ErrUpstream
		msg = "上游服务异常（HTTP " + strconv.Itoa(status) + "）"
	}
	return &provider.Error{Kind: kind, Status: status, Msg: msg}
}

func classifyBiz(code int, msg string) *provider.Error {
	kind := provider.ErrClient
	switch {
	case code == 12153 || strings.Contains(msg, "login") || strings.Contains(msg, "登录"):
		kind = provider.ErrAuth
	case code == 10001: // 今天已签到等幂等业务提示
		kind = provider.ErrNone
	case code == 11140 || strings.Contains(msg, "illegal"):
		kind = provider.ErrAuth
	}
	return &provider.Error{Kind: kind, Code: strconv.Itoa(code), Msg: truncate(msg, 200)}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// ── 登录（设备授权流） ───────────────────────────────────

type authStateResp struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

func (p *Provider) StartLogin(ctx context.Context, callbackBase string) (*model.LoginSession, error) {
	base := p.loginBase()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", nil)
	if err != nil {
		return nil, err
	}
	p.commonHeaders(req, nil)
	data, err := p.doJSON(req)
	if err != nil {
		return nil, fmt.Errorf("获取登录 state 失败: %w", err)
	}
	var st authStateResp
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" {
		return nil, fmt.Errorf("登录 state 响应异常: %s", truncate(string(data), 200))
	}
	s := &model.LoginSession{
		Provider: p.ID(),
		ID:       st.State,
		URL:      st.AuthURL,
		State:    st.State,
	}
	if s.URL == "" {
		s.URL = p.webLoginBase() + "/login?state=" + st.State
	}
	p.mu.Lock()
	p.sessions[st.State] = s
	p.mu.Unlock()
	return s, nil
}

type tokenBundle struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

func (p *Provider) PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error) {
	base := p.loginBase()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/plugin/auth/token?state="+s.State, nil)
	if err != nil {
		return nil, err
	}
	p.commonHeaders(req, nil)
	data, err := p.doJSON(req)
	if err != nil {
		// "login ing" 业务 code 非 0 = 用户尚未完成授权 → pending
		return &model.PollResult{Status: model.LoginPending, Message: "等待浏览器完成登录"}, nil
	}
	var tok tokenBundle
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return &model.PollResult{Status: model.LoginPending, Message: "等待授权完成"}, nil
	}
	a := &model.Account{
		Provider:     p.ID(),
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Domain:       tok.Domain,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
	}
	if p.realm == "global" {
		a.Realm = "global" // 国际实例账号显式标记，运行期所有请求走 workbuddy.ai
	}
	// 拿 uid / nickname
	areq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/plugin/login/account?state="+s.State, nil)
	if areq != nil {
		p.commonHeaders(areq, nil)
		areq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if adata, err := p.doJSON(areq); err == nil {
			var acct struct {
				UID          string `json:"uid"`
				UserId       string `json:"userId"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			}
			if json.Unmarshal(adata, &acct) == nil {
				if acct.UID == "" {
					acct.UID = acct.UserId
				}
				a.UID = acct.UID
				a.EnterpriseID = acct.EnterpriseID
				a.Nickname = acct.Nickname
			}
		}
	}
	if a.UID == "" {
		a.UID = "wb-" + randHex(6)
	}
	p.mu.Lock()
	delete(p.sessions, s.State)
	p.mu.Unlock()
	return &model.PollResult{Status: model.LoginDone, Account: a}, nil
}

// ImportCredential 支持粘贴： refreshToken、完整 token JSON、auth 文件 JSON。
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
		// 兼容 auth 文件嵌套形 {account:{...}, auth:{...}} 与平铺形
		flat := func(k string) string {
			if v, ok := m[k].(string); ok {
				return v
			}
			return ""
		}
		get := func(k1, k2 string) string {
			if v := flat(k1); v != "" {
				return v
			}
			if v := flat(k2); v != "" {
				return v
			}
			// 嵌套 auth/account 段
			for _, sec := range []string{"auth", "account"} {
				if secm, ok := m[sec].(map[string]any); ok {
					if v, ok := secm[k1].(string); ok && v != "" {
						return v
					}
					if v, ok := secm[k2].(string); ok && v != "" {
						return v
					}
				}
			}
			return ""
		}
		a.AccessToken = get("accessToken", "access_token")
		a.RefreshToken = get("refreshToken", "refresh_token")
		a.Domain = get("domain", "")
		a.UID = get("uid", "userId")
		a.EnterpriseID = get("enterpriseId", "enterprise_id")
		a.Nickname = get("nickname", "")
		a.DeviceToken = get("device_token", "deviceToken")
		if rt := get("realm", ""); rt != "" {
			a.Realm = rt
		}
		if a.UID == "" {
			a.UID = "wb-" + randHex(6)
		}
	} else {
		// 裸 token：优先当 refresh token（可换取完整凭证）；超长当 access token
		if len(raw) > 500 {
			a.AccessToken = raw
		} else {
			a.RefreshToken = raw
		}
		a.UID = "wb-" + randHex(6)
	}
	if a.AccessToken == "" && a.RefreshToken == "" {
		return nil, fmt.Errorf("凭证中未找到 accessToken / refreshToken")
	}
	// 国际实例导入的凭证默认按 global 区跑（JSON 里的 realm 优先）
	if p.realm == "global" && a.Realm == "" {
		a.Realm = "global"
	}
	if _, err := p.RefreshToken(ctx, a); err != nil && a.AccessToken == "" {
		return nil, err
	}
	return a, nil
}

// ── token 刷新 ───────────────────────────────────────────

func (p *Provider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
	if a.RefreshToken == "" {
		return false, nil
	}
	if a.ExpiresAt > 0 && time.Now().Add(10*time.Minute).Unix() < a.ExpiresAt {
		return false, nil // 尚未临近过期
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatBase(a)+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return false, err
	}
	p.commonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	data, err := p.doJSON(req)
	if err != nil {
		return false, err
	}
	var tok tokenBundle
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return false, fmt.Errorf("刷新响应异常: %s", truncate(string(data), 200))
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return true, nil
}

// NeedsRefresh token 是否已过期/临期。
func NeedsRefresh(a *model.Account, within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// ── 额度查询 ─────────────────────────────────────────────

type resourcePackage struct {
	CapacityRemain      int64 `json:"CapacityRemain"`
	CapacityUsed        int64 `json:"CapacityUsed"`
	CapacitySize        int64 `json:"CapacitySize"`
	CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64 `json:"CycleCapacitySize"`
}

// Quota 查询积分余额（聚合所有资源包）。
func (p *Provider) Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	path := "/v2/billing/meter/get-user-resource"
	if isGlobal(a) {
		path = "/billing/meter/get-user-resource"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, billingBase(a)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	p.billingHeaders(req, a)
	data, err := p.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				TotalDosage int64             `json:"TotalDosage"`
				Accounts    []resourcePackage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析额度响应失败: %w", err)
	}
	var remain, used, size int64
	for _, pk := range resp.Response.Data.Accounts {
		r, u, s := packageRemainUsed(pk)
		remain += r
		used += u
		size += s
	}
	if size > 0 {
		if d := size - remain; d > used {
			used = d
		}
	}
	if d := resp.Response.Data.TotalDosage; d > size {
		size = d
		if dd := size - remain; dd > used {
			used = dd
		}
	}
	q := &model.QuotaInfo{
		Provider:  p.ID(),
		UID:       a.UID,
		Used:      float64(used),
		Remaining: float64(remain),
		Unit:      "credits",
		Credits:   remain,
		Healthy:   remain > 0,
	}
	if size > 0 {
		q.Limit = float64(size)
	}
	return q, nil
}

func packageRemainUsed(a resourcePackage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = max64(0, a.CycleCapacityRemain)
		size = a.CycleCapacitySize
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ── 签到 ─────────────────────────────────────────────────

// Checkin 每日签到（code=10001 / “今天已签到” 视为已签）。
func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	path := "/v2/billing/meter/daily-checkin"
	if isGlobal(a) {
		path = "/billing/meter/daily-checkin"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, billingBase(a)+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	p.billingHeaders(req, a)
	_, err = p.doJSON(req)
	if err != nil {
		pe := provider.AsError(err)
		if strings.Contains(pe.Msg, "已签到") || pe.Code == "10001" {
			return &model.CheckinResult{OK: true, Already: true, Message: "今天已签到"}, nil
		}
		if pe.Kind == provider.ErrNone {
			return &model.CheckinResult{OK: true, Message: "签到成功"}, nil
		}
		return nil, err
	}
	return &model.CheckinResult{OK: true, Message: "签到成功"}, nil
}

// ── 任务中心（自动接受 + 领奖） ───────────────────────────

type growthTask struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title"`
	Credit       int64  `json:"reward_credit"`
	Energy       int64  `json:"reward_energy"`
	Target       int64  `json:"target"`
	Current      int64  `json:"current"`
	AcceptStatus string `json:"accept_status"`
	Status       string `json:"status"`
}

func (p *Provider) growthJSON(ctx context.Context, a *model.Account, method, path string, body any, mp bool) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	p.billingHeaders(req, a)
	if mp {
		req.Header.Set("X-Client-Platform", "miniprogram")
	}
	return p.doJSON(req)
}

// RunTasks 自动完成任务中心：列表（默认+小程序口径合并）→ 接受全部未接受 → 领取达标奖励。
// 返回 (获得的积分, 摘要)。
func (p *Provider) RunTasks(ctx context.Context, a *model.Account) (int64, string, error) {
	tasks, err := p.listTasks(ctx, a)
	if err != nil {
		return 0, "", err
	}
	var toAccept []string
	var earned int64
	var parts []string
	for _, t := range tasks {
		if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
			toAccept = append(toAccept, t.TaskCode)
		}
	}
	if len(toAccept) > 0 {
		_, _ = p.growthJSON(ctx, a, http.MethodPost, "/v2/activity/growth/tasks/accept", map[string]any{"task_codes": toAccept}, false)
		parts = append(parts, fmt.Sprintf("接受 %d 个任务", len(toAccept)))
	}
	for _, t := range tasks {
		claimable := (t.Status == "complete" || t.Current >= t.Target) && t.AcceptStatus != "claimed" && t.Target > 0
		if !claimable {
			continue
		}
		credit, _, cerr := p.claimReward(ctx, a, t.TaskCode)
		if cerr == nil && credit > 0 {
			earned += credit
			parts = append(parts, fmt.Sprintf("%s +%d", t.Title, credit))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "暂无可自动完成的任务")
	}
	return earned, strings.Join(parts, "；"), nil
}

func (p *Provider) listTasks(ctx context.Context, a *model.Account) ([]growthTask, error) {
	seen := map[string]bool{}
	var out []growthTask
	for _, mp := range []bool{false, true} {
		data, err := p.growthJSON(ctx, a, http.MethodGet, "/v2/activity/growth/tasks", nil, mp)
		if err != nil {
			continue
		}
		var resp struct {
			Tasks []growthTask `json:"tasks"`
		}
		if json.Unmarshal(data, &resp) != nil {
			continue
		}
		for _, t := range resp.Tasks {
			if t.TaskCode != "" && !seen[t.TaskCode] {
				seen[t.TaskCode] = true
				out = append(out, t)
			}
		}
	}
	return out, nil
}

func (p *Provider) claimReward(ctx context.Context, a *model.Account, taskCode string) (credit, energy int64, err error) {
	// chat 域领奖
	req, cerr := http.NewRequestWithContext(ctx, http.MethodPost,
		chatBase(a)+"/activity/growth/tasks/"+taskCode+"/claim", nil)
	if cerr == nil {
		p.billingHeaders(req, a)
		if data, e := p.doJSON(req); e == nil {
			var r struct {
				Credit int64 `json:"credit"`
				Energy int64 `json:"energy"`
			}
			_ = json.Unmarshal(data, &r)
			return r.Credit, r.Energy, nil
		}
	}
	// Web 域降级领奖
	webBase := webBaseCN
	if isGlobal(a) {
		webBase = globalBase
	}
	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, webBase+"/activity/growth/tasks/"+taskCode+"/claim", nil)
	if req2 != nil {
		p.billingHeaders(req2, a)
		req2.Header.Set("X-Client-Platform", "web")
		if data, e := p.doJSON(req2); e == nil {
			var r struct {
				Credit int64 `json:"credit"`
				Energy int64 `json:"energy"`
			}
			_ = json.Unmarshal(data, &r)
			return r.Credit, r.Energy, nil
		}
	}
	return 0, 0, fmt.Errorf("领取 %s 失败", taskCode)
}

// ── 模型列表 ─────────────────────────────────────────────

// Models 拉取模型列表。国内走 /console/enterprises/personal/models（agents[cli].models，
// 兼容数组/映射×字符串/对象）；国际区该端点 500，改从 /v3/config 的 models 数组直读
// （含 id/credits/maxInputTokens/maxOutputTokens/描述等完整字段）。
func (p *Provider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	if isGlobal(a) {
		return p.modelsGlobal(ctx, a)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatBase(a)+"/console/enterprises/personal/models", nil)
	if err != nil {
		return nil, err
	}
	p.commonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	data, err := p.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Agents json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}
	// agents: 数组形态 [{"name":"cli","models":[...]}] / 映射形态 {"cli":{"models":[...]}}
	type agentEntry struct {
		Name   string          `json:"name"`
		Models json.RawMessage `json:"models"`
	}
	var entries []agentEntry
	var asArr []agentEntry
	if json.Unmarshal(resp.Agents, &asArr) == nil {
		entries = asArr
	} else {
		var asMap map[string]agentEntry
		if json.Unmarshal(resp.Agents, &asMap) == nil {
			for name, e := range asMap {
				e.Name = name
				entries = append(entries, e)
			}
		}
	}
	var out []model.ModelInfo
	for _, ag := range entries {
		if ag.Name != "cli" && len(entries) > 1 {
			continue
		}
		out = append(out, parseModelList(ag.Models)...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("模型列表为空")
	}
	// 倍率牌价 + 限时优惠来自 /v3/config（企业端点新版只回模型名字符串数组）
	for id, c := range p.fetchV3Credits(ctx, a) {
		for i := range out {
			if out[i].ID == id {
				out[i].Credits = c
			}
		}
	}
	return out, nil
}

// modelsGlobal 国际区模型目录。
// 关键：国际版完整目录在 **codebuddy.ai**（endpoint 字段指向它）而非 workbuddy.ai
// （后者是裁剪版），且须用 CLI UA（CodeBuddyIDE UA 只能拿到部分档位）。
// 一次 GET https://www.codebuddy.ai/v3/config 即返回完整 data.models（含官方
// credits 倍率）+ data.agents[cli].models（完整可用清单）。以 cli 清单为主键，
// 再补入 models 数组里的图/视频等特殊模型。
func (p *Provider) modelsGlobal(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	// catalog 域：codebuddy.ai（endpoint）。workbuddy.ai 返回裁剪目录，弃用。
	const catalogHost = "https://www.codebuddy.ai"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogHost+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	p.commonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	// CLI UA 才能拿到完整目录（CodeBuddyIDE UA 只返回抽象档位子集）
	req.Header.Set("User-Agent", "WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	data, err := p.doJSON(req)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Models []struct {
			ID              string   `json:"id"`
			Name            string   `json:"name"`
			Credits         string   `json:"credits"`
			MaxInputTokens  int64    `json:"maxInputTokens"`
			MaxOutputTokens int64    `json:"maxOutputTokens"`
			Tags            []string `json:"tags"`
			DescriptionZh   string   `json:"descriptionZh"`
			DescriptionEn   string   `json:"descriptionEn"`
		} `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析国际版模型目录失败: %w", err)
	}
	// 元数据表（id → 条目，含官方 credits）
	meta := map[string]struct {
		ID              string   `json:"id"`
		Name            string   `json:"name"`
		Credits         string   `json:"credits"`
		MaxInputTokens  int64    `json:"maxInputTokens"`
		MaxOutputTokens int64    `json:"maxOutputTokens"`
		Tags            []string `json:"tags"`
		DescriptionZh   string   `json:"descriptionZh"`
		DescriptionEn   string   `json:"descriptionEn"`
	}{}
	for _, m := range cfg.Models {
		c := m
		meta[m.ID] = c
	}
	// cli 完整清单为主键
	cliNames := []string{}
	for _, ag := range cfg.Agents {
		if ag.Name == "cli" {
			cliNames = ag.Models
			break
		}
	}
	seen := map[string]bool{}
	order := make([]string, 0, len(cliNames)+len(meta))
	for _, id := range cliNames {
		if id == "" || id == "default-model" {
			continue
		}
		seen[id] = true
		order = append(order, id)
	}
	// 补充 cli 外的特殊模型（图/视频：hy-image-3.5、kling-video-v3 等）
	for id := range meta {
		if id == "" || id == "default-model" {
			continue
		}
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
	}

	var out []model.ModelInfo
	for _, id := range order {
		m := meta[id]
		display := oneOf(globalModelDisplay[id], m.Name)
		if display == "" || display == id {
			display = id
		}
		if desc := m.DescriptionZh; desc != "" && desc != display {
			display = display + " · " + desc
		}
		mi := model.ModelInfo{
			ID:            id,
			Name:          display,
			Description:   oneOf(m.DescriptionEn, m.DescriptionZh),
			Provider:      p.ID(),
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Credits:       m.Credits,
			Kind:          kindFromTags(m.Tags),
		}
		// 官方倍率缺失或为 0 时处理
		trimCredits := strings.TrimSpace(mi.Credits)
		if trimCredits == "" || trimCredits == "x0.00" {
			if s, ok := globalModelCredits[id]; ok {
				mi.Credits = s
			} else if mr := p.measuredRateFor(a.UID, id); mr != "" {
				mi.Credits = mr
			} else if trimCredits == "x0.00" {
				mi.Credits = "免费" // 官方 x0.00 = 不扣费
			}
		}
		// 清理上游可能带的多余修饰词（如 "x0.34 credits"）
		if c := strings.Fields(mi.Credits); len(c) > 1 {
			mi.Credits = c[0]
		}
		// 抽象档位外的真实底座模型 窗口用 seed 兜底
		if mi.ContextWindow <= 0 || mi.MaxTokens <= 0 {
			if seed, ok := globalSeedMeta[id]; ok {
				if mi.ContextWindow <= 0 {
					mi.ContextWindow = seed[0]
				}
				if mi.MaxTokens <= 0 {
					mi.MaxTokens = seed[1]
				}
			}
		}
		if mi.Kind == "" {
			mi.Kind = "chat"
		}
		out = append(out, mi)
	}
	return out, nil
}

// globalSeedMeta 国际版基础模型（真实底座，非抽象档位）的窗口规格。
// 来源：workbuddy2api-panel internal/upstream/model.json（seed 权威数据）。
// /v3/config 不为这些模型下发 credits（倍率缺失、面板显"—"），但窗口尺寸补齐便于参考。
var globalSeedMeta = map[string][2]int64{ // id → [contextLength, maxOutputTokens]
	"hy4-preview":      {1000000, 64000},
	"hy3":              {192000, 64000},
	"gpt-5.6-sol":      {1050000, 128000},
	"gpt-5.6-terra":    {1050000, 128000},
	"gpt-5.6-luna":     {1050000, 128000},
	"gpt-5.5":          {1050000, 128000},
	"gpt-5.4":          {1050000, 128000},
	"gpt-5.3-codex":    {400000, 128000},
	"gemini-3.5-flash": {1048576, 65536},
	"glm-5.3":          {1000000, 131072},
	"glm-5.2":          {1000000, 131072},
	"kimi-k3":          {1048576, 131072},
	"kimi-k2.6":        {256000, 262144},
}

// globalModelCredits 国际版模型的展示倍率（来源：v3/config 官方 x 系数 +
// 实测 usage.credit 反推，glm-5.2 归一 x1.0；免费模型标"免费"）。
// 订阅/后端路由可能有波动，运行期实测值会覆盖此处。
var globalModelCredits = map[string]string{
	// 官方抽象档位（v3/config 直接下发）
	"fast-model":     "x0.35",
	"balanced-model": "x0.81",
	"primary-model":  "x3.31",
	"auto-chat":      "x0.57",
	"nes-1.2":        "x0.57",
	// 真实底座（实测反推，相对 glm-5.2 = x1.0）
	"hy4-preview": "x0.85",
	"glm-5.2":     "x1.0",
	"glm-5.3":     "x1.0",
	"kimi-k3":     "x3.08",
	"kimi-k2.6":   "x0.28",
	// 免费模型（实测 credit=0）
	"hy3": "免费",
}

// kindFromTags 国际版模型 tags → chinacpa 类目（chat 默认 / image / video）。
func kindFromTags(tags []string) string {
	for _, t := range tags {
		switch {
		case strings.Contains(t, "text-to-video"), strings.Contains(t, "image-to-video"):
			return "video"
		case strings.Contains(t, "text-to-image"), strings.Contains(t, "image-to-image"):
			return "image"
		}
	}
	return "chat"
}

// wbPromoZone 优惠时区：上游恒 Asia/Shanghai（UTC+8 无夏令时）。
var wbPromoZone = time.FixedZone("CST", 8*3600)

// wbPromotion /v3/config data.modelPromotions 单条限时优惠。
type wbPromotion struct {
	Enabled  bool     `json:"enabled"`
	ModelIDs []string `json:"modelIds"`
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	Schedule *struct {
		Daily []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"daily"`
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

// activeNow 优惠当前是否生效（validFrom/Until + 每日时段，时段可跨午夜）。
func (pr wbPromotion) activeNow() bool {
	now := time.Now().In(wbPromoZone)
	s := pr.Schedule
	if s == nil {
		return true
	}
	if s.ValidFrom != "" {
		if t, err := time.Parse(time.RFC3339, s.ValidFrom); err == nil && now.Before(t.In(wbPromoZone)) {
			return false
		}
	}
	if s.ValidUntil != "" {
		if t, err := time.Parse(time.RFC3339, s.ValidUntil); err == nil && now.After(t.In(wbPromoZone)) {
			return false
		}
	}
	if len(s.Daily) == 0 {
		return true
	}
	cur := now.Hour()*60 + now.Minute()
	for _, d := range s.Daily {
		st, ok1 := wbPromoMinute(d.Start)
		en, ok2 := wbPromoMinute(d.End)
		if !ok1 || !ok2 {
			continue
		}
		if st <= en {
			if cur >= st && cur < en {
				return true
			}
		} else if cur >= st || cur < en { // 跨午夜（23:00→07:50）
			return true
		}
	}
	return false
}

func wbPromoMinute(hhmm string) (int, bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// fetchV3Credits 拉模型倍率牌价（data.models[].credits，"x0.29"）并叠加当前
// 生效的限时优惠（如 夜间五折 → "x0.29（夜间折扣 0.50x）"）。失败返回空表降级。
func (p *Provider) fetchV3Credits(ctx context.Context, a *model.Account) map[string]string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatBase(a)+"/v3/config", nil)
	if err != nil {
		return nil
	}
	p.commonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0")
	req.Header.Set("X-Product", "SaaS")
	data, err := p.doJSON(req)
	if err != nil {
		return nil
	}
	// doJSON 已剥 {code,msg,data} 信封，这里直接解析内层 data
	var cfg struct {
		Models []struct {
			ID      string `json:"id"`
			Credits string `json:"credits"`
		} `json:"models"`
		ModelPromotions []wbPromotion `json:"modelPromotions"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return nil
	}
	out := map[string]string{}
	for _, m := range cfg.Models {
		if m.ID != "" && m.Credits != "" {
			out[m.ID] = m.Credits
		}
	}
	for _, pr := range cfg.ModelPromotions {
		if !pr.Enabled || !pr.activeNow() {
			continue
		}
		label := ""
		if pr.Badge != nil {
			label = pr.Badge.Label
		}
		eff := ""
		if pr.Discount != nil {
			eff = pr.Discount.DiscountedCredits
		}
		for _, id := range pr.ModelIDs {
			base := out[id]
			switch {
			case base != "" && eff != "" && label != "":
				out[id] = base + "（" + label + " " + eff + "）"
			case base != "" && label != "":
				out[id] = base + "（" + label + "）"
			case eff != "":
				out[id] = eff
			}
		}
	}
	return out
}

// parseModelList 解析 models 数组：字符串形态（新版上游）与对象形态（旧版）。
func parseModelList(raw json.RawMessage) []model.ModelInfo {
	var out []model.ModelInfo
	// 新版：[]string
	var ids []string
	if json.Unmarshal(raw, &ids) == nil {
		for _, id := range ids {
			if id == "" || id == "auto" {
				continue
			}
			out = append(out, model.ModelInfo{ID: id, Provider: "workbuddy"})
		}
		return out
	}
	// 旧版：[]modelEntry
	var objs []modelEntry
	if json.Unmarshal(raw, &objs) == nil {
		for _, m := range objs {
			if m.Disabled || m.NonChatModel || m.ID == "" {
				continue
			}
			out = append(out, model.ModelInfo{
				ID:             m.ID,
				Provider:       "workbuddy",
				Name:           m.Name,
				Description:    m.Description,
				ContextWindow:  m.MaxInputTokens,
				MaxTokens:      m.MaxOutputTokens,
				SupportsTools:  m.SupportsTool,
				SupportsImages: m.SupportsImages,
				Credits:        m.Credits,
			})
		}
	}
	return out
}

// modelEntry 旧版上游模型目录对象形态。
type modelEntry struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"descriptionZh"`
	Credits         string `json:"credits"`
	MaxInputTokens  int64  `json:"maxInputTokens"`
	MaxOutputTokens int64  `json:"maxOutputTokens"`
	Disabled        bool   `json:"disabled"`
	SupportsImages  bool   `json:"supportsImages"`
	SupportsTool    bool   `json:"supportsToolCall"`
	NonChatModel    bool   `json:"nonChatModel"`
}

// ── 对话转发 ─────────────────────────────────────────────

// ChatStream 转发到 /v2/chat/completions。上游 2026 起仅支持流式（11101），
// 故恒以 stream=true 出站；客户端请求非流式时在本地聚合为完整 OpenAI JSON。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	wantStream := false
	var probe struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	if json.Unmarshal(body, &probe) == nil {
		wantStream = probe.Stream
	}
	// 改写：模型名覆盖 + 强制流式 + 注入 prompt_cache_key（命中上游前缀缓存，显著降费）
	body = rewriteChatBody(body, modelName, a.UID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatBase(a)+"/v2/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	p.chatHeaders(req, a)
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
			Err:      classifyHTTP(resp.StatusCode, raw),
		}, nil
	}
	if wantStream {
		// 包装响应流：捕获 usage.credit 反推模型倍率（国际版 usage 带 credit 权威扣费）
		return &provider.ChatResult{
			Body:        p.observeCredits(resp.Body, a, modelName),
			ContentType: "text/event-stream",
		}, nil
	}
	agg, aerr := aggregateOpenAISSE(resp.Body, modelName)
	resp.Body.Close()
	if aerr != nil {
		return nil, aerr
	}
	// 非流式同样捕获（聚合结果含 usage）
	p.applyMeasuredCredit(agg, a, modelName)
	return &provider.ChatResult{
		Body:        io.NopCloser(bytes.NewReader(agg)),
		ContentType: "application/json",
	}, nil
}

// creditObservingReader 包装响应流，逐行捕获 usage.credit 反推模型倍率。
type creditObservingReader struct {
	src       io.Reader
	p         *Provider
	a         *model.Account
	modelName string
	buf       []byte
	closed    bool
	recorded  bool
}

func (p *Provider) observeCredits(src io.ReadCloser, a *model.Account, modelName string) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{&creditObservingReader{src: src, p: p, a: a, modelName: modelName}, src}
}

func (r *creditObservingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.buf = append(r.buf, p[:n]...)
		// 按行扫 usage（OpenAI SSE 简形）：查找带 credit 的 usage 帧
		for {
			i := bytes.IndexByte(r.buf, '\n')
			if i < 0 {
				break
			}
			line := strings.TrimSpace(string(r.buf[:i]))
			r.buf = r.buf[i+1:]
			if !r.recorded && strings.Contains(line, `"credit"`) {
				r.p.applyCreditFromLine(line, r.a, r.modelName)
				r.recorded = true
			}
		}
	}
	return n, err
}

// applyCreditFromLine 从一行 usage SSE 解析 credit 并归一为倍率。
func (p *Provider) applyCreditFromLine(line string, a *model.Account, modelName string) {
	var frame struct {
		Usage struct {
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			Credit           float64 `json:"credit"`
		} `json:"usage"`
	}
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if json.Unmarshal([]byte(payload), &frame) != nil || frame.Usage.Credit <= 0 {
		return
	}
	tokens := frame.Usage.PromptTokens + frame.Usage.CompletionTokens
	if tokens <= 0 {
		return
	}
	// 归一为「相对 glm-5.2(x1) 的倍率」：glm 组基准实测约 0.67credit/8k-token，
	// 这里直接用 credit 绝对值 × 归一因子，得到可展示的 x 系数。
	rate := frame.Usage.Credit / float64(tokens) * 12000 // 每万 token 的 credit → x 系数基准
	p.creditMu.Lock()
	if p.measuredRates[a.UID] == nil {
		p.measuredRates[a.UID] = map[string]string{}
	}
	p.measuredRates[a.UID][modelName] = fmt.Sprintf("x%.2f", rate)
	p.creditMu.Unlock()
}

// applyMeasuredCredit 非流式路径：从聚合 JSON 的 usage 反推倍率。
func (p *Provider) applyMeasuredCredit(agg []byte, a *model.Account, modelName string) {
	var frame struct {
		Usage struct {
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			Credit           float64 `json:"credit"`
		} `json:"usage"`
	}
	if json.Unmarshal(agg, &frame) != nil || frame.Usage.Credit <= 0 {
		return
	}
	tokens := frame.Usage.PromptTokens + frame.Usage.CompletionTokens
	if tokens <= 0 {
		return
	}
	rate := frame.Usage.Credit / float64(tokens) * 12000
	p.creditMu.Lock()
	if p.measuredRates[a.UID] == nil {
		p.measuredRates[a.UID] = map[string]string{}
	}
	p.measuredRates[a.UID][modelName] = fmt.Sprintf("x%.2f", rate)
	p.creditMu.Unlock()
}

// measuredRateFor 取某账号的实测倍率（无则空串）。
func (p *Provider) measuredRateFor(uid, modelName string) string {
	p.creditMu.Lock()
	defer p.creditMu.Unlock()
	if p.measuredRates[uid] == nil {
		return ""
	}
	return p.measuredRates[uid][modelName]
}

// rewriteChatBody 模型名覆盖 + 强制 stream + prompt_cache_key 注入。
func rewriteChatBody(body []byte, modelName, uid string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if modelName != "" {
		obj["model"] = modelName
	}
	obj["stream"] = true
	obj["stream_options"] = map[string]any{"include_usage": true} // 让上游 SSE 带 usage，费用统计才不为 0
	if _, exists := obj["prompt_cache_key"]; !exists {
		obj["prompt_cache_key"] = "chinacpa:" + uid
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// aggregateOpenAISSE 聚合 OpenAI 格式 SSE 流为完整 chat.completion JSON。
func aggregateOpenAISSE(body io.Reader, modelName string) ([]byte, error) {
	out, _, _, err := provider.AggregateOpenAISSE(body, modelName)
	return out, err
}

// oneOf 返回第一个非空串。
func oneOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
