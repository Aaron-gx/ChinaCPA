// Package trae 实现字节 Trae（CN SOLO 免费通道）provider。
// 2026-10 起上游切换到 PKCE 授权码登录流（client ono9krqynydwx5 / auth_from=trae /
// IDE 3.3.62 / 设备密钥证明），登录回调携带 AuthCode，经
// POST /trae/api/v3/oauth/ExchangeToken（PKCE verifier + DeviceInfo）换取令牌。
// 对话经 scene 路由：主 scene chat_v3，回退 solo_work_lite。
// 接口口径移植自 cli2api（internal/providers/trae）与 trae2api-web 的实测结论。
package trae

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

const (
	agentHost   = "https://trae-api-cn.mchost.guru"
	ugHost      = "https://api.trae.cn"
	oauthHost   = "https://api.trae.com.cn"
	consoleHost = "https://www.trae.cn"
	// 国际版预留（trae.ai 家族；上游无公开反代实测，host 可按回调覆盖）
	consoleHostIntl = "https://www.trae.ai"

	clientID       = "ono9krqynydwx5" // PKCE 登录客户端
	legacyClientID = "en1oxy7wnw8j9n" // 旧 refresh token 的签发客户端
	authFrom       = "trae"
	appID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	ideVersion     = "3.3.62"
	ideVersionCode = "20260811"
	deviceBrand    = "83DG"
	osVersion      = "Windows 11 Pro"
	primaryScene   = "chat_v3"        // 主 scene：目录超集
	fallbackScene  = "solo_work_lite" // 回退 scene
	pluginVersion  = "2.3.62834"

	epChat          = "/api/agent/v3/llm_utils_chat"
	epModels        = "/api/ide/v1/get_detail_param"
	epExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	epExchangeCode  = "/trae/api/v3/oauth/ExchangeToken"
	epUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	epCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	epCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	epEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)

// Provider Trae 实现。
type Provider struct {
	http       *http.Client
	streamHTTP *http.Client

	mu       sync.Mutex
	sessions map[string]*loginPending // trace -> 登录会话

	// catalogMu 模型目录缓存（model → scene）
	catalogMu sync.Mutex
	catalog   map[string]string
}

type loginPending struct {
	session    *model.LoginSession
	trace      string
	machine    string
	device     string
	deviceKey  string
	verifier   string
	done       bool
	failed     bool
	message    string
	acct       *model.Account
	createdAt  time.Time
	finishedAt time.Time
}

// purgeStaleLocked 清理过期登录会话（调用方持锁）：未完成 >30 分钟、
// 已完成/失败 >10 分钟。完成态会话必须保留一段时间——回调先到、面板轮询
// 后到，立即删除会让轮询永远读不到「成功」，误报会话过期。
func (p *Provider) purgeStaleLocked() {
	now := time.Now()
	for k, v := range p.sessions {
		if v.finishedAt.IsZero() {
			if now.Sub(v.createdAt) > 30*time.Minute {
				delete(p.sessions, k)
			}
			continue
		}
		if now.Sub(v.finishedAt) > 10*time.Minute {
			delete(p.sessions, k)
		}
	}
}

// New 构造。
func New() *Provider {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Provider{
		http:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		streamHTTP: &http.Client{Transport: tr},
		sessions:   map[string]*loginPending{},
		catalog:    map[string]string{},
	}
}

func (p *Provider) ID() string          { return "trae" }
func (p *Provider) DisplayName() string { return "Trae（字节 SOLO 免费通道）" }

// ── 请求头 ───────────────────────────────────────────────

func clientUA() string { return "Trae/" + ideVersion }

func (p *Provider) soloHeaders(req *http.Request, a *model.Account, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", clientUA())
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
		req.Header.Set("X-Cloudide-Token", a.AccessToken)
		req.Header.Set("X-Ide-Token", a.AccessToken)
	}
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", appID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", ideVersion)
	req.Header.Set("X-Ide-Version-Code", ideVersionCode)
	req.Header.Set("X-App-Version-Code", ideVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", osVersion)
	req.Header.Set("X-Device-Brand", deviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

func (p *Provider) ugHeaders(req *http.Request, a *model.Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA())
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	req.Header.Set("X-User-Region", "CN")
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

func oauthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA())
}

