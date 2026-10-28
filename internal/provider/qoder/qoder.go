// Package qoder 实现阿里 Qoder（CN qoder.com.cn / 国际 qoder.sh）provider。
// 凭证为 PAT（控制台创建），经 COSY 协议（自定义 base64 + RSA + AES + MD5 签名）
// 换取会话；协议层整体移植自 cli2api / Qoder-2API-Go（internal/provider/qoder/qauth）。
//
// 能力：PAT 导入（双区）、每日签到（campaigns 领取）、额度（user/status + OpenAPI
// quota）、模型目录（model/list）、对话（agent_chat_generation SSE → OpenAI）。
package qoder

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
	"chinacpa/internal/provider/qoder/qauth"
)

const campaignsPath = "/sash/api/v1/me/campaigns"

// OAuth 设备授权端点（挂 openapi 基址，纯 Bearer，无 COSY 签名）。
const (
	pathDevicePoll  = "/api/v1/deviceToken/poll"
	pathDeviceRefsh = "/api/v1/deviceToken/refresh"
	pathUserinfo    = "/api/v1/userinfo"
)

// realmCfg 双区域 OAuth 配置（口径：qoder2api-hub REALM_CONFIGS 官方逆向）。
type realmCfg struct {
	website      string
	clientID     string
	redirectURI  string
	sendRedirect bool
	nonceDashed  bool
	ua           string
}

var (
	realmCN = realmCfg{
		website: "https://qoder.com.cn", clientID: "1c5e33e1-364d-4ce6-b02c-acaa81274a5c",
		redirectURI: "qoder-work-cn://", sendRedirect: true, nonceDashed: true,
		ua: "QoderWork/1.1.75",
	}
	realmIntl = realmCfg{
		website: "https://qoder.com", clientID: "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
		redirectURI: "qoder://aicoding.aicoding-agent/login-success", sendRedirect: false,
		nonceDashed: false, ua: "Qoder/1.1.75",
	}
)

// oauthPending 一次设备授权会话。
type oauthPending struct {
	state    string
	verifier string
	nonce    string
	created  time.Time
}

// Provider Qoder 实现（realm: "cn"/"intl" → provider id qoder / qoder-intl）。
type Provider struct {
	http  *http.Client
	realm string
	id    string

	mu      sync.Mutex
	bridges map[string]*bridge // uid -> 会话桥
	logins  map[string]*oauthPending
}

// bridge 单账号会话桥：PAT → jobToken 会话 + 刷新轮换；OAuth → 直连身份会话。
type bridge struct {
	pat    string
	region *qauth.RegionConfig
	source string // "pat" | "oauth"
	acct   *model.Account

	machineID, machineToken, machineType, machineCode string

	sess          *qauth.SessionContext
	identity      *qauth.AuthIdentity
	refreshToken  string
	securityOauth string
	expireMs      int64

	openapiMu        sync.Mutex
	openapiBearer    string
	openapiFetchedAt time.Time
}

// New 构造（realm "cn" 或 "intl"）。
func New(realm string) *Provider {
	if realm != "intl" {
		realm = "cn"
	}
	id := "qoder"
	if realm == "intl" {
		id = "qoder-intl"
	}
	return &Provider{
		http:    &http.Client{Timeout: 120 * time.Second},
		realm:   realm,
		id:      id,
		bridges: map[string]*bridge{},
		logins:  map[string]*oauthPending{},
	}
}

func (p *Provider) ID() string { return p.id }
func (p *Provider) DisplayName() string {
	if p.realm == "intl" {
		return "Qoder 国际版（qoder.com）"
	}
	return "Qoder 国内版（qoder.com.cn）"
}

// realmConfig 当前区域 OAuth 配置。
func (p *Provider) realmConfig() realmCfg {
	if p.realm == "intl" {
		return realmIntl
	}
	return realmCN
}

// ── 设备指纹（PAT 派生，跨重启稳定） ─────────────────────

func deriveMachineIdentity(pat string) (id, token, typ, code string) {
	idSum := md5.Sum([]byte("machine:" + pat))
	id = hex.EncodeToString(idSum[:])
	tokSum := sha512.Sum512([]byte("machinetoken:" + pat))
	token = strings.TrimRight(base64.RawURLEncoding.EncodeToString(tokSum[:]), "=")[:43]
	typeSum := md5.Sum([]byte("machinetype:" + pat))
	typeHex := strings.ReplaceAll(hex.EncodeToString(typeSum[:]), "-", "")
	typ = typeHex
	if len(typ) > 18 {
		typ = typ[:18]
	}
	codeSum := md5.Sum([]byte("machinecode:" + pat))
	code = hex.EncodeToString(codeSum[:])
	return
}

