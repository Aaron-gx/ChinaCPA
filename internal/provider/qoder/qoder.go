// Package qoder 实现阿里 Qoder（CN qoder.com.cn / 国际 qoder.sh）provider。
// 凭证为 PAT（控制台创建），经 COSY 协议（自定义 base64 + RSA + AES + MD5 签名）
// 换取会话；协议层整体移植自 cli2api / Qoder-2API-Go（internal/provider/qoder/qauth）。
//
// 能力：PAT 导入（双区）、每日签到（campaigns 领取）、额度（user/status + OpenAPI
// quota）、模型目录（model/list）、对话（agent_chat_generation SSE → OpenAI）。
package qoder

import (
	"context"
	"crypto/md5"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
	"chinacpa/internal/provider/qoder/qauth"
)

const campaignsPath = "/sash/api/v1/me/campaigns"

// Provider Qoder 实现。
type Provider struct {
	http *http.Client

	mu      sync.Mutex
	bridges map[string]*bridge // uid -> 会话桥
}

// bridge 单账号会话桥：PAT → jobToken 会话 + 刷新轮换。
type bridge struct {
	pat    string
	region *qauth.RegionConfig

	machineID, machineToken, machineType, machineCode string

	sess           *qauth.SessionContext
	identity       *qauth.AuthIdentity
	refreshToken   string
	securityOauth  string
	expireMs       int64

	openapiMu        sync.Mutex
	openapiBearer    string
	openapiFetchedAt time.Time
}

// New 构造。
func New() *Provider {
	return &Provider{
		http:    &http.Client{Timeout: 120 * time.Second},
		bridges: map[string]*bridge{},
	}
}

func (p *Provider) ID() string          { return "qoder" }
func (p *Provider) DisplayName() string { return "Qoder（阿里 · CN/国际双区）" }

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
	b, ok := p.bridges[a.UID]
	if ok && b.pat == a.RefreshToken {
		return b
	}
	id, token, typ, code := deriveMachineIdentity(a.RefreshToken)
	region := qauth.RegionByName(a.Realm)
	b = &bridge{
		pat: a.RefreshToken, region: region,
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
		UserType:         "personal_standard",
		SecurityOauthToken: securityOauth,
		RefreshToken:     refreshToken,
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

// StartLogin Qoder 无 OAuth 授权页：引导到控制台 PAT 页 + 粘贴导入。
func (p *Provider) StartLogin(ctx context.Context, callbackBase string) (*model.LoginSession, error) {
	return nil, provider.ErrNotSupported
}

// PollLogin 不适用（PAT 导入即完成）。
func (p *Provider) PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error) {
	return nil, provider.ErrNotSupported
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

// RefreshToken PAT 长期有效；会话刷新在 ensureFresh 内按需完成。
func (p *Provider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
	return false, nil
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
	raw, err := qauth.FetchModelCatalog(ctx, b.sess, b.region)
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
		out = append(out, model.ModelInfo{ID: key, Name: name, Provider: p.ID(), SupportsTools: true})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("模型目录为空")
	}
	return out, nil
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
		Tools         json.RawMessage `json:"tools"`
		MaxTokens     *int64          `json:"max_tokens"`
		Temperature   *float64        `json:"temperature"`
		TopP          *float64        `json:"top_p"`
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
	tools := json.RawMessage("[]")
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		tools = req.Tools
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

func newQoderID() string {
	sum := md5.Sum([]byte(time.Now().Format("20060102150405.000000000") + fmt.Sprint(time.Now().UnixNano())))
	return hex.EncodeToString(sum[:16])
}