// ── 登录（PKCE 授权码流，2026-10 新流程） ─────────────────

func hex32() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hex16() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// buildLoginURL 构造 TRAE 授权 URL（PKCE S256 + 设备标识，参数集对齐官方 3.3.62 客户端）。
func buildLoginURL(machineID, deviceID, callbackURL, trace, verifier string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", authFrom)
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", pluginVersion)
	v.Set("auth_type", "local")
	v.Set("client_id", clientID)
	v.Set("redirect", "0")
	v.Set("login_trace_id", trace)
	v.Set("auth_callback_url", callbackURL)
	v.Set("machine_id", machineID)
	v.Set("device_id", deviceID)
	v.Set("x_device_id", deviceID)
	v.Set("x_machine_id", machineID)
	v.Set("x_device_brand", "PC")
	v.Set("x_device_type", "PC")
	v.Set("x_os_version", "1.0")
	v.Set("x_env", "")
	v.Set("x_app_version", ideVersion)
	v.Set("x_app_type", "stable")
	if verifier != "" {
		v.Set("code_challenge", pkceChallenge(verifier))
		v.Set("code_challenge_method", "S256")
	}
	return consoleHost + "/authorization?" + v.Encode()
}

// StartLogin 发起 PKCE 登录：生成设备密钥对 + code_verifier，等浏览器回调。
func (p *Provider) StartLogin(ctx context.Context, callbackBase string) (*model.LoginSession, error) {
	machineID, deviceID := hex32(), hex32()
	deviceKey, _, err := generateDeviceKey()
	if err != nil {
		return nil, fmt.Errorf("生成设备密钥失败: %w", err)
	}
	if callbackBase == "" {
		callbackBase = "http://127.0.0.1:8318"
	}
	callbackURL := strings.TrimSuffix(callbackBase, "/") + "/authorize"
	trace := hex16()
	verifier := ""
	if v, _, err := pkcePair(); err == nil {
		verifier = v
	}
	s := &model.LoginSession{
		Provider:    p.ID(),
		ID:          trace,
		URL:         buildLoginURL(machineID, deviceID, callbackURL, trace, verifier),
		CallbackURL: callbackURL,
		State:       trace,
	}
	p.mu.Lock()
	p.purgeStaleLocked()
	p.sessions[trace] = &loginPending{
		session: s, trace: trace, machine: machineID, device: deviceID,
		deviceKey: deviceKey, verifier: verifier, createdAt: time.Now(),
	}
	p.mu.Unlock()
	return s, nil
}

// PollLogin 轮询登录（回调侧完成后这里返回成功）。
func (p *Provider) PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error) {
	p.mu.Lock()
	p.purgeStaleLocked()
	pend := p.sessions[s.State]
	p.mu.Unlock()
	if pend == nil {
		return &model.PollResult{Status: model.LoginFailed, Message: "登录会话已过期（若账号已出现在列表中即纳管成功），请重新发起"}, nil
	}
	if pend.failed {
		return &model.PollResult{Status: model.LoginFailed, Message: pend.message}, nil
	}
	if pend.done && pend.acct != nil {
		return &model.PollResult{Status: model.LoginDone, Account: pend.acct}, nil
	}
	return &model.PollResult{Status: model.LoginPending, Message: "等待浏览器授权完成（成功后自动回调纳管）"}, nil
}

// callbackInfo 回调解析结果。
type callbackInfo struct {
	AuthCode     string
	RefreshToken string
	AccessToken  string
	Trace        string
	APIHost      string
	UID          string
	Nickname     string
	ExpiresAt    int64
}