func machineOSString() string {
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	return arch + "_" + runtime.GOOS
}

func machineHostnameString() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return ""
	}
	if len(name) > 96 {
		return ""
	}
	return name
}

// ── 会话管理 ─────────────────────────────────────────────

func (p *Provider) bridgeOf(a *model.Account) *bridge {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b, ok := p.bridges[a.UID]; ok {
		if a.Source == "oauth" && b.source == "oauth" && b.acct == a {
			return b
		}
		if a.Source != "oauth" && b.source != "oauth" && b.pat == a.RefreshToken {
			return b
		}
	}
	region := qauth.RegionByName(a.Realm)
	if region.Name == "global" {
		// 官方推理主机候选：api3 主选（qoder2api-hub 实测）
		cp := *region
		cp.ChatBase = "https://api3.qoder.sh"
		region = &cp
	}
	if a.Source == "oauth" {
		id, token, typ := uidMachineIdentity(a.UID)
		codeSum := md5.Sum([]byte("machinecode:" + a.UID))
		identity := qauth.AuthIdentity{
			Name: a.Nickname, Aid: a.UID, UID: a.UID,
			UserType:           "personal_professional_trial",
			SecurityOauthToken: a.AccessToken,
			RefreshToken:       a.RefreshToken,
		}
		b := &bridge{
			region: region, source: "oauth", acct: a,
			machineID: id, machineToken: token, machineType: typ,
			machineCode:   hex.EncodeToString(codeSum[:]),
			identity:      &identity,
			securityOauth: a.AccessToken, refreshToken: a.RefreshToken,
			expireMs: a.ExpiresAt * 1000,
		}
		b.sess = qauth.NewSession(identity, id, token, typ)
		p.bridges[a.UID] = b
		return b
	}
	id, token, typ, code := deriveMachineIdentity(a.RefreshToken)
	b := &bridge{
		pat: a.RefreshToken, region: region, source: "pat", acct: a,
		machineID: id, machineToken: token, machineType: typ, machineCode: code,
	}
	p.bridges[a.UID] = b
	return b
}

// ensureFresh 确保会话可用（临期用 RefreshJobToken 轮换，失败回退 PAT 重换）。
func (p *Provider) ensureFresh(ctx context.Context, b *bridge) error {
	nowMs := time.Now().UnixMilli()
	if b.sess != nil && b.expireMs > 0 && nowMs < b.expireMs-5*60*1000 {
		return nil
	}
	if b.source == "oauth" {
		// OAuth 账号：deviceToken/refresh 轮换（在 RefreshToken() 中处理）
		return nil
	}
	var jt map[string]interface{}
	var err error
	if b.refreshToken != "" {
		jt, err = qauth.RefreshJobToken(ctx, b.pat, b.refreshToken, b.securityOauth, b.machineID, b.machineToken, b.machineType, b.region)
	}
	if err != nil || jt == nil {
		jt, err = qauth.ExchangeJobToken(ctx, b.pat, b.machineID, b.machineToken, b.machineType, b.region)
		if err != nil {
			return fmt.Errorf("Qoder 会话获取失败（PAT 可能失效）: %w", err)
		}
	}
	applyJobToken(b, jt)
	return nil
}

// applyJobToken 由 jobToken 响应构建签名会话。
func applyJobToken(b *bridge, jt map[string]interface{}) {
	name, _ := jt["name"].(string)
	id, _ := jt["id"].(string)
	securityOauth, _ := jt["securityOauthToken"].(string)
	refreshToken, _ := jt["refreshToken"].(string)
	if securityOauth == "" {
		securityOauth = b.securityOauth
	}
	if refreshToken == "" {
		refreshToken = b.refreshToken
	}
	identity := qauth.AuthIdentity{
		Name: name, Aid: id, UID: id,
		UserType:           "personal_standard",
		SecurityOauthToken: securityOauth,
		RefreshToken:       refreshToken,
	}
	b.sess = qauth.NewSession(identity, b.machineID, b.machineToken, b.machineType)
	b.identity = &identity
	b.securityOauth = securityOauth
	b.refreshToken = refreshToken
	if exp := toInt64Any(jt["expireTime"]); exp > 0 {
		b.expireMs = exp
	} else {
		b.expireMs = time.Now().Add(2 * time.Hour).UnixMilli()
	}
}

func toInt64Any(v interface{}) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

// ── 登录 / 凭证导入 ──────────────────────────────────────

