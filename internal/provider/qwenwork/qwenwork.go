// Package qwenwork 实现千问办公（QwenWork，gateway.qwenwork.cn）provider。
// 协议逆向自 QwenWorkCN 桌面端（asar）并经社区实现（xrp-cosy）在线验证：
//   - 登录：网关 device-flow（/device/selectAccounts → 钉钉扫码 → /api/v1/deviceToken/poll）
//   - 刷新：POST /api/v1/deviceToken/refresh {refresh_token, target:"c"}（refresh token 轮换式）
//   - 推理：POST /algo/api/v2/service/pro/sse/agent_chat_generation（明文 JSON + MD5 COSY 签名）
//   - 签名：cosy-key = base64(RSA_PKCS1(公钥, e))，info = base64(AES-128-CBC(key=e, iv=e, userInfo))，
//     authorization = "Bearer COSY.<b64(header)>.<md5(o \n cosyKey \n ts \n body \n path)>"
//   - 挡位：x-model-key 请求头传目录 key（flash / pro / qwen3.8-max-preview），
//     body 必须带 business.product=qoder_work（缺失时网关 503 Model catalog unavailable）
//   - 签到：每日登录赠送由服务端自动发放（钱包 daily_credits），桌面端 claim
//     /api/v1/adapter/user/daily-credit-notice/claim 仅是通知回执；本端以钱包余额确认为准
package qwenwork

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