// parseCallback 解析回调 URL：授权码（含 authCodeInfo 嵌套）、legacy refreshToken、
// userInfo、userJwt、host 区域提示。
func parseCallback(rawURL string) (callbackInfo, error) {
	var info callbackInfo
	u, err := url.Parse(rawURL)
	if err != nil {
		return info, fmt.Errorf("解析回调 URL 失败: %w", err)
	}
	q := u.Query()
	info.AuthCode = firstNonEmpty(q.Get("code"), q.Get("authCode"), q.Get("auth_code"))
	info.RefreshToken = firstNonEmpty(q.Get("refreshToken"), q.Get("refresh_token"))
	info.AccessToken = firstNonEmpty(q.Get("accessToken"))
	info.Trace = firstNonEmpty(q.Get("loginTraceID"), q.Get("login_trace_id"))
	if info.AuthCode == "" {
		if raw := firstNonEmpty(q.Get("authCodeInfo"), q.Get("auth_code_info")); raw != "" {
			var nested struct {
				AuthCode string `json:"AuthCode"`
				Code     string `json:"code"`
			}
			if json.Unmarshal([]byte(raw), &nested) == nil {
				info.AuthCode = firstNonEmpty(nested.AuthCode, nested.Code)
			}
		}
	}
	if host := strings.TrimSpace(q.Get("host")); host != "" &&
		(strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://")) {
		info.APIHost = strings.TrimRight(host, "/")
	}
	if ui := q.Get("userInfo"); ui != "" {
		for i := 0; i < 2; i++ { // 可能双重 URL 编码
			if dec, e := url.QueryUnescape(ui); e == nil && dec != ui {
				ui = dec
			}
		}
		var nested struct {
			UserID     string `json:"UserID"`
			ScreenName string `json:"ScreenName"`
			UID        string `json:"uid"`
			Nickname   string `json:"nickname"`
			TenantID   string `json:"TenantID"`
		}
		if json.Unmarshal([]byte(ui), &nested) == nil {
			info.UID = firstNonEmpty(nested.UserID, nested.UID)
			info.Nickname = firstNonEmpty(nested.ScreenName, nested.Nickname)
			_ = nested.TenantID
		}
	}
	if uj := q.Get("userJwt"); uj != "" {
		var jwt struct {
			Token         string `json:"Token"`
			RefreshToken  string `json:"RefreshToken"`
			TokenExpireAt int64  `json:"TokenExpireAt"`
		}
		if json.Unmarshal([]byte(uj), &jwt) == nil {
			if info.RefreshToken == "" {
				info.RefreshToken = jwt.RefreshToken
			}
			if jwt.Token != "" {
				info.AccessToken = jwt.Token
			}
			if jwt.TokenExpireAt != 0 {
				info.ExpiresAt = normExpires(jwt.TokenExpireAt)
			}
		}
	}
	if info.AuthCode == "" && info.RefreshToken == "" && info.AccessToken == "" {
		return info, fmt.Errorf("回调缺少授权码 / refreshToken / userJwt")
	}
	return info, nil
}