// StartLogin 发起 OAuth 设备授权（PKCE S256 + nonce，浏览器打开授权页）。
// 双区 URL 差异（官方逆向）：CN 带 redirect_uri + 带横线 nonce；国际版不带
// redirect_uri + 32-hex nonce；两区都带 client_id + machine_id。
func (p *Provider) StartLogin(ctx context.Context, callbackBase string) (*model.LoginSession, error) {
	cfg := p.realmConfig()
	verifier := pkceVerifierB64()
	nonce := pkceNonce(cfg.nonceDashed)
	q := url.Values{}
	q.Set("challenge", pkceChallengeB64(verifier))
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	if cfg.sendRedirect {
		q.Set("redirect_uri", cfg.redirectURI)
	}
	q.Set("client_id", cfg.clientID)
	q.Set("machine_id", stableMachineID(p.realm))
	state := "qd-" + fmt.Sprintf("%d", time.Now().UnixNano())
	p.mu.Lock()
	p.logins[state] = &oauthPending{state: state, verifier: verifier, nonce: nonce, created: time.Now()}
	p.mu.Unlock()
	return &model.LoginSession{
		Provider: p.ID(),
		ID:       state,
		URL:      cfg.website + "/device/selectAccounts?" + q.Encode(),
	}, nil
}

// PollLogin 轮询设备授权（404/202 = 等待用户授权）。
func (p *Provider) PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error) {
	p.mu.Lock()
	pend := p.logins[s.ID]
	p.mu.Unlock()
	if pend == nil {
		return &model.PollResult{Status: model.LoginFailed, Message: "登录会话不存在或已过期，请重新发起"}, nil
	}
	if time.Since(pend.created) > 10*time.Minute {
		p.mu.Lock()
		delete(p.logins, s.ID)
		p.mu.Unlock()
		return &model.PollResult{Status: model.LoginFailed, Message: "授权窗口已过期，请重新发起"}, nil
	}
	region := qauth.RegionByName(p.realm)
	cfg := p.realmConfig()
	pollQ := url.Values{}
	pollQ.Set("nonce", pend.nonce)
	pollQ.Set("verifier", pend.verifier)
	pollQ.Set("challenge_method", "S256")
	pollURL := region.OpenAPIBase + pathDevicePoll + "?" + pollQ.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "QoderWork")
	resp, err := p.http.Do(req)
	if err != nil {
		return &model.PollResult{Status: model.LoginPending, Message: "网络波动，继续等待授权…"}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 404 || resp.StatusCode == 202 {
		return &model.PollResult{Status: model.LoginPending, Message: "等待浏览器完成 Qoder 设备授权"}, nil
	}
	if resp.StatusCode >= 400 {
		return &model.PollResult{Status: model.LoginFailed, Message: fmt.Sprintf("授权轮询失败 HTTP %d", resp.StatusCode)}, nil
	}
	var data struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		ExpiresIn    int64  `json:"expires_in"`
		ExpiresAt    string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &data); err != nil || (data.Token == "" && data.DeviceToken == "") {
		return &model.PollResult{Status: model.LoginPending, Message: "等待授权完成"}, nil
	}
	token := data.Token
	if token == "" {
		token = data.DeviceToken
	}
	// userinfo 补全昵称（尽力而为）
	uid, nickname := data.UserID, ""
	if uireq, err := http.NewRequestWithContext(ctx, http.MethodGet, region.OpenAPIBase+pathUserinfo, nil); err == nil {
		uireq.Header.Set("Accept", "application/json")
		uireq.Header.Set("User-Agent", cfg.ua)
		uireq.Header.Set("Authorization", "Bearer "+token)
		if uiresp, err := p.http.Do(uireq); err == nil {
			uiraw, _ := io.ReadAll(io.LimitReader(uiresp.Body, 1<<20))
			uiresp.Body.Close()
			var ui struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if json.Unmarshal(uiraw, &ui) == nil {
				if ui.ID != "" {
					uid = ui.ID
				}
				nickname = ui.Name
			}
		}
	}
	if uid == "" {
		uid = "q-" + pend.nonce[:16]
	}
	expiresAt := time.Now().Add(30 * 24 * time.Hour).Unix()
	if data.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(data.ExpiresIn) * time.Millisecond).Unix()
	} else if data.ExpiresAt != "" {
		if t, err := time.Parse("2006-01-02T15:04:05", data.ExpiresAt[:min19(data.ExpiresAt)]); err == nil {
			expiresAt = t.Unix()
		}
	}
	a := &model.Account{
		Provider:     p.ID(),
		UID:          uid,
		Nickname:     nickname,
		Realm:        p.realm,
		Source:       "oauth",
		AccessToken:  token,
		RefreshToken: data.RefreshToken,
		ExpiresAt:    expiresAt,
	}
	p.mu.Lock()
	delete(p.logins, s.ID)
	p.mu.Unlock()
	return &model.PollResult{Status: model.LoginDone, Account: a}, nil
}

