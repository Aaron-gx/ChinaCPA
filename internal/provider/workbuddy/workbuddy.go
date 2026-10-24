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
	chatBaseCN        = "https://copilot.tencent.com"
	billingBaseCN     = "https://www.codebuddy.cn"
	webBaseCN         = "https://www.workbuddy.cn"
	globalBase        = "https://www.workbuddy.ai"
	clientVersion     = "5.5.4"
	cliVersion        = "2.137.1"
	originCN          = "https://www.codebuddy.cn"
	originGlobal      = "https://www.workbuddy.ai"
)

// Provider WorkBuddy 实现。
type Provider struct {
	http *http.Client
	// loginSessions 面板登录会话（state -> session）
	mu      sync.Mutex
	sessions map[string]*model.LoginSession
}

// New 构造。
func New() *Provider {
	return &Provider{
		http:     &http.Client{Timeout: 60 * time.Second},
		sessions: map[string]*model.LoginSession{},
	}
}

func (p *Provider) ID() string          { return "workbuddy" }
func (p *Provider) DisplayName() string { return "WorkBuddy / CodeBuddy" }

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
	if isGlobal(a) {
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
	base := chatBaseCN
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
		s.URL = webBaseCN + "/login?state=" + st.State
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
	base := chatBaseCN
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
		Provider:      p.ID(),
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
		Domain:        tok.Domain,
		ExpiresAt:     time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
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

// Models 拉取模型列表（/console/enterprises/personal/models → agents[cli].models）。
func (p *Provider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
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
		Agents map[string]struct {
			Models []modelEntry `json:"models"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}
	var out []model.ModelInfo
	for agent, g := range resp.Agents {
		if agent != "cli" {
			continue
		}
		for _, m := range g.Models {
			if m.Disabled || m.NonChatModel || m.ID == "" {
				continue
			}
			out = append(out, model.ModelInfo{
				ID:             m.ID,
				Provider:       p.ID(),
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
	if len(out) == 0 {
		return nil, fmt.Errorf("模型列表为空")
	}
	return out, nil
}

// ── 对话转发 ─────────────────────────────────────────────

// ChatStream 透传 OpenAI 兼容请求到 /v2/chat/completions（SSE 原样透传）。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	// 注入 prompt_cache_key（命中上游前缀缓存，显著降费）
	body = injectCacheKey(body, a.UID)
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
	ct := resp.Header.Get("Content-Type")
	return &provider.ChatResult{Body: resp.Body, ContentType: ct}, nil
}

// injectCacheKey 注入按账号隔离的稳定缓存键（费用优化）。
func injectCacheKey(body []byte, uid string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, exists := obj["prompt_cache_key"]; exists {
		return body
	}
	obj["prompt_cache_key"] = "chinacpa:" + uid
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