// HandleCallback 处理 /trae/callback：优先 PKCE 授权码兑换，回退 legacy refresh。
func (p *Provider) HandleCallback(q url.Values) (*model.PollResult, error) {
	rawURL := ""
	if len(q) > 0 {
		rawURL = "?"
	}
	_ = rawURL
	// 直接用 Values 重建解析（复用 parseCallback 逻辑）
	fake := url.Values{}
	for k, v := range q {
		for _, item := range v {
			fake.Set(k, item)
		}
	}
	info, err := parseCallback("/callback?" + fake.Encode())
	if err != nil {
		return nil, err
	}
	// 找到 pending（trace 匹配优先，否则最早的）
	p.mu.Lock()
	var pend *loginPending
	if info.Trace != "" {
		pend = p.sessions[info.Trace]
	}
	if pend == nil {
		for _, v := range p.sessions {
			pend = v
			break
		}
	}
	p.mu.Unlock()
	a := &model.Account{Provider: p.ID(), UID: info.UID, Nickname: info.Nickname, AuthHost: info.APIHost}
	if pend != nil {
		a.MachineID, a.DeviceID, a.DevicePrivateKey = pend.machine, pend.device, pend.deviceKey
	}
	if a.MachineID == "" {
		a.MachineID, a.DeviceID = hex32(), hex32()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	switch {
	case info.AuthCode != "":
		a.RefreshClientID = clientID
		if _, err := p.exchangeAuthCode(ctx, a, info.AuthCode, pendVerifier(pend)); err != nil {
			p.failPending(pend, err.Error())
			return nil, fmt.Errorf("授权码兑换失败: %w", err)
		}
	case info.RefreshToken != "":
		a.AccessToken = info.AccessToken
		a.RefreshToken = info.RefreshToken
		a.ExpiresAt = info.ExpiresAt
		a.RefreshClientID = legacyClientID
		if _, err := p.refreshTokenExchange(ctx, a, true); err != nil {
			p.failPending(pend, err.Error())
			return nil, fmt.Errorf("令牌交换失败: %w", err)
		}
	default:
		a.AccessToken = info.AccessToken
		a.ExpiresAt = info.ExpiresAt
	}
	if a.UID == "" {
		if uid, nick, _, err := p.getUserInfo(ctx, a); err == nil {
			a.UID, a.Nickname = uid, nick
		}
	}
	if a.UID == "" {
		a.UID = "trae-" + hex16()
	}
	if pend != nil {
		p.mu.Lock()
		pend.done, pend.acct, pend.finishedAt = true, a, time.Now()
		p.mu.Unlock()
	}
	return &model.PollResult{Status: model.LoginDone, Account: a}, nil
}

func pendVerifier(p *loginPending) string {
	if p == nil {
		return ""
	}
	return p.verifier
}

func (p *Provider) failPending(pend *loginPending, msg string) {
	if pend == nil {
		return
	}
	p.mu.Lock()
	pend.failed, pend.message, pend.finishedAt = true, msg, time.Now()
	p.mu.Unlock()
}

// exchangeAuthCode PKCE 授权码兑换（DeviceInfo + DevicePublicKey）。
func (p *Provider) exchangeAuthCode(ctx context.Context, a *model.Account, authCode, verifier string) (bool, error) {
	deviceInfo := map[string]any{
		"DeviceID":      a.DeviceID,
		"MachineID":     a.MachineID,
		"PlatformCode":  "IDE_PC",
		"DeviceType":    "PC",
		"DeviceName":    "ChinaCPA",
		"ClientVersion": ideVersion,
		"OSInfo":        "windows",
		"OSVersion":     osVersion,
		"DeviceBrand":   deviceBrand,
	}
	if pub := devicePublicKeyPEM(a.DevicePrivateKey); pub != "" {
		deviceInfo["DevicePublicKey"] = pub
	}
	body, _ := json.Marshal(map[string]any{
		"ClientID":     clientID,
		"AuthCode":     authCode,
		"CodeVerifier": verifier,
		"IDEVersion":   ideVersion,
		"DeviceInfo":   deviceInfo,
	})
	// 主 host：api.trae.cn；回调带 host 时作为回退
	hosts := []string{ugHost}
	if a.AuthHost != "" && a.AuthHost != ugHost {
		hosts = append(hosts, a.AuthHost)
	}
	var lastErr error
	for _, host := range hosts {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+epExchangeCode, bytes.NewReader(body))
		if err != nil {
			return false, err
		}
		oauthHeaders(req)
		data, err := p.doJSON(req)
		if err == nil {
			if err := applyExchangeResult(a, data); err != nil {
				return false, err
			}
			return true, nil
		}
		lastErr = err
	}
	return false, lastErr
}