func min19(s string) int {
	if len(s) >= 19 {
		return 19
	}
	return len(s)
}

// ── PKCE / 设备指纹小工具 ──

func pkceVerifierB64() string {
	b := make([]byte, 32)
	_, _ = randRead(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func pkceChallengeB64(verifier string) string {
	sum := sha256Sum([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum)
}

func pkceNonce(dashed bool) string {
	if dashed {
		return uuidNewString()
	}
	return uuidNewHexString()
}

// stableMachineID 区域内稳定的虚拟设备 ID（OAuth 授权 URL 用）。
func stableMachineID(realm string) string {
	sum := md5.Sum([]byte("qoder-machine:" + realm))
	return hex.EncodeToString(sum[:16])
}

// ImportCredential 粘贴 PAT 导入。支持：
//   - 裸 PAT（默认 CN 区）
//   - 前缀选择区域："cn:PAT" / "global:PAT"
//   - JSON：{"api_key"/"pat": "...", "region": "cn"|"global"}
func (p *Provider) ImportCredential(ctx context.Context, raw string) (*model.Account, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("凭证为空")
	}
	pat := raw
	region := "cn"
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "global:") || strings.HasPrefix(lower, "intl:") {
		region = "global"
		pat = strings.TrimSpace(raw[strings.Index(raw, ":")+1:])
	} else if strings.HasPrefix(lower, "cn:") {
		region = "cn"
		pat = strings.TrimSpace(raw[strings.Index(raw, ":")+1:])
	} else if strings.HasPrefix(raw, "{") {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %v", err)
		}
		if v, ok := m["api_key"].(string); ok && v != "" {
			pat = v
		} else if v, ok := m["pat"].(string); ok && v != "" {
			pat = v
		}
		if v, ok := m["region"].(string); ok && v != "" {
			region = v
		}
	}
	if len(pat) < 16 || len(pat) > 512 {
		return nil, fmt.Errorf("PAT 长度不合法（在 Qoder 控制台 → 设置 → API Token 创建）")
	}
	id, token, typ, code := deriveMachineIdentity(pat)
	b := &bridge{
		pat: pat, region: qauth.RegionByName(region),
		machineID: id, machineToken: token, machineType: typ, machineCode: code,
	}
	jt, err := qauth.ExchangeJobToken(ctx, pat, id, token, typ, b.region)
	if err != nil {
		return nil, fmt.Errorf("PAT 校验失败: %w", err)
	}
	applyJobToken(b, jt)
	a := &model.Account{
		Provider: p.ID(),
		UID:      b.identity.UID,
		Nickname: b.identity.Name,
		Realm:    qauth.NormalizeRegion(region),
		// PAT 存 RefreshToken 槽位（长期凭证）
		RefreshToken: pat,
	}
	if a.UID == "" {
		uidSum := md5.Sum([]byte(pat))
		a.UID = "qoder-" + hex.EncodeToString(uidSum[:8])
	}
	p.mu.Lock()
	p.bridges[a.UID] = b
	p.mu.Unlock()
	return a, nil
}

// RefreshToken：OAuth 账号临期时走 deviceToken/refresh；PAT 长期有效不刷新。
func (p *Provider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
	if a.Source != "oauth" {
		return false, nil
	}
	if a.ExpiresAt > 0 && time.Now().Add(30*time.Minute).Unix() < a.ExpiresAt {
		return false, nil
	}
	if a.RefreshToken == "" {
		return false, nil
	}
	region := qauth.RegionByName(a.Realm)
	body, _ := json.Marshal(map[string]any{"refresh_token": a.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, region.OpenAPIBase+pathDeviceRefsh, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	cfg := p.realmConfig()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cfg.ua)
	resp, err := p.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("deviceToken 刷新失败 HTTP %d", resp.StatusCode)
	}
	var data struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || data.Token == "" {
		return false, fmt.Errorf("deviceToken 刷新响应异常")
	}
	a.AccessToken = data.Token
	if data.RefreshToken != "" {
		a.RefreshToken = data.RefreshToken
	}
	if data.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(data.ExpiresIn) * time.Millisecond).Unix()
	}
	// 重建会话桥（旧 COSY 会话带旧 token）
	p.mu.Lock()
	delete(p.bridges, a.UID)
	p.mu.Unlock()
	return true, nil
}

// ── 额度 ─────────────────────────────────────────────────