const (
	gatewayBase = "https://gateway.qwenwork.cn"
	webBase     = "https://qwenwork.cn"

	pathDevicePoll    = "/api/v1/deviceToken/poll"
	pathDeviceRefresh = "/api/v1/deviceToken/refresh"
	pathModelList     = "/api/v2/model/list?Encode=1"
	pathChat          = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common"
	pathBalance       = "/user/balance"
	pathDailyClaim    = "/api/v1/adapter/user/daily-credit-notice/claim"

	deviceClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	deviceRedirect = "qwenwork-cn://"

	// rsaPublicKeyPEM 桌面端 asar 硬编码 RSA-1024 公钥（modulus 头 c0f223…）。
	// 仅用于封装每请求随机 16B AES key（cosy-key），非敏感材料。
	rsaPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`
)

// 内置挡位目录（云端目录 /api/v2/model/list 不可达时的回落）。
var builtinModels = []model.ModelInfo{
	{ID: "flash", Name: "Standard", Provider: "qwenwork", SupportsTools: true, ContextWindow: 128000, MaxTokens: 32768},
	{ID: "pro", Name: "Pro", Provider: "qwenwork", SupportsTools: true, ContextWindow: 128000, MaxTokens: 32768},
	{ID: "qwen3.8-max-preview", Name: "Qwen3.8-Max", Provider: "qwenwork", SupportsTools: true, ContextWindow: 128000, MaxTokens: 32768},
}

// Provider 千问办公实现。
type Provider struct {
	http *http.Client

	mu       sync.Mutex
	sessions map[string]*loginSession
}

// loginSession 一次设备流登录（verifier/challenge/nonce 绑定校验用）。
type loginSession struct {
	verifier  string
	challenge string
	nonce     string
	machineID string
	created   time.Time
}

// New 构造。
func New() *Provider {
	return &Provider{
		http:     &http.Client{Timeout: 120 * time.Second},
		sessions: map[string]*loginSession{},
	}
}

func (p *Provider) ID() string          { return "qwenwork" }
func (p *Provider) DisplayName() string { return "千问办公（QwenWork）" }

// ── 登录（网页 device-flow，钉钉扫码） ────────────────────

// pkceVerifier 字符集/长度对齐官方客户端：43 + rand(0..85) 字符。
func pkceVerifier() string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	n := 43 + randInt(86)
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[randInt(len(chars))]
	}
	return string(b)
}

func randInt(n int) int {
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	return int(v.Int64())
}

// StartLogin 生成 PKCE + nonce，返回网关登录页 URL（浏览器打开后钉钉扫码，
// 服务端把凭据绑定到 nonce/challenge，本端无需回调监听）。
func (p *Provider) StartLogin(_ context.Context, _ string) (*model.LoginSession, error) {
	verifier := pkceVerifier()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	nonce := uuid.NewString()
	machineID := uuid.NewString()
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	q.Set("machine_id", machineID)
	q.Set("client_id", deviceClientID)
	q.Set("redirect_uri", deviceRedirect)
	p.mu.Lock()
	p.sessions[nonce] = &loginSession{verifier: verifier, challenge: challenge, nonce: nonce, machineID: machineID, created: time.Now()}
	p.mu.Unlock()
	return &model.LoginSession{
		Provider:     p.ID(),
		ID:           nonce,
		URL:          gatewayBase + "/device/selectAccounts?" + q.Encode(),
		CodeVerifier: verifier,
		DeviceCode:   nonce,
	}, nil
}

// PollLogin 轮询 /api/v1/deviceToken/poll：404 / 400(INVALID_DEVICE_FLOW) = 进行中。
func (p *Provider) PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error) {
	p.mu.Lock()
	sess := p.sessions[s.ID]
	p.mu.Unlock()
	if sess == nil {
		return &model.PollResult{Status: model.LoginFailed, Message: "登录会话不存在或已过期，请重新发起"}, nil
	}
	if time.Since(sess.created) > 10*time.Minute {
		p.mu.Lock()
		delete(p.sessions, s.ID)
		p.mu.Unlock()
		return &model.PollResult{Status: model.LoginFailed, Message: "授权窗口已过期，请重新发起"}, nil
	}
	q := url.Values{}
	q.Set("nonce", sess.nonce)
	q.Set("verifier", sess.verifier)
	q.Set("challenge_method", "S256")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayBase+pathDevicePoll+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-Id", uuid.NewString())
	resp, err := p.http.Do(req)
	if err != nil {
		return &model.PollResult{Status: model.LoginPending, Message: "网络波动，继续等待授权…"}, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 404 {
		return &model.PollResult{Status: model.LoginPending, Message: "等待浏览器完成钉钉扫码授权"}, nil
	}
	if resp.StatusCode == 400 {
		var eb struct {
			ErrorCode string `json:"errorCode"`
		}
		_ = json.Unmarshal(raw, &eb)
		if eb.ErrorCode == "INVALID_DEVICE_FLOW" {
			// nonce 未登记/未完成授权，继续轮询
			return &model.PollResult{Status: model.LoginPending, Message: "等待浏览器完成钉钉扫码授权"}, nil
		}
		return &model.PollResult{Status: model.LoginFailed, Message: "授权轮询失败: " + truncate(string(raw), 200)}, nil
	}
	if resp.StatusCode >= 300 {
		return &model.PollResult{Status: model.LoginFailed, Message: fmt.Sprintf("授权轮询失败 HTTP %d", resp.StatusCode)}, nil
	}
	var data struct {
		Token               string `json:"token"`
		RefreshToken        string `json:"refresh_token"`
		Nonce               string `json:"nonce"`
		CodeChallenge       string `json:"code_challenge"`
		CodeChallengeMethod string `json:"code_challenge_method"`
		ExpiresAt           string `json:"expires_at"`
		ExpiresIn           int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || data.Token == "" || data.RefreshToken == "" {
		return &model.PollResult{Status: model.LoginPending, Message: "等待授权完成"}, nil
	}
	// 三连绑定校验（防串号采纳他人凭据）
	if data.Nonce != sess.nonce || data.CodeChallenge != sess.challenge || data.CodeChallengeMethod != "S256" {
		return &model.PollResult{Status: model.LoginFailed, Message: "poll 绑定校验失败（nonce/challenge 不匹配），已拒绝"}, nil
	}
	p.mu.Lock()
	delete(p.sessions, s.ID)
	p.mu.Unlock()
	a := accountFromToken(data.Token, data.RefreshToken, parseExpiry(data.ExpiresAt, data.ExpiresIn, 3600))
	return &model.PollResult{Status: model.LoginDone, Account: a}, nil
}

// accountFromToken 由 access token（JWT）解出 uid/昵称并构建账号。
func accountFromToken(token, refreshToken string, expiresAt int64) *model.Account {
	claims := jwtPayload(token)
	a := &model.Account{
		Provider:     "qwenwork",
		AccessToken:  token,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Source:       "device",
	}
	a.UID = str(claims["sub"])
	if a.UID == "" {
		a.UID = str(claims["uid"])
	}
	if a.UID == "" {
		a.UID = "qw-" + hashSuffix(refreshToken)
	}
	a.Nickname = firstNonEmpty(str(claims["username"]), str(claims["name"]), str(claims["nickname"]))
	a.Email = str(claims["email"])
	return a
}

// jwtPayload 解 JWT payload 为 claims（base64url，无签名校验——客户端只读 claims）。
func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 容错 padding 变体
		if raw, err = base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)); err != nil {
			return map[string]any{}
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return map[string]any{}
	}
	return m
}

// ── 凭证导入 ─────────────────────────────────────────────

// ImportCredential 支持粘贴：
//   - ory_rt_ 开头的 refresh token（裸文本，最常见）
//   - JSON：{"refresh_token": "..."} 或含 access_token 的完整凭据
func (p *Provider) ImportCredential(ctx context.Context, raw string) (*model.Account, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("凭证为空")
	}
	rt := raw
	var accessToken string
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
		rt = firstNonEmpty(str("refresh_token"), str("refreshToken"))
		accessToken = firstNonEmpty(str("access_token"), str("accessToken"), str("device_token"))
	}
	if rt == "" && accessToken == "" {
		return nil, fmt.Errorf("凭证中未找到 refresh_token / access_token（千问办公凭据为 ory_rt_ 开头的刷新令牌）")
	}
	if rt != "" {
		// 用刷新链验证并轮换（refresh token 是轮换式，验证即换新）
		st, err := p.refreshDeviceToken(ctx, rt)
		if err != nil {
			return nil, fmt.Errorf("refresh token 校验失败: %w", err)
		}
		a := accountFromToken(st.token, st.refreshToken, st.expiresAt)
		if accessToken != "" && a.UID == "" {
			a.UID = "qw-" + hashSuffix(a.RefreshToken)
		}
		return a, nil
	}
	// 仅 access token：直接采用（到期前可用，之后需重新导入）
	a := accountFromToken(accessToken, "", time.Now().Add(time.Hour).Unix())
	if claims := jwtPayload(accessToken); a.ExpiresAt == 0 {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			a.ExpiresAt = int64(exp)
		}
	}
	return a, nil
}

// ── Token 刷新 ───────────────────────────────────────────

type tokenState struct {
	token        string
	refreshToken string
	expiresAt    int64
}

// refreshDeviceToken 调 /api/v1/deviceToken/refresh（纯 HTTP API，无 COSY 签名）。
// refresh token 轮换式：响应中的新值必须落盘，旧值作废。
func (p *Provider) refreshDeviceToken(ctx context.Context, refreshToken string) (*tokenState, error) {
	body, _ := json.Marshal(map[string]any{"refresh_token": refreshToken, "target": "c"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayBase+pathDeviceRefresh, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &provider.Error{Kind: provider.ErrAuth, Status: resp.StatusCode,
			Msg: fmt.Sprintf("deviceToken/refresh HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))}
	}
	var data struct {
		DeviceToken  string `json:"device_token"`
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("刷新响应解析失败: %w", err)
	}
	tok := firstNonEmpty(data.DeviceToken, data.Token)
	if tok == "" || data.RefreshToken == "" {
		return nil, fmt.Errorf("刷新响应缺 device_token/refresh_token")
	}
	return &tokenState{token: tok, refreshToken: data.RefreshToken, expiresAt: parseExpiry(data.ExpiresAt, 0, 3600)}, nil
}

// RefreshToken 临期（<10 分钟）时轮换；changed=true 表示凭证有更新。
func (p *Provider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
	if a.RefreshToken == "" {
		return false, nil
	}
	if a.AccessToken != "" && a.ExpiresAt > 0 && time.Now().Add(10*time.Minute).Unix() < a.ExpiresAt {
		return false, nil
	}
	st, err := p.refreshDeviceToken(ctx, a.RefreshToken)
	if err != nil {
		return false, err
	}
	a.AccessToken = st.token
	a.RefreshToken = st.refreshToken
	a.ExpiresAt = st.expiresAt
	claims := jwtPayload(st.token)
	if a.UID == "" {
		a.UID = firstNonEmpty(str(claims["sub"]), str(claims["uid"]), "qw-"+hashSuffix(a.RefreshToken))
	}
	if a.Nickname == "" {
		a.Nickname = firstNonEmpty(str(claims["username"]), str(claims["name"]))
	}
	return true, nil
}

// ── COSY 签名 ────────────────────────────────────────────

// signMaterial 每请求新生成的签名材料（e 为随机 16B AES key）。
type signMaterial struct {
	cosyKey string // base64(RSA_PKCS1(pub, e)) → Cosy-Key 头
	info    string // base64(AES-128-CBC(e, e, userInfo)) → COSY token 的 info 字段
	uid     string
}

var cachedPubKey *rsa.PublicKey

func embeddedPubKey() (*rsa.PublicKey, error) {
	if cachedPubKey != nil {
		return cachedPubKey, nil
	}
	block, _ := pem.Decode([]byte(rsaPublicKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("RSA 公钥 PEM 解析失败")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("非 RSA 公钥")
	}
	cachedPubKey = rk
	return rk, nil
}

// aesCBCEncrypt PKCS7 填充的 AES-128-CBC（key 与 iv 同为 e 的 UTF-8 字节）。
func aesCBCEncrypt(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := block.BlockSize() - len(plain)%block.BlockSize()
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:block.BlockSize()]).CryptBlocks(out, padded)
	return out, nil
}

// buildSignMaterial 等价于官方 encryptUserInfo：随机 16 字符 hex 作为 AES key e。
func buildSignMaterial(accessToken, uid, name, email string) (*signMaterial, error) {
	e := randomHex16()
	userInfo, err := json.Marshal(struct {
		UID                string `json:"uid"`
		AID                string `json:"aid"`
		Name               string `json:"name"`
		Email              string `json:"email"`
		SecurityOauthToken string `json:"security_oauth_token"`
	}{UID: uid, Name: name, Email: email, SecurityOauthToken: accessToken})
	if err != nil {
		return nil, err
	}
	info, err := aesCBCEncrypt(userInfo, []byte(e))
	if err != nil {
		return nil, err
	}
	pub, err := embeddedPubKey()
	if err != nil {
		return nil, err
	}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(e))
	if err != nil {
		return nil, err
	}
	return &signMaterial{
		cosyKey: base64.StdEncoding.EncodeToString(enc),
		info:    base64.StdEncoding.EncodeToString(info),
		uid:     uid,
	}, nil
}

// randomHex16 16 个 hex 字符（uuid 去横线取前 16，等价官方实现）。
func randomHex16() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
}

// signHeaders 等价于官方 generateAuthToken：authorization + Cosy-Key/Date/User。
// path 归一化：去 query、去 /algo 前缀。
func signHeaders(m *signMaterial, fullURL, body string) map[string]string {
	return signHeadersAt(m, fullURL, body, time.Now().Unix())
}

func signHeadersAt(m *signMaterial, fullURL, body string, ts int64) map[string]string {
	header, _ := json.Marshal(struct {
		Version     string `json:"version"`
		RequestID   string `json:"requestId"`
		Info        string `json:"info"`
		CosyVersion string `json:"cosyVersion"`
		IdeVersion  string `json:"ideVersion"`
	}{Version: "v1", RequestID: uuid.NewString(), Info: m.info, CosyVersion: "1.0.0", IdeVersion: "1.0.0"})
	o := base64.StdEncoding.EncodeToString(header)
	path := fullURL
	if u, err := url.Parse(fullURL); err == nil {
		path = u.Path
	}
	if i := strings.Index(path, "?"); i > 0 {
		path = path[:i]
	}
	if strings.HasPrefix(path, "/algo") {
		path = path[5:]
	}
	signStr := o + "\n" + m.cosyKey + "\n" + fmt.Sprint(ts) + "\n" + body + "\n" + path
	sum := md5.Sum([]byte(signStr))
	return map[string]string{
		"Authorization": "Bearer COSY." + o + "." + fmt.Sprintf("%x", sum[:]),
		"Cosy-Key":      m.cosyKey,
		"Cosy-Date":     fmt.Sprint(ts),
		"Cosy-User":     m.uid,
	}
}

// cosyStaticHeaders 对齐官方客户端的 Cosy 静态头。
func cosyStaticHeaders() map[string]string {
	return map[string]string{
		"Cosy-Business-Product": "qoder_work",
		"Cosy-Business-Type":    "agent",
		"Cosy-ClientType":       "6",
		"Cosy-Data-Policy":      "disagree",
		"Cosy-MachineId":        "unknown",
		"Cosy-MachineToken":     "unknown",
		"Cosy-MachineType":      "5",
		"Cosy-Scene":            "qwork",
		"Cosy-MachineOs":        machineOS(),
		"Cosy-Version":          "1.1.59",
		"Login-Version":         "v2",
		"x-model-source":        "system",
	}
}

// machineOS 对齐官方 `${arch}_${platform}`（node 平台名，如 x86_64_win32）。
func machineOS() string {
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	osName := "linux"
	switch runtime.GOOS {
	case "windows":
		osName = "win32"
	case "darwin":
		osName = "darwin"
	}
	return arch + "_" + osName
}

// ── 额度 ─────────────────────────────────────────────────

// Quota 查询积分余额（网站域 /user/balance，纯 Bearer，无 COSY 签名）。
func (p *Provider) Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error) {
	if a.AccessToken == "" {
		return nil, fmt.Errorf("无 access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webBase+pathBalance, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &provider.Error{Kind: classifyStatus(resp.StatusCode), Status: resp.StatusCode,
			Msg: fmt.Sprintf("余额查询 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))}
	}
	var data struct {
		Data struct {
			Balance       float64        `json:"balance"`
			HasActivePlan bool           `json:"has_active_plan"`
			Raw           map[string]any `json:"-"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("余额响应解析失败: %w", err)
	}
	detail := map[string]any{}
	var extra map[string]any
	_ = json.Unmarshal(raw, &extra)
	if d, ok := extra["data"].(map[string]any); ok {
		for k, v := range d {
			if k != "balance" {
				detail[k] = v
			}
		}
	}
	q := &model.QuotaInfo{
		Provider:  p.ID(),
		UID:       a.UID,
		Remaining: data.Data.Balance,
		Limit:     0,
		Unit:      "credits",
		Healthy:   data.Data.Balance > 0,
		Detail:    detail,
		FetchedAt: time.Now().Unix(),
	}
	return q, nil
}