// refreshTokenExchange legacy/常规 refresh token 交换（ExchangeToken）。
func (p *Provider) refreshTokenExchange(ctx context.Context, a *model.Account, force bool) (bool, error) {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return false, nil
	}
	if !force && a.ExpiresAt > 0 && time.Now().Add(10*time.Minute).Unix() < a.ExpiresAt && a.AccessToken != "" {
		return false, nil
	}
	host := oauthHost
	if a.AuthHost != "" {
		host = a.AuthHost
	}
	cid := a.RefreshClientID
	if cid == "" {
		cid = legacyClientID
	}
	body, _ := json.Marshal(map[string]any{
		"ClientID":     cid,
		"RefreshToken": a.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+epExchange, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	oauthHeaders(req)
	data, err := p.doJSON(req)
	if err != nil {
		return false, err
	}
	if err := applyExchangeResult(a, data); err != nil {
		return false, err
	}
	return true, nil
}

// applyExchangeResult 应用 ExchangeToken/授权码兑换响应。
func applyExchangeResult(a *model.Account, data json.RawMessage) error {
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("兑换响应解析失败: %w", err)
	}
	if strings.TrimSpace(resp.Result.Token) == "" {
		return fmt.Errorf("兑换响应无 token（可能授权码过期或重复使用），请重新登录")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	if resp.Result.TokenExpireAt != 0 {
		a.ExpiresAt = normExpires(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

func normExpires(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ImportCredential 支持粘贴 refreshToken（或 JSON：refresh_token/device 信息），
// 前缀 global: 切国际版 host。
func (p *Provider) ImportCredential(ctx context.Context, raw string) (*model.Account, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("凭证为空")
	}
	a := &model.Account{Provider: p.ID(), RefreshClientID: legacyClientID}
	region := ""
	if strings.HasPrefix(strings.ToLower(raw), "global:") || strings.HasPrefix(strings.ToLower(raw), "intl:") {
		region = "global"
		raw = raw[strings.Index(raw, ":")+1:]
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %v", err)
		}
		str := func(keys ...string) string {
			for _, k := range keys {
				if v, ok := m[k].(string); ok && v != "" {
					return v
				}
			}
			return ""
		}
		a.AccessToken = str("access_token", "accessToken", "Token")
		a.RefreshToken = str("refresh_token", "refreshToken", "RefreshToken")
		a.UID = str("uid", "userId", "UserId")
		a.Nickname = str("nickname", "nickName")
		a.MachineID = str("machine_id", "machineId")
		a.DeviceID = str("device_id", "deviceId")
		if r := str("region", "realm"); r != "" {
			region = r
		}
	} else {
		a.RefreshToken = raw
	}
	if region == "global" || strings.EqualFold(a.Realm, "global") {
		a.Realm = "global"
		if a.AuthHost == "" {
			a.AuthHost = ugHost // 结构预留；国际版 host 以后续实测为准
		}
	}
	if a.AccessToken == "" && a.RefreshToken == "" {
		return nil, fmt.Errorf("凭证中未找到 refreshToken")
	}
	if a.MachineID == "" {
		a.MachineID, a.DeviceID = hex32(), hex32()
	}
	if _, err := p.refreshTokenExchange(ctx, a, a.AccessToken == ""); err != nil && a.AccessToken == "" {
		return nil, err
	}
	if a.UID == "" {
		if uid, nick, _, err := p.getUserInfo(ctx, a); err == nil {
			a.UID, a.Nickname = uid, nick
		}
	}
	if a.UID == "" {
		a.UID = "trae-" + hex16()
	}
	return a, nil
}

// RefreshToken 临期刷新（绑定签发客户端）。
func (p *Provider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
	return p.refreshTokenExchange(ctx, a, false)
}

func (p *Provider) getUserInfo(ctx context.Context, a *model.Account) (uid, nickname, enterpriseID string, err error) {
	host := oauthHost
	if a.AuthHost != "" {
		host = a.AuthHost
	}
	body, _ := json.Marshal(map[string]any{"ReqSource": "IDE", "IDEVersion": ideVersion})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+epUserInfo, bytes.NewReader(body))
	if err != nil {
		return "", "", "", err
	}
	oauthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.AccessToken)
	data, err := p.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserId    string `json:"UserId"`
			UserIdStr string `json:"userId"`
			NickName  string `json:"NickName"`
			EntID     string `json:"EnterpriseId"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo 解析失败: %w", err)
	}
	return pick(resp.Result.UserId, resp.Result.UserIdStr), resp.Result.NickName, resp.Result.EntID, nil
}

// ── 额度查询 ─────────────────────────────────────────────

// Quota 聚合权益包额度（同旧实现）。
func (p *Provider) Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ugHost+epEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	p.ugHeaders(req, a)
	data, err := p.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("额度解析失败: %w", err)
	}
	var remain, limit, used int64
	packs := 0
	for _, pk := range resp.UserEntitlementPackList {
		l := pk.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		u := int64(pk.Usage.CreditsAmount)
		limit += l
		used += u
		remain += l - u
		packs++
	}
	// 对话流实时上报的 ide_credits 更准，优先展示
	if ide, _, ok := p.LastIdeCredits(a.UID); ok {
		remain = int64(ide)
	}
	return &model.QuotaInfo{
		Provider:  p.ID(),
		UID:       a.UID,
		Used:      float64(used),
		Limit:     float64(limit),
		Remaining: float64(remain),
		Unit:      "ide_credits",
		Healthy:   remain > 0,
		Detail:    map[string]any{"packs": packs},
	}, nil
}

// ReportIdeCredits 对话流 notify_usage 实时上报（SOLO 通道真实余额）。
func (p *Provider) ReportIdeCredits(uid string, ideCredits, workCredits float64) {
	ideReportMu.Lock()
	ideReport[uid] = ideReportEntry{ts: time.Now().Unix(), ide: ideCredits, work: workCredits}
	ideReportMu.Unlock()
}

var (
	ideReportMu sync.Mutex
	ideReport   = map[string]ideReportEntry{}
)

type ideReportEntry struct {
	ts   int64
	ide  float64
	work float64
}

// LastIdeCredits 最近一次对话流上报。
func (p *Provider) LastIdeCredits(uid string) (ide, work float64, ok bool) {
	ideReportMu.Lock()
	defer ideReportMu.Unlock()
	e, exists := ideReport[uid]
	return e.ide, e.work, exists && time.Now().Unix()-e.ts < 86400
}

// ── 签到（每日 +200 ide_credits） ────────────────────────

func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ugHost+epCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	p.ugHeaders(req, a)
	data, err := p.doJSON(req)
	if err != nil {
		return nil, err
	}
	var st struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("签到状态解析失败: %w", err)
	}
	if st.CheckedIn {
		return &model.CheckinResult{OK: true, Already: true, Message: "今天已签到"}, nil
	}
	creq, err := http.NewRequestWithContext(ctx, http.MethodPost, ugHost+epCheckinClaim, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	p.ugHeaders(creq, a)
	cdata, err := p.doJSON(creq)
	if err != nil {
		pe := provider.AsError(err)
		if strings.Contains(pe.Msg, "9074") || strings.Contains(pe.Msg, "人数太多") {
			return &model.CheckinResult{OK: false, Message: "签到人数过多（9074），稍后自动重试"}, nil
		}
		return nil, err
	}
	_ = cdata
	return &model.CheckinResult{OK: true, Message: "签到成功", CreditsEarned: 200}, nil
}

// ── 模型列表（双 scene 合并） ────────────────────────────

// Models 拉取 chat_v3（主）+ solo_work_lite（回退）两个 scene 的模型表并合并。
func (p *Provider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	seen := map[string]bool{}
	var out []model.ModelInfo
	scenes := map[string]string{}
	for _, scene := range []string{primaryScene, fallbackScene} {
		list, err := p.fetchScene(ctx, a, scene)
		if err != nil {
			continue
		}
		for _, m := range list {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
			scenes[m.ID] = scene
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("模型表为空（两个 scene 均失败）")
	}
	p.catalogMu.Lock()
	if p.catalog == nil {
		p.catalog = map[string]string{}
	}
	for k, v := range scenes {
		p.catalog[k] = v
	}
	p.catalogMu.Unlock()
	return out, nil
}

func (p *Provider) fetchScene(ctx context.Context, a *model.Account, scene string) ([]model.ModelInfo, error) {
	body, _ := json.Marshal(map[string]any{
		"function":            scene,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentHost+epModels, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	p.soloHeaders(req, a, false)
	data, err := p.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("模型表解析失败: %w", err)
	}
	var out []model.ModelInfo
	for _, cfg := range resp.ConfigInfoList {
		if cfg.ConfigName == "" {
			continue
		}
		out = append(out, model.ModelInfo{ID: cfg.ConfigName, Name: cfg.DisplayConfig.DisplayName, Provider: p.ID()})
	}
	return out, nil
}

// sceneFor 模型 → scene（目录缓存，缺省 chat_v3）。
func (p *Provider) sceneFor(modelName string) string {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	if p.catalog == nil {
		p.catalog = map[string]string{}
	}
	if s, ok := p.catalog[modelName]; ok && s != "" {
		return s
	}
	return primaryScene
}

// ── 对话转发（OpenAI → SOLO，SSE 双向转换） ───────────────

// prepareBody OpenAI → SOLO 请求体改写（function 用 scene）。
func prepareBody(src []byte, modelName, scene string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = scene
	obj["config_name"] = modelName
	obj["model"] = modelName
	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			if c, ok := m["content"].(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": c}}
			}
			if role == "assistant" {
				if tcs, ok := m["tool_calls"].([]any); ok {
					for _, tci := range tcs {
						tc, ok := tci.(map[string]any)
						if !ok {
							continue
						}
						if fn, ok := tc["function"].(map[string]any); ok {
							tc["function_call"] = fn
							delete(tc, "function")
						}
					}
				}
			}
		}
	}
	if tc, ok := obj["tool_choice"].(string); ok && tc == "none" {
		delete(obj, "tools")
		delete(obj, "tool_choice")
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// ChatStream 转发对话：恒流式出站，按客户端要求输出 SSE 或聚合 JSON。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	wantStream := false
	var probe struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &probe) == nil {
		wantStream = probe.Stream
	}
	prepared := prepareBody(body, modelName, p.sceneFor(modelName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentHost+epChat, bytes.NewReader(prepared))
	if err != nil {
		return nil, err
	}
	p.soloHeaders(req, a, true)
	resp, err := p.streamHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return &provider.ChatResult{Status: resp.StatusCode, RespBody: raw, Err: classify(resp.StatusCode, raw)}, nil
	}
	if wantStream {
		return &provider.ChatResult{
			Body:        newSoloToOpenAIStream(resp.Body, a.UID, p, modelName),
			ContentType: "text/event-stream",
		}, nil
	}
	agg, err := aggregateSolo(resp.Body, a.UID, p, modelName)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	return &provider.ChatResult{
		Body:        io.NopCloser(bytes.NewReader(agg)),
		ContentType: "application/json",
	}, nil
}

// classify 错误分类（HTTP 状态 + body 业务码）。
func classify(status int, raw []byte) *provider.Error {
	msg := truncate(string(raw), 200)
	kind := provider.ErrClient
	switch {
	case status == http.StatusTooManyRequests:
		kind = provider.ErrRate
	case status == http.StatusUnauthorized:
		kind = provider.ErrAuth
	case status >= 500:
		kind = provider.ErrUpstream
	}
	for _, c := range []struct {
		code int64
		kind provider.ErrKind
		text string
	}{
		{1001, provider.ErrAuth, "认证失败"},
		{1005, provider.ErrQuota, "plan 权益不足"},
		{4008, provider.ErrQuota, "配额超限"},
		{4011, provider.ErrRate, "请求频率超限"},
	} {
		if strings.Contains(msg, strconv.FormatInt(c.code, 10)) {
			return &provider.Error{Kind: c.kind, Status: status, Code: strconv.FormatInt(c.code, 10), Msg: c.text + ": " + msg}
		}
	}
	return &provider.Error{Kind: kind, Status: status, Msg: msg}
}

// ── 工具 ─────────────────────────────────────────────────

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
		return nil, classify(resp.StatusCode, raw)
	}
	return raw, nil
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