// quotaBearer CN 用 COSY 会话 token；国际区需 OpenAPI token（PAT 换取，缓存）。
func (b *bridge) quotaBearer(ctx context.Context) (string, error) {
	if !b.region.NeedsOpenAPIToken {
		return b.securityOauth, nil
	}
	b.openapiMu.Lock()
	defer b.openapiMu.Unlock()
	if b.openapiBearer != "" && time.Since(b.openapiFetchedAt) < 50*time.Minute {
		return b.openapiBearer, nil
	}
	tok, err := qauth.ExchangeOpenAPIToken(ctx, b.pat, b.region)
	if err != nil {
		return "", err
	}
	b.openapiBearer = tok.Token
	b.openapiFetchedAt = time.Now()
	return b.openapiBearer, nil
}

// Quota 查询额度：user/status（套餐信息）+ OpenAPI quota（权威余额，三桶合计）。
func (p *Provider) Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error) {
	b := p.bridgeOf(a)
	if err := p.ensureFresh(ctx, b); err != nil {
		return nil, err
	}
	status := ""
	if raw, err := qauth.FetchUserStatus(ctx, b.identity.UID, b.machineID, b.machineToken, b.machineType, b.region); err == nil {
		if st, ok := qauth.ParseAccountStatus(raw); ok {
			status = st.UserTag
			if st.Plan != "" && status == "" {
				status = st.Plan
			}
		}
	}
	q := &model.QuotaInfo{
		Provider: p.ID(),
		UID:      a.UID,
		Unit:     "credits",
	}
	detail := map[string]any{}
	if status != "" {
		detail["plan"] = status
	}
	if bearer, err := b.quotaBearer(ctx); err == nil {
		if usage, err := qauth.FetchQuotaUsage(ctx, bearer, b.region); err == nil && usage != nil {
			var remain, used, limit float64
			for _, quota := range []*qauth.Quota{usage.UserQuota, usage.AddOnQuota} {
				if quota != nil {
					remain += quota.Remaining
					used += quota.Used
					limit += quota.Total
				}
			}
			if usage.OrgResourcePackage != nil && usage.OrgResourcePackage.Available {
				remain += usage.OrgResourcePackage.Remaining
				used += usage.OrgResourcePackage.Used
				limit += usage.OrgResourcePackage.Cap
			}
			q.Used, q.Limit, q.Remaining = used, limit, remain
			q.Healthy = !usage.IsQuotaExceeded && remain > 0
			detail["quota_exceeded"] = usage.IsQuotaExceeded
			if usage.ExpiresAtMs > 0 {
				detail["resets_at"] = time.UnixMilli(usage.ExpiresAtMs).Format("01-02 15:04")
				detail["resets_at_unix"] = usage.ExpiresAtMs / 1000
			}
		} else if err != nil {
			detail["quota_error"] = err.Error()
		}
	}
	q.Detail = detail
	return q, nil
}

// ── 签到（campaigns 领取） ───────────────────────────────