// Checkin 每日积分确认（桌面端 daily-credit-notice 协议逆向）：
// 每日登录赠送（个人版 100~200/天，当日有效）由服务端在登录事件上自动发放——
// 上游没有独立的「领取」接口，桌面端的 claim 只是到账通知的回执（无待领通知时 404）。
// 本端流程：查钱包 daily_credits → 已到账直接确认；未到账则调一次 claim 兜底
// 并短暂轮询钱包（发放可能异步）。登录事件由调度器签到前的 EnsureFreshToken
// （token 临期轮换）提供。结果以钱包真实余额为准。
func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	before, bOK := p.dailyCreditBalance(ctx, a)
	if bOK && before > 0 {
		return &model.CheckinResult{OK: true, Already: true,
			Message: fmt.Sprintf("今日每日积分已到账 %d", before)}, nil
	}
	if _, err := p.RefreshToken(ctx, a); err != nil {
		// 刷新失败不阻断：存量 access token 可能仍在有效期
	}
	if raw, err := p.claimDailyNotice(ctx, a); err == nil {
		if claim := parseDailyClaim(raw); claim.credits > 0 {
			return &model.CheckinResult{OK: true, Message: "签到成功", CreditsEarned: claim.credits}, nil
		}
	}
	// 发放可能异步：轮询钱包最多 ~10s
	for i := 0; i < 5; i++ {
		time.Sleep(2 * time.Second)
		after, aOK := p.dailyCreditBalance(ctx, a)
		if aOK && bOK && after > before {
			return &model.CheckinResult{OK: true, Message: "签到成功", CreditsEarned: after - before}, nil
		}
		if aOK && !bOK && after > 0 {
			return &model.CheckinResult{OK: true, Message: "签到成功", CreditsEarned: after}, nil
		}
	}
	if bOK {
		// 上游无独立签到接口，发放由登录事件触发；本端无 web 登录通道时只能等待。
		// OK=false 但 Info=true：这是状态说明而非签到失败，调度层照常重试。
		return &model.CheckinResult{OK: false, Info: true,
			Message: "今日每日积分未到账（由登录触发发放，可网页登录一次后重试）"}, nil
	}
	return &model.CheckinResult{OK: false, Message: "钱包查询失败，稍后自动重试"}, nil
}

