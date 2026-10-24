// Package trae 实现字节 Trae（SOLO 免费通道）provider：
// 授权 URL 登录 + 回调纳管、ExchangeToken 刷新、每日签到（+200 ide_credits）、
// 额度查询、模型列表（get_detail_param）、对话转发与 SOLO SSE → OpenAI SSE 转换。
//
// 接口口径来自开源参考实现（trae2api-web）的实测结论：
//   - 对话 POST trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat，function 固定 solo_work_lite
//   - 鉴权 Cloud-IDE-JWT；签到/积分在 api.trae.cn（X-User-Region: CN）
//   - SSE 事件：metadata / output / extra_info / token_usage / done / error
//   - 错误码：1001 认证失效；1005 权益不足；4008 配额超限；4011 频率超限；9074 签到人数过多
package trae

import (
	"bufio"
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

	clientID       = "en1oxy7wnw8j9n"
	appID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	ideVersion     = "0.1.52"
	ideVersionCode = "20260811"
	deviceBrand    = "83DG"
	osVersion      = "Windows 11 Pro"
	soloFunction   = "solo_work_lite"

	epChat          = "/api/agent/v3/llm_utils_chat"
	epModels        = "/api/ide/v1/get_detail_param"
	epExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	epUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	epCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	epCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	epEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)

// Provider Trae 实现。
type Provider struct {
	http *http.Client
	// streamHTTP 无总超时（SSE 长流）
	streamHTTP *http.Client

	mu       sync.Mutex
	sessions map[string]*model.LoginSession // sessionID -> 待回调会话
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
		sessions:   map[string]*model.LoginSession{},
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
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	req.Header.Set("X-Cloudide-Token", a.AccessToken)
	req.Header.Set("X-Ide-Token", a.AccessToken)
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

// ── 登录（授权 URL + 回调） ───────────────────────────────

func hex32() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// BuildLoginURL 构造 TRAE 授权 URL（参数集对齐官方 login.sh）。
func BuildLoginURL(machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", clientID)
	v.Set("redirect", "0")
	trace := machineID + deviceID
	if len(trace) > 16 {
		trace = trace[len(trace)-16:]
	}
	return consoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(trace) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(ideVersion) +
		"&x_app_type=stable"
}

// StartLogin 生成授权 URL；callbackBase 为面板外部可达地址（如 http://127.0.0.1:8317）。
func (p *Provider) StartLogin(ctx context.Context, callbackBase string) (*model.LoginSession, error) {
	machineID := hex32()
	deviceID := hex32()
	if callbackBase == "" {
		callbackBase = "http://127.0.0.1:8317"
	}
	callbackURL := strings.TrimSuffix(callbackBase, "/") + "/trae/callback"
	id := hex16()
	s := &model.LoginSession{
		Provider:    p.ID(),
		ID:          id,
		URL:         BuildLoginURL(machineID, deviceID, callbackURL),
		CallbackURL: callbackURL,
		State:       machineID + ":" + deviceID, // 回调时取回
	}
	p.mu.Lock()
	p.sessions[id] = s
	p.mu.Unlock()
	return s, nil
}

func hex16() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleCallback 处理 /trae/callback 回调（refreshToken + userInfo + userJwt），
// 返回完成态登录会话。未找到会话时返回错误。
func (p *Provider) HandleCallback(q url.Values) (*model.PollResult, error) {
	refreshToken := q.Get("refreshToken")
	accessToken := ""
	uid := ""
	nickname := ""
	// userInfo: {nickName/userId...}（可能双重 URL 编码，宽松解两层）
	if ui := q.Get("userInfo"); ui != "" {
		for i := 0; i < 2; i++ {
			if dec, err := url.QueryUnescape(ui); err == nil && dec != ui {
				ui = dec
			}
		}
		var m map[string]any
		if json.Unmarshal([]byte(ui), &m) == nil {
			str := func(keys ...string) string {
				for _, k := range keys {
					if v, ok := m[k].(string); ok && v != "" {
						return v
					}
				}
				return ""
			}
			nickname = str("nickName", "nickname", "name", "userName")
			uid = str("userId", "uid", "user_id")
		}
	}
	// userJwt 兜底
	if uj := q.Get("userJwt"); uj != "" {
		var m map[string]any
		if json.Unmarshal([]byte(uj), &m) == nil {
			if v, ok := m["Token"].(string); ok && accessToken == "" {
				accessToken = v
			}
			if v, ok := m["RefreshToken"].(string); ok && refreshToken == "" {
				refreshToken = v
			}
			if uid == "" {
				if v, ok := m["UserId"].(string); ok {
					uid = v
				} else if v, ok := m["userId"].(string); ok {
					uid = v
				}
			}
		}
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("回调缺少 refreshToken")
	}
	a := &model.Account{
		Provider:      p.ID(),
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		UID:           uid,
		Nickname:      nickname,
	}
	// 立即 ExchangeToken 换正式 access + uid
	if _, err := p.RefreshToken(context.Background(), a); err != nil {
		return nil, fmt.Errorf("ExchangeToken 失败: %w", err)
	}
	if a.UID == "" {
		if uid, nick, _, err := p.getUserInfo(context.Background(), a); err == nil {
			a.UID, a.Nickname = uid, nick
		}
	}
	if a.UID == "" {
		a.UID = "trae-" + hex16()
	}
	// 消费最早的待回调会话取 machine/device（保持与登录 URL 一致）
	p.mu.Lock()
	var s *model.LoginSession
	for _, v := range p.sessions {
		s = v
		break
	}
	if s != nil {
		parts := strings.SplitN(s.State, ":", 2)
		if len(parts) == 2 {
			a.MachineID, a.DeviceID = parts[0], parts[1]
		}
		delete(p.sessions, s.ID)
	}
	p.mu.Unlock()
	if a.MachineID == "" {
		a.MachineID, a.DeviceID = hex32(), hex32()
	}
	return &model.PollResult{Status: model.LoginDone, Account: a}, nil
}

// PollLogin Trae 通过回调完成，轮询返回等待（回调侧直接完成纳管）。
func (p *Provider) PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error) {
	return &model.PollResult{Status: model.LoginPending, Message: "请在浏览器完成登录，成功后自动回调纳管"}, nil
}

