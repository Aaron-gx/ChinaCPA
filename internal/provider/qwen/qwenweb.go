// qwenweb.go 实现 Qwen 网页会话通道（chat.qwen.ai，Account.Source == "web"）。
// 背景：Qwen Code 的 OAuth 设备流免费层已于 2026-04-15 下线，chat.qwen.ai 前端
// （0.3.22+）移除了 /authorize 授权页（路由直接跳回首页），设备码登录链路整体失效。
// 网页版聊天仍免费，故改为网页会话反代：用户粘贴 Local Storage 的 token（或整串
// Cookie），以 Bearer + Cookie 调用网页端 API。协议参考社区逆向实现（qwen3-reverse）：
//   - 模型目录：GET /api/models → {data:[{id,...}]}
//   - 用户信息：GET /api/v1/auths/ → {id,name,email,...}
//   - 建会话：POST /api/v2/chats/new {title,models,chat_mode,chat_type,timestamp} → {data:{id}}
//   - 对话：POST /api/v2/chat/completions?chat_id=X（SSE：delta.phase=think/answer +
//     delta.status=finished，包一层 choices[0].delta，转 OpenAI chunk 输出）
package qwen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

// newUUID 会话字段 ID（fid/childrenIds）。
func newUUID() string { return uuid.NewString() }

const webBase = "https://chat.qwen.ai"

const webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// webMu 串行化同一账号的网页会话创建（chats/new 与 completions 需要配对）。
var webMu sync.Mutex

// webHeaders 网页端 API 通用头（带 UA 防 WAF；token 失效时优先检查这里）。
// a 可为 nil（登录前请求，无 Authorization/Cookie）。
func (p *Provider) webHeaders(a *model.Account, accept string) map[string]string {
	h := map[string]string{
		"Content-Type":      "application/json",
		"Accept":            accept,
		"User-Agent":        webUserAgent,
		"x-accel-buffering": "no",
		"Origin":            webBase,
		"Referer":           webBase + "/",
	}
	if a != nil {
		if a.AccessToken != "" {
			h["Authorization"] = "Bearer " + a.AccessToken
		}
		if a.WebCookie != "" {
			h["Cookie"] = a.WebCookie
		}
	}
	return h
}

// webDo 网页端 API 请求小封装。
func (p *Provider) webDo(ctx context.Context, a *model.Account, method, path, accept string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, webBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range p.webHeaders(a, accept) {
		req.Header.Set(k, v)
	}
	return p.http.Do(req)
}

// webValidate 校验网页会话凭证并取用户信息（id/name/email，尽力而为）。
func (p *Provider) webValidate(ctx context.Context, a *model.Account) error {
	resp, err := p.webDo(ctx, a, http.MethodGet, "/api/v1/auths/", "application/json", nil)
	if err != nil {
		return fmt.Errorf("凭证校验请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if isWAFPage(raw) {
		return fmt.Errorf("凭证校验被 WAF 人机校验拦截，请稍后重试或改用整串 Cookie 导入")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &provider.Error{Kind: provider.ErrAuth, Status: resp.StatusCode,
			Msg: "token 无效或已过期（chat.qwen.ai → F12 → Local Storage → token 重新复制）"}
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("凭证校验失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	var data struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &data) == nil {
		if data.ID != "" {
			a.UID = data.ID
		}
		if data.Name != "" {
			a.Nickname = data.Name
		}
		if data.Email != "" {
			a.Email = data.Email
		}
	}
	return nil
}

// webImport 解析并校验网页会话凭证。支持四种粘贴格式：
//  1. 邮箱:密码 —— 自动调 /api/v1/auths/signin 登录取 token（推荐，全自动）
//  2. 整串 Cookie（含 token=eyJ...，带浏览器已解的 WAF 风控字段，对话最稳）
//  3. 裸 token（Local Storage 里的 token 值，eyJ 开头的 JWT）
//  4. JSON {"email","password"} 或 {"token","cookie"}
func (p *Provider) webImport(ctx context.Context, raw string) (*model.Account, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("凭证为空")
	}
	token, cookie := "", ""
	var signInErr error
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
		if email, pass := str("email"), str("password"); email != "" && pass != "" {
			token, cookie, signInErr = p.webSignIn(ctx, email, pass)
		} else {
			token = pick(str("token"), str("accessToken"), str("access_token"))
			cookie = pick(str("cookie"), str("Cookie"))
		}
	} else if isEmailPassword(raw) {
		idx := strings.Index(raw, ":")
		token, cookie, signInErr = p.webSignIn(ctx, raw[:idx], raw[idx+1:])
	} else if strings.Contains(raw, ";") && strings.Contains(raw, "=") {
		cookie = raw
		token = extractCookieValue(raw, "token")
	} else {
		token = raw
	}
	if signInErr != nil {
		return nil, signInErr
	}
	if token == "" && cookie != "" {
		return nil, fmt.Errorf("Cookie 中未找到 token= 字段（请复制包含 token 的完整 Cookie，或直接粘贴 Local Storage 的 token 值）")
	}
	if token == "" {
		return nil, fmt.Errorf("未识别的凭证格式：支持 邮箱:密码 自动登录 / 整串 Cookie / Local Storage 的 token 值")
	}
	a := &model.Account{
		Provider:    p.ID(),
		Source:      "web",
		AccessToken: token,
		WebCookie:   cookie,
	}
	if err := p.webValidate(ctx, a); err != nil {
		return nil, err
	}
	if a.UID == "" {
		a.UID = "qwen-web-" + hashSuffix(token)
	}
	if exp := jwtExpiry(token); exp > 0 {
		a.ExpiresAt = exp
	}
	return a, nil
}