// Checkin 每日积分领取：GET campaigns → 过滤 CLAIMABLE → POST claim。
func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	b := p.bridgeOf(a)
	if err := p.ensureFresh(ctx, b); err != nil {
		return nil, err
	}
	bearer, err := b.quotaBearer(ctx)
	if err != nil || bearer == "" {
		bearer = b.securityOauth
	}
	if bearer == "" {
		return nil, fmt.Errorf("签到 token 不可用")
	}
	origin := "https://qoder.com.cn"
	if b.region.Name == "global" {
		origin = "https://qoder.sh"
	}
	do := func(path, method string) (map[string]any, int, error) {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		req, rerr := http.NewRequestWithContext(reqCtx, method, b.region.OpenAPIBase+path, nil)
		if rerr != nil {
			return nil, 0, rerr
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Qoder")
		req.Header.Set("Cosy-ClientType", "10")
		req.Header.Set("Cosy-Version", "0.3.4")
		req.Header.Set("Cosy-MachineId", b.machineID)
		req.Header.Set("Cosy-MachineToken", b.machineToken)
		req.Header.Set("Cosy-MachineType", b.machineType)
		req.Header.Set("Cosy-MachineCode", b.machineCode)
		req.Header.Set("Cosy-MachineOS", machineOSString())
		if h := machineHostnameString(); h != "" {
			req.Header.Set("Cosy-MachineHostname", h)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", b.region.OpenAPIBase+"/growth-page/activity-iframe")
		resp, rerr := p.http.Do(req)
		if rerr != nil {
			return nil, 0, rerr
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode >= 300 {
			return nil, resp.StatusCode, fmt.Errorf("签到接口 HTTP %d", resp.StatusCode)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, resp.StatusCode, fmt.Errorf("签到响应解析失败: %w", err)
		}
		return parsed, resp.StatusCode, nil
	}
	list, _, err := do(campaignsPath, http.MethodGet)
	if err != nil {
		return &model.CheckinResult{OK: false, Message: err.Error()}, nil
	}
	items := campaignsFrom(list)
	var claimable, claimed int
	for _, c := range items {
		if c["actionType"] != "CLAIM_BENEFIT" {
			continue
		}
		switch c["claimStatus"] {
		case "CLAIMED":
			claimed++
		case "CLAIMABLE":
			claimable++
		}
	}
	if claimable == 0 {
		if claimed > 0 {
			return &model.CheckinResult{OK: true, Already: true, Message: "今日已签到"}, nil
		}
		return &model.CheckinResult{OK: false, Message: "暂无可领取的签到活动"}, nil
	}
	okCount := 0
	for _, c := range items {
		if c["actionType"] != "CLAIM_BENEFIT" || c["claimStatus"] != "CLAIMABLE" {
			continue
		}
		cid, _ := c["campaignId"].(string)
		if cid == "" {
			if cid2, ok := c["id"].(string); ok {
				cid = cid2
			} else {
				continue
			}
		}
		if _, _, cerr := do(campaignsPath+"/"+cid+"/claim", http.MethodPost); cerr == nil {
			okCount++
		}
	}
	if okCount > 0 {
		return &model.CheckinResult{OK: true, Message: fmt.Sprintf("签到成功（领取 %d 项积分活动）", okCount), CreditsEarned: int64(okCount)}, nil
	}
	return &model.CheckinResult{OK: false, Message: "领取未确认（可能异步入账，稍后刷新额度查看）"}, nil
}

func campaignsFrom(raw map[string]any) []map[string]any {
	if raw == nil {
		return nil
	}
	var out []map[string]any
	// 常见包裹：{"data": {"campaignList": [...]}} / {"data": [...]} / {"campaigns": [...]}
	for _, key := range []string{"campaigns", "campaignList"} {
		if v, ok := raw[key].([]any); ok {
			out = append(out, toMaps(v)...)
		}
	}
	if data, ok := raw["data"].(map[string]any); ok {
		out = append(out, campaignsFrom(data)...)
	} else if v, ok := raw["data"].([]any); ok {
		out = append(out, toMaps(v)...)
	}
	return out
}

func toMaps(v []any) []map[string]any {
	var out []map[string]any
	for _, item := range v {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ── 模型列表 ─────────────────────────────────────────────

// Models 拉取模型目录（model/list → raw.chat[]，字段 key/display_name）。
func (p *Provider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	b := p.bridgeOf(a)
	if err := p.ensureFresh(ctx, b); err != nil {
		return nil, err
	}
	mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := qauth.FetchModelCatalog(mctx, b.sess, b.region)
	if err != nil {
		return nil, fmt.Errorf("模型目录获取失败: %w", err)
	}
	scene, ok := raw["chat"].([]any)
	if !ok {
		return nil, fmt.Errorf("模型目录为空")
	}
	var out []model.ModelInfo
	for _, item := range scene {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, _ := m["key"].(string)
		if key == "" {
			continue
		}
		name, _ := m["display_name"].(string)
		if name == "" {
			name, _ = m["displayName"].(string)
		}
		out = append(out, model.ModelInfo{ID: key, Name: name, Provider: p.ID(), SupportsTools: true, Credits: qoderCredits(m)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("模型目录为空")
	}
	return out, nil
}

// qoderCredits 目录条目的计价倍率：显式 credits 原文优先；否则 price_factor
// 渲染 "<factor>x"（控制台同款）；is_free=true 且无正倍率才算免费（上游会出现
// is_free=true 但 price_factor>0 的情况，以正倍率为准）。
func qoderCredits(m map[string]any) string {
	if c, _ := m["credits"].(string); strings.TrimSpace(c) != "" {
		return c
	}
	factor, hasFactor := 0.0, false
	switch v := m["price_factor"].(type) {
	case float64:
		factor, hasFactor = v, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			factor, hasFactor = f, true
		}
	}
	if hasFactor && factor > 0 {
		return strconv.FormatFloat(factor, 'f', -1, 64) + "x"
	}
	if free, _ := m["is_free"].(bool); free {
		return "免费"
	}
	return ""
}

// ── 对话（COSY 签名 SSE → OpenAI SSE） ───────────────────

// upstreamFrame 上游帧（OpenAI 形态增量）。
type upstreamFrame struct {
	content   string
	reasoning string
	tools     []byte
	usage     []byte
	finish    string
	errText   string
}

// ChatStream 发起对话：构建 Qoder 请求体 → COSY 签名流 → 转 OpenAI SSE。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	b := p.bridgeOf(a)
	if err := p.ensureFresh(ctx, b); err != nil {
		return nil, err
	}
	plain := buildChatBody(body, modelName)
	raw, err := json.Marshal(plain)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		err := qauth.OpenStreamLines(ctx, b.sess, qauth.ChatURL(b.region), raw, nil, func(line string) error {
			out, cerr := convertLine(pw, line, modelName)
			if cerr != nil {
				return cerr
			}
			if len(out) > 0 {
				if _, werr := pw.Write(out); werr != nil {
					return werr
				}
			}
			return nil
		})
		// 流结束：补 finish/[DONE]
		if err != nil && !errors.Is(err, io.EOF) {
			errPayload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
			_, _ = pw.Write(append(append([]byte("data: "), errPayload...), '\n', '\n'))
		} else {
			_, _ = pw.Write([]byte("data: [DONE]\n\n"))
		}
		_ = pw.CloseWithError(nil)
	}()
	return &provider.ChatResult{
		Body:        pr,
		ContentType: "text/event-stream",
	}, nil
}

// convertLine 单行上游 SSE → OpenAI chunk（可返回空）。
func convertLine(w io.Writer, line, modelName string) ([]byte, error) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "data:") {
		return nil, nil
	}
	data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if data == "" || data == "[DONE]" {
		return nil, nil
	}
	var outer map[string]any
	if json.Unmarshal([]byte(data), &outer) != nil {
		return nil, nil
	}
	body := outer
	if nested, ok := outer["body"]; ok {
		switch typed := nested.(type) {
		case string:
			var inner map[string]any
			if json.Unmarshal([]byte(typed), &inner) == nil {
				body = inner
			}
		case map[string]any:
			body = typed
		}
	}
	frame := frameFromBody(body)
	if frame.errText != "" && frame.content == "" && frame.reasoning == "" && len(frame.tools) == 0 {
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": frame.errText, "type": "api_error"}})
		return append(append([]byte("data: "), payload...), '\n', '\n'), nil
	}
	delta := map[string]any{}
	if frame.content != "" {
		delta["content"] = frame.content
	}
	if frame.reasoning != "" {
		delta["reasoning_content"] = frame.reasoning
	}
	if len(frame.tools) > 0 {
		var tools any
		if json.Unmarshal(frame.tools, &tools) == nil {
			delta["tool_calls"] = tools
		}
	}
	if len(delta) == 0 && frame.finish == "" && frame.usage == nil {
		return nil, nil
	}
	chunk := map[string]any{
		"id": "chatcmpl-qoder", "object": "chat.completion.chunk", "model": modelName,
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	}
	if frame.finish != "" {
		chunk["choices"].([]any)[0].(map[string]any)["finish_reason"] = frame.finish
	}
	if frame.usage != nil {
		var u any
		if json.Unmarshal(frame.usage, &u) == nil {
			chunk["usage"] = u
		}
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("data: "), payload...), '\n', '\n'), nil
}