// claimDailyNotice 调桌面端 claim 端点（POST，无 query/body）。
// 该端点仅在有到账通知待确认时返回 200，否则 404——一律视为兜底手段，错误不外抛。
func (p *Provider) claimDailyNotice(ctx context.Context, a *model.Account) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayBase+pathDailyClaim, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	for k, v := range qwenWorkClientHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &provider.Error{Kind: classifyStatus(resp.StatusCode), Status: resp.StatusCode,
			Msg: fmt.Sprintf("claim HTTP %d: %s", resp.StatusCode, truncate(string(raw), 120))}
	}
	return raw, nil
}

// dailyClaim claim 响应关键结果（{code:"ok", data:{should_show, credits}}）。
type dailyClaim struct {
	ok         bool
	shouldShow bool
	credits    int64
}

// parseDailyClaim 解析 claim 响应体。
func parseDailyClaim(raw []byte) dailyClaim {
	var data struct {
		Code string `json:"code"`
		Data struct {
			ShouldShow bool  `json:"should_show"`
			Credits    int64 `json:"credits"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return dailyClaim{}
	}
	return dailyClaim{ok: data.Code == "ok", shouldShow: data.Data.ShouldShow, credits: data.Data.Credits}
}

// dailyCreditBalance 读钱包 daily_credits.total_balance（当日有效积分）。
func (p *Provider) dailyCreditBalance(ctx context.Context, a *model.Account) (int64, bool) {
	if a.AccessToken == "" {
		return 0, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webBase+"/user/wallets", nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data struct {
		Data struct {
			DailyCredits struct {
				TotalBalance float64 `json:"total_balance"`
			} `json:"daily_credits"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return 0, false
	}
	return int64(data.Data.DailyCredits.TotalBalance), true
}

// qwenWorkClientHeaders 桌面端 OpenAPI 客户端头（asar getQwenWorkClientHeaders）。
func qwenWorkClientHeaders() map[string]string {
	return map[string]string{
		"X-QwenWork-Version":         "1.1.59",
		"X-QwenWork-Release-Version": "1.1.59",
		"X-QwenWork-Build":           "1159",
		"X-QwenWork-Platform":        "win32",
		"X-QwenWork-Arch":            "x64",
		"X-QwenWork-Channel":         "stable",
		"X-QwenWork-App-Mode":        "default",
	}
}

// ── 模型列表 ─────────────────────────────────────────────

// Models 拉取云端挡位目录（/api/v2/model/list，COSY 签名 GET），失败回落内置目录。
func (p *Provider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	if a.AccessToken == "" {
		return builtinModels, nil
	}
	m, err := buildSignMaterial(a.AccessToken, a.UID, a.Nickname, a.Email)
	if err != nil {
		return nil, err
	}
	full := gatewayBase + pathModelList
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range cosyStaticHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range signHeaders(m, full, "") {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return builtinModels, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return builtinModels, nil
	}
	var data struct {
		Qwork []struct {
			Key            string  `json:"key"`
			DisplayName    string  `json:"display_name"`
			PriceFactor    float64 `json:"price_factor"`
			IsDefault      bool    `json:"is_default"`
			IsNew          bool    `json:"is_new"`
			MaxInputTokens int64   `json:"max_input_tokens"`
			Enable         *bool   `json:"enable"`
		} `json:"qwork"`
	}
	if json.Unmarshal(raw, &data) != nil || len(data.Qwork) == 0 {
		return builtinModels, nil
	}
	out := make([]model.ModelInfo, 0, len(data.Qwork))
	for _, item := range data.Qwork {
		if item.Key == "" || (item.Enable != nil && !*item.Enable) {
			continue
		}
		mi := model.ModelInfo{
			ID:            item.Key,
			Name:          firstNonEmpty(item.DisplayName, item.Key),
			Provider:      p.ID(),
			SupportsTools: true,
			ContextWindow: item.MaxInputTokens,
		}
		if item.PriceFactor > 0 {
			mi.Credits = fmt.Sprintf("%gx", item.PriceFactor)
		}
		out = append(out, mi)
	}
	if len(out) == 0 {
		return builtinModels, nil
	}
	return out, nil
}

// ── 对话转发 ─────────────────────────────────────────────

// ChatStream 转发到 gateway.qwenwork.cn 推理端点：明文 JSON + MD5 COSY 签名，
// 上游为双层 SSE（外层 {headers,body,statusCodeValue}，内层 OpenAI chunk），解包后输出。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if modelName == "" {
		modelName = "flash"
	}
	obj["model"] = modelName
	if _, ok := obj["request_id"].(string); !ok || obj["request_id"] == "" {
		obj["request_id"] = uuid.NewString()
	}
	if _, ok := obj["session_id"].(string); !ok || obj["session_id"] == "" {
		obj["session_id"] = uuid.NewString()
	}
	// business.product 是网关定位模型目录的唯一开关（缺失 → 503 Model catalog unavailable）
	if _, ok := obj["business"].(map[string]any); !ok {
		obj["business"] = map[string]any{"product": "qoder_work"}
	}
	delete(obj, "encode")
	delete(obj, "extra_body")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	bodyStr := string(out)
	m, err := buildSignMaterial(a.AccessToken, a.UID, a.Nickname, a.Email)
	if err != nil {
		return nil, err
	}
	full := gatewayBase + pathChat
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, strings.NewReader(bodyStr))
	if err != nil {
		return nil, err
	}
	for k, v := range cosyStaticHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-model-key", modelName)
	req.Header.Set("User-Agent", "node")
	for k, v := range signHeaders(m, full, bodyStr) {
		req.Header.Set(k, v)
	}
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
	pr, pw := io.Pipe()
	go func() {
		err := unwrapSSE(resp.Body, pw)
		if err != nil && err != io.EOF {
			_, _ = pw.Write(errorEvent(err.Error()))
		}
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
		_ = pw.CloseWithError(nil)
	}()
	return &provider.ChatResult{Body: pr, ContentType: "text/event-stream"}, nil
}