// isEmailPassword 识别 邮箱:密码 形态（含 @ 与 :，无分号/大括号/空格）。
func isEmailPassword(raw string) bool {
	if strings.ContainsAny(raw, ";{}") || strings.Contains(raw, " ") {
		return false
	}
	at, colon := strings.Index(raw, "@"), strings.Index(raw, ":")
	return at > 0 && colon > at
}

// webSignIn 邮箱+密码自动登录（POST /api/v1/auths/signin，OpenWebUI 标准端点，
// 实测服务端直调可行、无验证码）。返回 token 与登录响应下发的全部 Cookie
// （含 acw_tc 等 WAF 字段，供对话请求复用）。
func (p *Provider) webSignIn(ctx context.Context, email, password string) (string, string, error) {
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webBase+"/api/v1/auths/signin", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	h := p.webHeaders(nil, "application/json")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("登录请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if isWAFPage(raw) {
		return "", "", fmt.Errorf("登录请求被 WAF 人机校验拦截（当前出口 IP 风控较严），请改用整串 Cookie 导入")
	}
	if resp.StatusCode >= 400 {
		// detail 有两种形态：{"detail":{"code","details"}} 或 {"detail":"email not found"}
		var eb struct {
			Detail json.RawMessage `json:"detail"`
		}
		_ = json.Unmarshal(raw, &eb)
		code, msg := "", string(eb.Detail)
		var obj struct {
			Code    string `json:"code"`
			Details string `json:"details"`
		}
		if len(eb.Detail) > 0 && eb.Detail[0] == '{' {
			if json.Unmarshal(eb.Detail, &obj) == nil {
				code, msg = obj.Code, obj.Details
			}
		}
		msg = strings.Trim(msg, "\"")
		switch {
		case strings.Contains(strings.ToLower(msg), "email not found"):
			return "", "", fmt.Errorf("邮箱不存在（请确认是 chat.qwen.ai 的注册邮箱）")
		case code == "INVALID_CRED" || resp.StatusCode == http.StatusUnauthorized:
			return "", "", fmt.Errorf("邮箱或密码错误（%s）", pick(msg, "chat.qwen.ai 登录口令不匹配"))
		}
		return "", "", fmt.Errorf("登录失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	var data struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Token == "" {
		return "", "", fmt.Errorf("登录响应异常（未返回 token）: %s", truncate(string(raw), 160))
	}
	// 收集登录响应下发的全部 Cookie（含 WAF/会话字段）
	var sb strings.Builder
	for _, c := range resp.Cookies() {
		if sb.Len() > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(c.Name + "=" + c.Value)
	}
	return data.Token, sb.String(), nil
}

// isWAFPage 识别阿里 WAF 人机校验挑战页（服务端无浏览器环境解不了 JS 挑战）。
func isWAFPage(raw []byte) bool {
	return bytes.Contains(raw, []byte("aliyun_waf_aa")) ||
		bytes.Contains(raw, []byte("acw_sc__v2"))
}

// extractCookieValue 从 Cookie 串取指定字段值。
func extractCookieValue(cookie, name string) string {
	for _, item := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) == 2 && kv[0] == name {
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

// jwtExpiry 从 JWT exp claim 取过期时间（秒）；非 JWT 返回 0。
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	padded := parts[1]
	if pad := len(padded) % 4; pad != 0 {
		padded += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	return claims.Exp
}

// webModels 网页端模型目录（GET /api/models → {data:[{id,...}]}）。
func (p *Provider) webModels(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	resp, err := p.webDo(ctx, a, http.MethodGet, "/api/models", "application/json", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("模型目录 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	var data struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &data) != nil || len(data.Data) == 0 {
		return nil, fmt.Errorf("模型目录为空")
	}
	out := make([]model.ModelInfo, 0, len(data.Data))
	for _, m := range data.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, model.ModelInfo{ID: m.ID, Name: pick(m.Name, m.ID), Provider: p.ID(), SupportsTools: false})
	}
	return out, nil
}

// webFlattenMessages OpenAI messages → 网页端单条 user 文本（网页 API 无多轮结构，
// 历史平铺拼接；对齐 qwen3-reverse 的格式）。
func webFlattenMessages(openAIBody []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(openAIBody, &req)
	var parts []string
	hasSystem := false
	for _, m := range req.Messages {
		text := flattenContent(m.Content)
		if text == "" {
			continue
		}
		if m.Role == "system" || m.Role == "developer" {
			hasSystem = true
			parts = append(parts, "system:\n\n"+text)
			continue
		}
		parts = append(parts, m.Role+": "+text)
	}
	joined := strings.Join(parts, "\n\n")
	if !hasSystem && joined != "" {
		joined = "system:\n\n" + joined
	}
	if joined == "" {
		joined = "ping"
	}
	return joined
}

// flattenContent OpenAI content（字符串或数组）→ 纯文本。
func flattenContent(raw json.RawMessage) string {
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
		var out []string
		for _, p := range arr {
			if p.Text != "" {
				out = append(out, p.Text)
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// webChatStream 网页端对话：建会话 → SSE → 转 OpenAI chunk（think→reasoning_content）。
func (p *Provider) webChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	var req struct {
		Stream         *bool `json:"stream"`
		EnableThinking *bool `json:"enable_thinking"`
	}
	_ = json.Unmarshal(body, &req)
	content := webFlattenMessages(body)

	webMu.Lock()
	chatID, err := p.webCreateChat(ctx, a, modelName)
	webMu.Unlock()
	if err != nil {
		return nil, err
	}

	nowMs := time.Now().UnixMilli()
	thinking := true
	if req.EnableThinking != nil {
		thinking = *req.EnableThinking
	}
	payload := map[string]any{
		"stream":             true,
		"incremental_output": true,
		"chat_id":            chatID,
		"chat_mode":          "normal",
		"model":              modelName,
		"parent_id":          nil,
		"messages": []map[string]any{{
			"fid":            newUUID(),
			"parentId":       nil,
			"childrenIds":    []string{newUUID()},
			"role":           "user",
			"content":        content,
			"user_action":    "chat",
			"files":          []any{},
			"timestamp":      nowMs,
			"models":         []string{modelName},
			"chat_type":      "t2t",
			"feature_config": map[string]any{"output_schema": "phase", "thinking_enabled": thinking},
			"extra":          map[string]any{"meta": map[string]any{"subChatType": "t2t"}},
			"sub_chat_type":  "t2t",
			"parent_id":      nil,
		}},
		"timestamp": nowMs,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := p.webDo(ctx, a, http.MethodPost, "/api/v2/chat/completions?chat_id="+chatID, "text/event-stream", raw)
	if err != nil {
		p.webDeleteChat(a, chatID)
		return nil, err
	}
	if resp.StatusCode >= 400 {
		rbody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		p.webDeleteChat(a, chatID)
		return &provider.ChatResult{
			Status:   resp.StatusCode,
			RespBody: rbody,
			Err: &provider.Error{
				Kind:   classifyStatus(resp.StatusCode),
				Status: resp.StatusCode,
				Msg:    truncate(string(rbody), 200),
			},
		}, nil
	}
	pr, pw := io.Pipe()
	go func() {
		err := convertWebSSE(resp.Body, pw, chatID, modelName)
		if err != nil && err != io.EOF {
			errPayload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
			_, _ = pw.Write(append(append([]byte("data: "), errPayload...), '\n', '\n'))
		}
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
		_ = pw.CloseWithError(nil)
		go p.webDeleteChat(a, chatID)
	}()
	return &provider.ChatResult{Body: pr, ContentType: "text/event-stream"}, nil
}

// webCreateChat 创建网页会话（POST /api/v2/chats/new → data.id）。
func (p *Provider) webCreateChat(ctx context.Context, a *model.Account, modelName string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"title":     "ChinaCPA",
		"models":    []string{modelName},
		"chat_mode": "normal",
		"chat_type": "t2t",
		"timestamp": time.Now().UnixMilli(),
	})
	resp, err := p.webDo(ctx, a, http.MethodPost, "/api/v2/chats/new", "application/json", body)
	if err != nil {
		return "", fmt.Errorf("创建会话失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 || isWAFPage(raw) {
		if isWAFPage(raw) {
			return "", &provider.Error{Kind: provider.ErrUpstream, Status: resp.StatusCode,
				Msg: "创建会话被 chat.qwen.ai WAF 人机校验拦截（服务端无浏览器风控 Cookie）。" +
					"请删除该账号改用「整串 Cookie」导入：F12 → Network → 随便发一条消息 → 复制 chat/completions 请求的完整 Cookie"}
		}
		return "", &provider.Error{Kind: classifyStatus(resp.StatusCode), Status: resp.StatusCode,
			Msg: fmt.Sprintf("创建会话 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))}
	}
	var data struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Data.ID == "" {
		return "", fmt.Errorf("创建会话响应异常: %s", truncate(string(raw), 160))
	}
	return data.Data.ID, nil
}

// webDeleteChat 清理网页会话（best-effort，避免账号聊天列表被灌满）。
func (p *Provider) webDeleteChat(a *model.Account, chatID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := p.webDo(ctx, a, http.MethodDelete, "/api/v2/chats/"+chatID, "application/json", nil)
	if err == nil {
		resp.Body.Close()
	}
}

// convertWebSSE 网页端 SSE → OpenAI chunk：
//   - delta.phase=="think" → reasoning_content
//   - delta.phase=="answer" 或无 phase 的 content → content
//   - delta.status=="finished" → finish_reason 终止帧
func convertWebSSE(rc io.Reader, w io.Writer, chatID, modelName string) error {
	buf := make([]byte, 0, 32*1024)
	tmp := make([]byte, 16*1024)
	chunkID := "chatcmpl-qwen-" + chatID
	sent := func(payload []byte) error {
		_, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n'))
		return err
	}
	emit := func(delta map[string]any, finish string) error {
		chunk := map[string]any{
			"id": chunkID, "object": "chat.completion.chunk", "model": modelName,
			"choices": []any{map[string]any{"index": 0, "delta": delta}},
		}
		if finish != "" {
			chunk["choices"].([]any)[0].(map[string]any)["finish_reason"] = finish
		}
		payload, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		return sent(payload)
	}
	handleLine := func(line string) error {
		s := strings.TrimSpace(line)
		if !strings.HasPrefix(s, "data:") {
			return nil
		}
		data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
		if data == "" || data == "[DONE]" {
			return nil
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Phase        string `json:"phase"`
					Status       string `json:"status"`
					Content      string `json:"content"`
					FinishReason string `json:"finish_reason"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &frame) != nil || len(frame.Choices) == 0 {
			return nil
		}
		d := frame.Choices[0].Delta
		switch {
		case d.Phase == "think":
			if d.Status != "finished" && d.Content != "" {
				return emit(map[string]any{"reasoning_content": d.Content}, "")
			}
		case d.Phase == "answer" || (d.Phase == "" && d.Content != ""):
			if d.Content != "" {
				return emit(map[string]any{"content": d.Content}, "")
			}
		}
		if d.Status == "finished" {
			finish := d.FinishReason
			if finish == "" {
				finish = "stop"
			}
			return emit(map[string]any{}, finish)
		}
		return nil
	}
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
				if err := handleLine(line); err != nil {
					return err
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF && len(buf) > 0 {
				if err := handleLine(string(buf)); err != nil {
					return err
				}
			}
			return rerr
		}
	}
}