// frameFromBody 从上游帧提取增量（OpenAI 形态：choices[].delta/message）。
func frameFromBody(body map[string]any) upstreamFrame {
	var frame upstreamFrame
	if usage, ok := body["usage"]; ok && usage != nil {
		frame.usage, _ = json.Marshal(usage)
	}
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		return frame
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return frame
	}
	if finish, _ := choice["finish_reason"].(string); finish != "" {
		frame.finish = finish
	}
	part, _ := choice["delta"].(map[string]any)
	if part == nil {
		part, _ = choice["message"].(map[string]any)
	}
	if part == nil {
		return frame
	}
	frame.content, _ = part["content"].(string)
	frame.reasoning, _ = part["reasoning_content"].(string)
	if tools, ok := part["tool_calls"].([]any); ok && len(tools) > 0 {
		frame.tools, _ = json.Marshal(tools)
	}
	return frame
}

// buildChatBody OpenAI 请求 → Qoder agent_chat_generation 请求体。
// 结构对齐 cli2api BuildPlainChatBody 的关键字段。
func buildChatBody(openAIBody []byte, modelName string) map[string]any {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools            json.RawMessage `json:"tools"`
		MaxTokens        *int64          `json:"max_tokens"`
		Temperature      *float64        `json:"temperature"`
		TopP             *float64        `json:"top_p"`
		EnableThinking   *bool           `json:"enable_thinking"`
		PresencePenalty  *float64        `json:"presence_penalty"`
		FrequencyPenalty *float64        `json:"frequency_penalty"`
		Seed             *int64          `json:"seed"`
	}
	_ = json.Unmarshal(openAIBody, &req)

	var systemParts []string
	messages := make([]map[string]any, 0, len(req.Messages))
	prompt := ""
	for _, m := range req.Messages {
		text := contentToText(m.Content)
		if m.Role == "system" || m.Role == "developer" {
			if text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		messages = append(messages, map[string]any{"role": m.Role, "content": text})
		if m.Role == "user" && text != "" {
			prompt = text
		}
	}
	systemText := strings.Join(systemParts, "\n\n")
	if systemText != "" {
		messages = append([]map[string]any{{"role": "system", "content": systemText}}, messages...)
	}
	if len(messages) == 0 {
		messages = []map[string]any{{"role": "user", "content": "ping"}}
	}
	if prompt == "" {
		prompt = "chat"
	}
	maxTokens := int64(32000)
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	parameters := map[string]any{"max_tokens": maxTokens}
	if req.Temperature != nil {
		parameters["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		parameters["top_p"] = *req.TopP
	}
	// 采样/思考开关等参数透传（客户端可带 enable_thinking:false 关闭推理烧输出预算）
	if req.EnableThinking != nil {
		parameters["enable_thinking"] = *req.EnableThinking
	}
	if req.PresencePenalty != nil {
		parameters["presence_penalty"] = *req.PresencePenalty
	}
	if req.FrequencyPenalty != nil {
		parameters["frequency_penalty"] = *req.FrequencyPenalty
	}
	if req.Seed != nil {
		parameters["seed"] = *req.Seed
	}
	tools := json.RawMessage("[]")
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		tools = req.Tools // Qoder 网关接受标准 OpenAI 形态（数组 + 对象参数），勿改写
	}
	name := prompt
	if len(name) > 40 {
		name = name[:40]
	}
	key := modelName
	if key == "" {
		key = "auto"
	}
	return map[string]any{
		"request_id":     newQoderID(),
		"request_set_id": newQoderID(),
		"chat_record_id": newQoderID(),
		"session_id":     newQoderID(),
		"stream":         true,
		"chat_task":      "FREE_INPUT",
		"chat_context": map[string]any{
			"text":     map[string]any{"type": "text", "text": prompt},
			"features": []any{},
			"extra": map[string]any{
				"modelConfig":     map[string]any{"key": key, "is_reasoning": false},
				"originalContent": map[string]any{"type": "text", "text": prompt},
			},
			"chatPrompt": "",
			"imageUrls":  nil,
		},
		"is_reply":     false,
		"is_retry":     false,
		"source":       1,
		"version":      "3",
		"agent_id":     "agent_common",
		"task_id":      "common",
		"session_type": "qodercli",
		"model_config": map[string]any{
			"key": key, "display_name": key, "model": "", "format": "openai",
			"is_vl": true, "is_reasoning": false, "api_key": "", "url": "",
			"source": "system", "max_input_tokens": 180000,
		},
		"custom_model": nil,
		"system":       systemText,
		"messages":     messages,
		"tools":        tools,
		"parameters":   parameters,
		"business": map[string]any{
			"product": "cli", "version": qauth.CosyVersion, "type": "agent",
			"id": newQoderID(), "name": name, "begin_at": time.Now().UnixMilli(), "stage": "start",
		},
	}
}

// contentToText OpenAI content（字符串或数组）→ 纯文本。
func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		var parts []string
		for _, p := range arr {
			if p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func randRead(b []byte) (int, error) { return rand.Read(b) }

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func uuidNewString() string { return uuid.New().String() }

func uuidNewHexString() string { return uuid.New().String()[:32] }

// uidMachineIdentity OAuth 账号的设备指纹（按 UID 派生，同账号跨重启稳定）。
func uidMachineIdentity(uid string) (id, token, typ string) {
	idSum := md5.Sum([]byte("machine:" + uid))
	id = hex.EncodeToString(idSum[:])
	tokSum := sha512.Sum512([]byte("machinetoken:" + uid))
	token = base64.RawURLEncoding.EncodeToString(tokSum[:])[:43]
	typeSum := md5.Sum([]byte("machinetype:" + uid))
	typ = strings.ReplaceAll(hex.EncodeToString(typeSum[:]), "-", "")
	if len(typ) > 18 {
		typ = typ[:18]
	}
	return
}

func newQoderID() string {
	sum := md5.Sum([]byte(time.Now().Format("20060102150405.000000000") + fmt.Sprint(time.Now().UnixNano())))
	return hex.EncodeToString(sum[:16])
}