// unwrapSSE 读取上游双层 SSE：外层 data:{"headers":...,"body":"<内层>","statusCodeValue":200}，
// 内层可能是裸 JSON 或再包一层 "data: {...}"；跳过 [DONE] 与空 {}，透传标准 OpenAI chunk。
// 网关错误以「HTTP 200 + wrapper statusCodeValue≠200」出现（如余额不足 402），
// 统一转成 OpenAI error 事件：客户端可正常解析，网关 peekStreamError 据此跨渠道失败转移。
func unwrapSSE(rc io.Reader, w io.Writer) error {
	buf := make([]byte, 0, 32*1024)
	tmp := make([]byte, 16*1024)
	for {
		n, rerr := rc.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				idx := bytes.IndexByte(buf, '\n')
				if idx < 0 {
					break
				}
				line := string(buf[:idx])
				buf = append(buf[:0], buf[idx+1:]...)
				if out := unwrapLine(line); len(out) > 0 {
					if _, werr := w.Write(out); werr != nil {
						return werr
					}
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF && len(buf) > 0 {
				if out := unwrapLine(string(buf)); len(out) > 0 {
					if _, werr := w.Write(out); werr != nil {
						return werr
					}
				}
			}
			return rerr
		}
	}
}

func unwrapLine(line string) []byte {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "data:") {
		return nil
	}
	data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if data == "" || data == "[DONE]" || data == "{}" {
		return nil
	}
	var outer map[string]any
	if json.Unmarshal([]byte(data), &outer) != nil {
		return nil
	}
	// wrapper 错误帧：HTTP 200 但 statusCodeValue≠200（如余额不足 402 Payment Required）
	if sc, ok := statusValueOf(outer); ok && sc != 200 {
		return errorEvent(fmt.Sprintf("上游 HTTP %s: %s", statusTextOf(outer), wrapperErrMsg(outer)))
	}
	inner, ok := outer["body"]
	if !ok {
		// 非 wrapper 帧：标准 OpenAI chunk / error 事件原样透传，其余丢弃
		if _, has := outer["choices"]; has {
			return emitData(data)
		}
		if _, has := outer["error"]; has {
			return emitData(data)
		}
		return nil
	}
	innerStr := innerOf(inner)
	innerStr = strings.TrimSpace(innerStr)
	// 内层还可能再包 "data: {...}"
	if strings.HasPrefix(innerStr, "data:") {
		innerStr = strings.TrimSpace(strings.TrimPrefix(innerStr, "data:"))
	}
	if innerStr == "" || innerStr == "[DONE]" || innerStr == "{}" {
		return nil
	}
	// 内层非 OpenAI chunk 的处理：
	//   code+message 错误体 → 转错误事件；其余 JSON 帧（usage 块、心跳等）补空 choices，
	//   兼容对无 choices 事件直接报错的严格客户端。
	var probe map[string]any
	if json.Unmarshal([]byte(innerStr), &probe) == nil {
		if _, hasChoices := probe["choices"]; !hasChoices {
			if _, hasErr := probe["error"]; !hasErr {
				if _, hasCode := probe["code"]; hasCode {
					if msg, hasMsg := probe["message"].(string); hasMsg && msg != "" {
						return errorEvent("上游错误: " + msg)
					}
				}
				probe["choices"] = []any{}
				if fixed, err := json.Marshal(probe); err == nil {
					return emitData(string(fixed))
				}
			}
		}
	}
	return emitData(innerStr)
}