// ImportCredential 支持粘贴 refreshToken（或含 refresh_token 的 JSON）。
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
	} else {
		a.RefreshToken = raw
	}
	if a.AccessToken == "" && a.RefreshToken == "" {
		return nil, fmt.Errorf("凭证中未找到 refreshToken")
	}
	if a.MachineID == "" {
		a.MachineID, a.DeviceID = hex32(), hex32()
	}
	if _, err := p.RefreshToken(ctx, a); err != nil && a.AccessToken == "" {
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

// ── ExchangeToken（token 刷新，refresh 轮换） ─────────────

func (p *Provider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return false, nil
	}
	if a.ExpiresAt > 0 && time.Now().Add(10*time.Minute).Unix() < a.ExpiresAt && a.AccessToken != "" {
		return false, nil
	}
	body, _ := json.Marshal(map[string]any{
		"ClientID":     clientID,
		"RefreshToken": a.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthHost+epExchange, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	oauthHeaders(req)
	data, err := p.doJSON(req)
	if err != nil {
		return false, err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, fmt.Errorf("exchange 解析失败: %w", err)
	}
	if resp.Result.Token == "" {
		return false, &provider.Error{Kind: provider.ErrAuth, Msg: "exchange 无 token，需要重新登录"}
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normExpires(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return true, nil
}

func normExpires(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func (p *Provider) getUserInfo(ctx context.Context, a *model.Account) (uid, nickname, enterpriseID string, err error) {
	body, _ := json.Marshal(map[string]any{"ReqSource": "IDE", "IDEVersion": ideVersion})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthHost+epUserInfo, bytes.NewReader(body))
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
			UserId       string `json:"UserId"`
			UserIdStr    string `json:"userId"`
			NickName     string `json:"NickName"`
			EnterpriseId string `json:"EnterpriseId"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo 解析失败: %w", err)
	}
	uid = pick(resp.Result.UserId, resp.Result.UserIdStr)
	return uid, resp.Result.NickName, resp.Result.EnterpriseId, nil
}

// ── 额度查询 ─────────────────────────────────────────────

// Quota 聚合权益包额度：remain = Σ(limit - used)。
// 注意：聚合值含 work 包；SOLO 通道实际可用以对话流 notify_usage 的 ide_credits 为准。
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

// ReportIdeCredits 对话流 notify_usage 实时上报的 SOLO 可用额度（比权益包准确）。
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

// LastIdeCredits 最近一次对话流上报的 ide_credits。
func (p *Provider) LastIdeCredits(uid string) (ide, work float64, ok bool) {
	ideReportMu.Lock()
	defer ideReportMu.Unlock()
	e, exists := ideReport[uid]
	return e.ide, e.work, exists && time.Now().Unix()-e.ts < 86400
}

// ── 签到（每日 +200 ide_credits） ────────────────────────

func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	// 先查状态
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
		Enable    bool  `json:"enable"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("签到状态解析失败: %w", err)
	}
	if st.CheckedIn {
		return &model.CheckinResult{OK: true, Already: true, Message: "今天已签到"}, nil
	}
	// 领取
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
	var cr struct {
		Credits int64 `json:"credits"`
	}
	_ = json.Unmarshal(cdata, &cr)
	return &model.CheckinResult{OK: true, Message: "签到成功", CreditsEarned: 200}, nil
}

// ── 模型列表 ─────────────────────────────────────────────

// Models 拉取 SOLO 模型配置表（get_detail_param）。
func (p *Provider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	body, _ := json.Marshal(map[string]any{
		"function":            soloFunction,
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
	if len(out) == 0 {
		return nil, fmt.Errorf("模型表为空")
	}
	return out, nil
}

// ── 对话转发（OpenAI → SOLO，SSE 双向转换） ───────────────

// prepareBody OpenAI → SOLO 请求体改写：
// content 字符串 → 数组；stream 强制 true；model → config_name+model；function 固定；
// tools/tool_choice 归一化；assistant tool_calls function → function_call。
func prepareBody(src []byte, modelName string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = soloFunction
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
			if role == "tool" {
				// OpenAI tool 结果消息 → SOLO 可识别的 function 结果形态
				if c, ok := m["content"].(string); ok {
					m["content"] = []any{map[string]any{"type": "text", "text": c}}
				}
			}
		}
	}
	// tools/tool_choice 归一化
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

// ChatStream 转发对话：上游恒为 SSE，按客户端请求决定流出形态
// （客户端要 SSE → 实时转换透传；要 JSON → 聚合成完整响应）。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	wantStream := false
	var probe struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &probe) == nil {
		wantStream = probe.Stream
	}
	prepared := prepareBody(body, modelName)
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
	// 非流式：聚合
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

// classify 错误分类（HTTP 状态 + body 中的 code）。
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
	// 业务码识别
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

// ── SOLO SSE → OpenAI SSE 转换 ───────────────────────────

// soloToOpenAIStream 包装上游 SSE body 为 OpenAI 兼容 SSE 流。
type soloToOpenAIStream struct {
	src      io.ReadCloser
	sc       *bufio.Scanner
	uid      string
	p        *Provider
	buf      []byte // 待读出数据
	done     bool
	usage    *openAIUsage
	model    string
	id       string
	created  int64
	curEvent string // 当前事件名（event: 行设置，data: 行消费）
}

func newSoloToOpenAIStream(src io.ReadCloser, uid string, p *Provider, modelName string) *soloToOpenAIStream {
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &soloToOpenAIStream{
		src:     src,
		sc:      sc,
		uid:     uid,
		p:       p,
		model:   modelName,
		id:      "chatcmpl-" + hex16(),
		created: time.Now().Unix(),
	}
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func (s *soloToOpenAIStream) chunk(delta map[string]any, finish any, usage *openAIUsage) []byte {
	msg := map[string]any{"role": "assistant", "content": nil}
	for k, v := range delta {
		msg[k] = v
	}
	obj := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         msg,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		obj["usage"] = usage
	}
	raw, _ := json.Marshal(obj)
	return append(raw, '\n')
}

// Read 逐事件转换（Lazy：每次读填 s.buf）。
func (s *soloToOpenAIStream) Read(out []byte) (int, error) {
	for len(s.buf) == 0 {
		if s.done {
			return 0, io.EOF
		}
		if !s.sc.Scan() {
			s.done = true
			s.src.Close()
			// 流意外结束也补 [DONE]
			s.buf = append(s.buf, []byte("data: [DONE]\n\n")...)
			if len(s.buf) > 0 {
				break
			}
			return 0, io.EOF
		}
		line := s.sc.Text()
		if ev, data, isData := parseSSELine(line); isData {
			s.buf = s.handleEvent(s.curEvent, data)
		} else if ev != "" {
			s.curEvent = ev
		}
	}
	n := copy(out, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// parseSSELine 解析 `event:xxx` / `data:{...}`；其他行忽略。
// 返回 (事件名, 数据行, 是否为 data 行)。
func parseSSELine(line string) (ev string, data string, isData bool) {
	line = strings.TrimRight(line, "\r")
	switch {
	case strings.HasPrefix(line, "event:"):
		return strings.TrimSpace(line[6:]), "", false
	case strings.HasPrefix(line, "data:"):
		return "", strings.TrimSpace(line[5:]), true
	}
	return "", "", false
}

// handleEvent 处理一条 SOLO 事件并产出 OpenAI SSE 字节。
func (s *soloToOpenAIStream) handleEvent(evName string, dataLine string) []byte {
	var out []byte
	if dataLine == "" {
		return out
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return out
	}
	switch evName {
	case "output":
		delta := map[string]any{}
		if v, ok := raw["response"].(string); ok && v != "" {
			delta["content"] = v
		}
		if v, ok := raw["reasoning_content"].(string); ok && v != "" {
			delta["reasoning_content"] = v
		}
		if len(delta) > 0 {
			out = append(out, sseWrap(s.chunk(delta, nil, nil))...)
		}
	case "token_usage":
		u := &openAIUsage{}
		if v, ok := raw["prompt_tokens"].(float64); ok {
			u.PromptTokens = int64(v)
		}
		if v, ok := raw["completion_tokens"].(float64); ok {
			u.CompletionTokens = int64(v)
		}
		if v, ok := raw["total_tokens"].(float64); ok {
			u.TotalTokens = int64(v)
		} else {
			u.TotalTokens = u.PromptTokens + u.CompletionTokens
		}
		s.usage = u
		// 倒数第二块：带 usage 的空 delta（OpenAI 惯例 stream_options.include_usage）
		out = append(out, sseWrap(s.chunk(map[string]any{}, nil, u))...)
	case "notify_usage":
		// 实时额度上报：cn_credits_remain_info.ide_credits
		if info, ok := raw["cn_credits_remain_info"].(map[string]any); ok {
			ide, _ := info["ide_credits"].(float64)
			work, _ := info["work_credits"].(float64)
			s.p.ReportIdeCredits(s.uid, ide, work)
		}
	case "done":
		var finish string
		if v, ok := raw["finish_reason"].(string); ok && v != "" {
			finish = v
		} else {
			finish = "stop"
		}
		out = append(out, sseWrap(s.chunk(map[string]any{}, finish, nil))...)
		out = append(out, []byte("data: [DONE]\n\n")...)
		s.done = true
		s.src.Close()
	case "error":
		msg := "upstream error"
		if v, ok := raw["message"].(string); ok {
			msg = v
		} else if v, ok := raw["error"].(string); ok {
			msg = v
		}
		errObj, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}})
		out = append(out, []byte("data: "+string(errObj)+"\n\n")...)
		out = append(out, []byte("data: [DONE]\n\n")...)
		s.done = true
		s.src.Close()
	}
	return out
}

func sseWrap(chunk []byte) []byte {
	return append([]byte("data: "), append(chunk, []byte("\n\n")...)...)
}

func (s *soloToOpenAIStream) Close() error { return s.src.Close() }

// aggregateSolo 非流式：聚合 SOLO SSE 为完整 OpenAI 响应 JSON。
func aggregateSolo(body io.Reader, uid string, p *Provider, modelName string) ([]byte, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var content, reasoning strings.Builder
	var usage openAIUsage
	var finish = "stop"
	errMsg := ""
	evName := ""
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(line, "event:") {
			evName = strings.TrimSpace(line[6:])
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataLine := strings.TrimSpace(line[5:])
		if dataLine == "" {
			continue
		}
		var raw map[string]any
		if json.Unmarshal([]byte(dataLine), &raw) != nil {
			continue
		}
		switch evName {
		case "output":
			if v, ok := raw["response"].(string); ok {
				content.WriteString(v)
			}
			if v, ok := raw["reasoning_content"].(string); ok {
				reasoning.WriteString(v)
			}
		case "token_usage":
			if v, ok := raw["prompt_tokens"].(float64); ok {
				usage.PromptTokens = int64(v)
			}
			if v, ok := raw["completion_tokens"].(float64); ok {
				usage.CompletionTokens = int64(v)
			}
			if v, ok := raw["total_tokens"].(float64); ok {
				usage.TotalTokens = int64(v)
			}
		case "notify_usage":
			if info, ok := raw["cn_credits_remain_info"].(map[string]any); ok {
				ide, _ := info["ide_credits"].(float64)
				work, _ := info["work_credits"].(float64)
				p.ReportIdeCredits(uid, ide, work)
			}
		case "done":
			if v, ok := raw["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		case "error":
			if v, ok := raw["message"].(string); ok {
				errMsg = v
			}
		}
	}
	if errMsg != "" {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Msg: errMsg}
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	resp := map[string]any{
		"id":      "chatcmpl-" + hex16(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	return json.Marshal(resp)
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

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