// emitData 包一行标准 SSE data 事件。
func emitData(payload string) []byte {
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

// errorEvent 输出 OpenAI 形态错误事件（与 qoder 渠道、网关 peekStreamError 约定一致）。
func errorEvent(msg string) []byte {
	errPayload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}})
	return emitData(string(errPayload))
}

// statusValueOf 读 wrapper 的 statusCodeValue（数字或字符串）。
func statusValueOf(outer map[string]any) (float64, bool) {
	switch v := outer["statusCodeValue"].(type) {
	case float64:
		return v, true
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// statusTextOf 拼状态码与原因短语（"402" / "402 Payment Required"）。
func statusTextOf(outer map[string]any) string {
	sc, ok := statusValueOf(outer)
	if !ok {
		return "错误"
	}
	base := fmt.Sprintf("%.0f", sc)
	if phrase, _ := outer["statusCode"].(string); phrase != "" && phrase != base {
		return base + " " + phrase
	}
	return base
}

// wrapperErrMsg 从 wrapper 帧提取错误消息：内层 body 的 message 字段优先。
func wrapperErrMsg(outer map[string]any) string {
	switch b := outer["body"].(type) {
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(b), &m) == nil {
			if s, ok := m["message"].(string); ok && s != "" {
				return s
			}
		}
		if b != "" {
			return truncate(b, 160)
		}
	case map[string]any:
		if s, ok := b["message"].(string); ok && s != "" {
			return s
		}
		raw, _ := json.Marshal(b)
		return truncate(string(raw), 160)
	}
	return "upstream error"
}

// innerOf 内层 body 的两种形态（字符串 / 已是对象）。
func innerOf(inner any) string {
	switch v := inner.(type) {
	case string:
		return v
	case map[string]any:
		b, _ := json.Marshal(v)
		return string(b)
	default:
		return ""
	}
}

// ── 工具 ─────────────────────────────────────────────────

func classifyStatus(code int) provider.ErrKind {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return provider.ErrAuth
	case code == http.StatusTooManyRequests:
		return provider.ErrRate
	case code == http.StatusPaymentRequired:
		return provider.ErrQuota
	case code >= 500:
		return provider.ErrUpstream
	}
	return provider.ErrClient
}

// parseExpiry ISO 串优先，其次 expiresIn 秒数，都缺用 fallbackSec。
func parseExpiry(iso string, expiresIn int64, fallbackSec int64) int64 {
	if iso != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, iso); err == nil {
				return t.Unix()
			}
		}
		if t, err := time.Parse(time.RFC3339, iso[:minLen(len(iso), 19)]+"Z"); err == nil {
			return t.Unix()
		}
	}
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
	}
	if fallbackSec > 0 {
		return time.Now().Add(time.Duration(fallbackSec) * time.Second).Unix()
	}
	return 0
}

func minLen(a, b int) int {
	if a < b {
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

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func hashSuffix(s string) string {
	sum := sha256.Sum256([]byte("qwenwork:" + s))
	return fmt.Sprintf("%x", sum[:4])
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
