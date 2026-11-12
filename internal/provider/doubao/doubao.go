// Package doubao 实现豆包（www.doubao.com 网页版）provider：
// 扫码登录（字节 passport 协议，自动回调纳管）、Cookie 凭证导入（兜底）、
// 文本对话（语言模型）、Seedream 图片生成、Seedance 视频生成。
// 协议为「提交 + 轮询」模型：POST /chat/completion（SSE 读到 SSE_ACK 拿
// conversation_id）→ 轮询 POST /im/chain/single 取增量文本 / 生成产物。
// 协议依据：Doora v0.8.2 逆向（复刻与反代协议文档.md）+ 社区 doubao2api 实测。
package doubao

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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

const dbUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// Provider 豆包实现。无浏览器依赖：扫码登录走字节 passport 协议（StartLogin，
// 打开链接 → 豆包 App 扫码 → 自动回调纳管）；生成请求以整串 Cookie + 生成的
// 设备标识直连网页 API（fp=s_v_web_id 随 Cookie 自带，msToken 可为空）。
type Provider struct {
	http *http.Client // 重定向不跟随：登录链路需手动收每跳 Set-Cookie；登录失效时上游会 302

	// streamHTTP 同步对话内联 SSE 长连接专用：http.Client.Timeout 覆盖 Body 读取
	// 全程，30s 默认值会掐断流，这里不设请求级超时（靠 ctx 与空闲判定）。
	streamHTTP *http.Client

	// ImageDailyLimit / VideoDailyLimit 每账号每日生成上限（本地计数，config 注入）
	ImageDailyLimit int64
	VideoDailyLimit int64

	// chatMu 同一账号串行提交（上游会话/风控约束；不同账号可并行）。
	chatMu sync.Map // uid -> *sync.Mutex

	// qrSessions 扫码登录会话表（StartLogin 建、PollLogin 驱动、面板扫码页只读）。
	qrMu       sync.Mutex
	qrSessions map[string]*qrSession
}

// New 构造；imageLimit/videoLimit 为每日生成上限（<=0 用默认 50/10）。
func New(imageLimit, videoLimit int64) *Provider {
	if imageLimit <= 0 {
		imageLimit = 50
	}
	if videoLimit <= 0 {
		videoLimit = 10
	}
	return &Provider{
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		streamHTTP: &http.Client{
			Timeout: 0,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ImageDailyLimit: imageLimit,
		VideoDailyLimit: videoLimit,
		qrSessions:      map[string]*qrSession{},
	}
}

func (p *Provider) ID() string          { return "doubao" }
func (p *Provider) DisplayName() string { return "豆包 Doubao" }

// lockAccount 单账号互斥锁。
func (p *Provider) lockAccount(uid string) *sync.Mutex {
	m, _ := p.chatMu.LoadOrStore(uid, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ── 设备标识 / Cookie 工具 ───────────────────────────────

// seedOf 取（或生成）账号设备标识（生成形态对齐上游风控 SDK：device_id 16 位首位 7、
// web_id 19 位前缀 76）。
func (p *Provider) seedOf(a *model.Account) deviceIDs {
	if a.DoubaoSeed != "" {
		var ids deviceIDs
		if json.Unmarshal([]byte(a.DoubaoSeed), &ids) == nil && ids.DeviceID != "" {
			return ids
		}
	}
	ids := deviceIDs{DeviceID: genDeviceID(), WebID: genWebID()}
	if raw, err := json.Marshal(ids); err == nil {
		a.DoubaoSeed = string(raw)
	}
	return ids
}

// cookieValue 从账号 Cookie 串取字段（URL 解码尽力而为）。
func cookieValue(cookie, name string) string {
	for _, item := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) == 2 && kv[0] == name {
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

// fpOf 设备指纹（s_v_web_id Cookie；风控关键字段，缺失上游会拒）。
func fpOf(a *model.Account) string { return cookieValue(a.WebCookie, "s_v_web_id") }

// msTokenOf 字节风控令牌（Cookie 里的 msToken）。缺失时返回空串——
// query 侧必须整个省略该参数，发空值会被上游判为伪造触发限流（710022002）。
func msTokenOf(a *model.Account) string { return cookieValue(a.WebCookie, "msToken") }

// regionOf 账号区域（cookie flow_user_country / store-country-code，兜底 CN）。
func regionOf(a *model.Account) string {
	for _, name := range []string{"flow_user_country", "store-country-code"} {
		if v := strings.ToUpper(cookieValue(a.WebCookie, name)); len(v) == 2 {
			return v
		}
	}
	return "CN"
}

// dbHeaders 通用请求头（同源页面 fetch 等价形态）。
func dbHeaders(a *model.Account, contentType string) map[string]string {
	h := map[string]string{
		"Content-Type": contentType,
		"User-Agent":   dbUserAgent,
		"Origin":       dbOrigin,
		"Referer":      dbOrigin + "/chat/",
		"Cookie":       a.WebCookie,
	}
	return h
}

// dbPost POST JSON（SSE/JSON 通用；重定向已被 client 禁止，3xx 即凭证失效）。
func (p *Provider) dbPost(ctx context.Context, a *model.Account, path string, query url.Values, contentType string, body []byte, extra map[string]string) (*http.Response, error) {
	u := dbOrigin + path + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range dbHeaders(a, contentType) {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	return p.http.Do(req)
}

// classifyStatus HTTP 状态 → 错误分类。
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

// authExpiredError 统一的凭证失效提示。
func authExpiredError(code int, body string) *provider.Error {
	return &provider.Error{
		Kind:   provider.ErrAuth,
		Status: code,
		Msg:    "豆包 Cookie 已失效或被风控（登录 www.doubao.com → F12 → Network → 任一请求 → 复制完整 Cookie 重新导入）。上游响应: " + truncateRunes(body, 120),
	}
}

// ── 登录（扫码，qrlogin.go）/ 导入（Cookie 兜底）────────

// StartLogin / PollLogin 实现见 qrlogin.go（字节 passport 扫码协议）。

// ImportCredential 解析并校验凭证。支持：
//  1. 整串 Cookie（推荐：F12 → Network → 任一 www.doubao.com 请求的 Cookie 头）
//  2. JSON {"cookie":"...","device_id":"...","web_id":"..."}（后两项可选，
//     缺省自动生成并落盘）
func (p *Provider) ImportCredential(ctx context.Context, raw string) (*model.Account, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("凭证为空")
	}
	cookieStr, devID, webID := "", "", ""
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %v", err)
		}
		cookieStr = pick(m["cookie"], m["Cookie"])
		devID = pick(m["device_id"], m["deviceID"])
		webID = pick(m["web_id"], m["webID"])
	} else {
		cookieStr = raw
	}
	if !strings.Contains(cookieStr, "=") {
		return nil, fmt.Errorf("未识别的凭证格式：请粘贴 www.doubao.com 请求头的完整 Cookie 值")
	}
	if fp := cookieValue(cookieStr, "s_v_web_id"); fp == "" {
		return nil, fmt.Errorf("Cookie 缺少 s_v_web_id（风控必需字段）；请复制完整的 Cookie 头")
	}
	if cookieValue(cookieStr, "sessionid") == "" && cookieValue(cookieStr, "sid_guard") == "" {
		return nil, fmt.Errorf("Cookie 缺少 sessionid（未登录）；请先登录 www.doubao.com 再复制")
	}
	a := &model.Account{
		Provider:  p.ID(),
		Source:    "web",
		WebCookie: cookieStr,
	}
	if devID != "" || webID != "" {
		if devID == "" {
			devID = genDeviceID()
		}
		if webID == "" {
			webID = genWebID()
		}
		if rawIDs, err := json.Marshal(deviceIDs{DeviceID: devID, WebID: webID}); err == nil {
			a.DoubaoSeed = string(rawIDs)
		}
	}
	// 校验：素材库首页需登录态（302/401 = Cookie 失效）
	ctxv, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := p.validate(ctxv, a); err != nil {
		return nil, err
	}
	p.seedOf(a) // 生成并保存设备标识
	if a.UID == "" {
		sum := sha256.Sum256([]byte("doubao:" + pick(cookieValue(cookieStr, "sessionid"), cookieStr)))
		a.UID = fmt.Sprintf("doubao-%x", sum[:4])
	}
	return a, nil
}

// validate 校验登录态并尽力取用户名（POST /samantha/aispace/homepage）。
func (p *Provider) validate(ctx context.Context, a *model.Account) error {
	ids := p.seedOf(a)
	q := commonQuery(ids, fpOf(a), regionOf(a), msTokenOf(a))
	body, _ := json.Marshal(map[string]any{})
	resp, err := p.dbPost(ctx, a, "/samantha/aispace/homepage", q, "application/json", body, nil)
	if err != nil {
		return fmt.Errorf("校验请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return authExpiredError(resp.StatusCode, string(raw))
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("校验失败 HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 160))
	}
	// 尽力取昵称（响应结构上游未公开，递归找 nickname/name 字段）
	var data any
	if json.Unmarshal(raw, &data) == nil {
		if nick := deepFindString(data, "nickname", 6); nick != "" {
			a.Nickname = nick
		}
		if a.UID == "" {
			if uid := deepFindString(data, "user_id", 6); uid != "" {
				a.UID = "doubao-" + uid
			}
		}
	}
	return nil
}

// deepFindString 递归找指定键的字符串值（限深度防爆）。
func deepFindString(v any, key string, depth int) string {
	if depth <= 0 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t[key].(string); ok && s != "" {
			return s
		}
		for _, vv := range t {
			if s := deepFindString(vv, key, depth-1); s != "" {
				return s
			}
		}
	case []any:
		for _, vv := range t {
			if s := deepFindString(vv, key, depth-1); s != "" {
				return s
			}
		}
	}
	return ""
}

// pick 第一个非空串。
func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── 刷新 / 额度 / 签到 ───────────────────────────────────

// RefreshToken Cookie 长效，无可刷新令牌（返回不变更）。
func (p *Provider) RefreshToken(_ context.Context, _ *model.Account) (bool, error) {
	return false, nil
}

// Quota 网页通道无公开额度接口：探测登录态 + 本地日计数（图片/视频/对话）。
func (p *Provider) Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error) {
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	healthy := true
	if err := p.validate(qctx, a); err != nil {
		if pe, ok := err.(*provider.Error); ok && pe.Kind == provider.ErrAuth {
			healthy = false
		} else {
			return nil, err
		}
	}
	detail := p.buildQuotaDetail(a.UID)
	c := usageToday(a.UID)
	// 剩余额度取图片/视频两类中较小者（主展示值）；对话不设限
	remaining := int64(-1)
	if r := p.ImageDailyLimit - c.images; p.ImageDailyLimit > 0 && (remaining < 0 || r < remaining) {
		remaining = r
	}
	if r := p.VideoDailyLimit - c.videos; p.VideoDailyLimit > 0 && (remaining < 0 || r < remaining) {
		remaining = r
	}
	if remaining < 0 {
		remaining = 0
	}
	return &model.QuotaInfo{
		Provider: p.ID(), UID: a.UID,
		Used:      float64(c.images + c.videos),
		Limit:     float64(p.ImageDailyLimit + p.VideoDailyLimit),
		Remaining: float64(remaining),
		Unit:      "generations/day", Daily: true,
		Healthy: healthy && remaining > 0,
		Detail:  detail,
	}, nil
}

// Checkin 豆包无签到体系。
func (p *Provider) Checkin(_ context.Context, _ *model.Account) (*model.CheckinResult, error) {
	return nil, provider.ErrNotSupported
}

// ── 模型目录 ─────────────────────────────────────────────

// Models 内置目录：文本（网页模型选择菜单反带）+ Seedream 图片 + Seedance 视频。
// Kind 标注类别，发布编辑器按此分组图片/视频渠道候选。
func (p *Provider) Models(_ context.Context, _ *model.Account) ([]model.ModelInfo, error) {
	out := []model.ModelInfo{
		{ID: "doubao-fast", Name: "豆包 快速", Provider: p.ID(), Kind: "chat",
			ContextWindow: 131072, MaxTokens: 8192},
		{ID: textModelID, Name: "豆包对话（快速·别名）", Provider: p.ID(), Kind: "chat",
			ContextWindow: 131072, MaxTokens: 8192},
		{ID: "doubao-expert", Name: "豆包 专家（深度检索）", Provider: p.ID(), Kind: "chat",
			ContextWindow: 128000, MaxTokens: 8192},
		{ID: "doubao-2.1-lite", Name: "豆包 2.1 Lite", Provider: p.ID(), Kind: "chat",
			ContextWindow: 256000, MaxTokens: 16384},
		{ID: "doubao-2.1-pro", Name: "豆包 2.1 Pro（会员）", Provider: p.ID(), Kind: "chat",
			ContextWindow: 256000, MaxTokens: 16384},
		{ID: "doubao-auto", Name: "豆包 工作任务 Auto", Provider: p.ID(), Kind: "chat",
			ContextWindow: 256000, MaxTokens: 16384},
	}
	for _, m := range imageModels {
		out = append(out, model.ModelInfo{ID: m.Public, Name: m.Upstream, Provider: p.ID(),
			Kind: "image", SupportsImages: true})
	}
	for _, m := range videoModels {
		out = append(out, model.ModelInfo{ID: m.Public, Name: m.Upstream, Provider: p.ID(), Kind: "video"})
	}
	return out, nil
}

// ── 提交与轮询引擎 ───────────────────────────────────────

// submit 提交生成请求：POST /chat/completion（SSE 读到 SSE_ACK 即收流）。
func (p *Provider) submit(ctx context.Context, a *model.Account, payload map[string]any) (*sseAck, error) {
	// ext.fp 注入（buildPayload 留空）
	if ext, ok := payload["ext"].(map[string]any); ok {
		ext["fp"] = fpOf(a)
	}
	ids := p.seedOf(a)
	q := commonQuery(ids, fpOf(a), regionOf(a), msTokenOf(a))
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	extra := map[string]string{
		"agw-js-conv":   "str, str",
		"last-event-id": "undefined",
		"x-flow-trace":  flowTrace(),
		"Accept":        "text/event-stream",
	}
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := p.dbPost(reqCtx, a, "/chat/completion", q, "application/json", body, extra)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			(resp.StatusCode >= 300 && resp.StatusCode < 400) {
			return nil, authExpiredError(resp.StatusCode, string(raw))
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, &provider.Error{Kind: provider.ErrQuota, Status: resp.StatusCode,
				Msg: "上游限流: " + truncateRunes(string(raw), 160)}
		}
		return nil, &provider.Error{Kind: classifyStatus(resp.StatusCode), Status: resp.StatusCode,
			Msg: fmt.Sprintf("提交失败 HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 200))}
	}
	// SSE 增量读：见到 SSE_ACK / STREAM_ERROR 事件（其后跟随空行）即断流，
	// 生成结果改走 /im/chain/single 轮询（Doora 同款策略）
	var sb strings.Builder
	chunk := make([]byte, 16*1024)
	readDone := func() bool {
		s := sb.String()
		for _, marker := range []string{"SSE_ACK", "STREAM_ERROR"} {
			if i := strings.Index(s, marker); i >= 0 && strings.Contains(s[i:], "\n\n") {
				return true
			}
		}
		return sb.Len() > 2<<20 // 防御性上限
	}
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			sb.Write(chunk[:n])
			if readDone() {
				break
			}
		}
		if rerr != nil {
			break // EOF / 超时 / 连接重置：交给 parseSseAck 判定
		}
	}
	return parseSseAck(sb.String())
}

// liveAck submitKeep 的返回：SSE_ACK 解析结果 + 尚未消费的剩余流。
// 上游在 ACK 之后立刻推送文本事件，读 ACK 时缓冲的尾部字节必须续读，不能丢。
type liveAck struct {
	ack  *sseAck
	read *bufio.Reader // 活的响应流（ACK 之后的事件从这里继续读）
	body io.ReadCloser // 原始 Body（调用方负责 Close）
}

// submitKeep 提交对话并保持 SSE 连接：读到 SSE_ACK 即返回（含已缓冲的后续字节），
// 供内联流式解析续读。HTTP ≥300 / STREAM_ERROR 时返回错误并已关闭连接。
func (p *Provider) submitKeep(ctx context.Context, a *model.Account, payload map[string]any) (*liveAck, error) {
	if ext, ok := payload["ext"].(map[string]any); ok {
		ext["fp"] = fpOf(a)
	}
	ids := p.seedOf(a)
	q := commonQuery(ids, fpOf(a), regionOf(a), msTokenOf(a))
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		dbOrigin+"/chat/completion?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range dbHeaders(a, "application/json") {
		req.Header.Set(k, v)
	}
	req.Header.Set("agw-js-conv", "str, str")
	req.Header.Set("last-event-id", "undefined")
	req.Header.Set("x-flow-trace", flowTrace())
	req.Header.Set("Accept", "text/event-stream")
	resp, err := p.streamHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			(resp.StatusCode >= 300 && resp.StatusCode < 400) {
			return nil, authExpiredError(resp.StatusCode, string(raw))
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, &provider.Error{Kind: provider.ErrQuota, Status: resp.StatusCode,
				Msg: "上游限流: " + truncateRunes(string(raw), 160)}
		}
		return nil, &provider.Error{Kind: classifyStatus(resp.StatusCode), Status: resp.StatusCode,
			Msg: fmt.Sprintf("提交失败 HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 200))}
	}
	rd := bufio.NewReader(resp.Body)
	packets, err := readSSEUntilAckPacket(rd)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	ack, err := parseSseAck(packets)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	return &liveAck{ack: ack, read: rd, body: resp.Body}, nil
}

// readSSEUntilAckPacket 逐 packet 读到 SSE_ACK / STREAM_ERROR（含已缓冲的后续字节
// 一并返回，交给 parseSseAck 提取 conversation_id；续读从 rd 接着来）。
// io.EOF 是流正常结束信号，吞掉并返回已收集内容：登录失效时上游返回纯 JSON 错误体
// （无 event/data、无结尾空行），此时 packets 即该 JSON，由 parseSseAck →
// classifyJSONErrorBody 归类为 ErrAuth；若流在 ACK 前断开，parseSseAck 报"缺少 SSE_ACK"。
func readSSEUntilAckPacket(rd *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		pkt, err := readSSEPacket(rd)
		if pkt != "" {
			sb.WriteString(pkt)
			sb.WriteString("\n\n")
		}
		if err == io.EOF {
			return sb.String(), nil
		}
		if err != nil {
			return sb.String(), err
		}
		if ev := sseEventName(pkt); ev == "SSE_ACK" || ev == "STREAM_ERROR" {
			return sb.String(), nil
		}
		if sb.Len() > 4<<20 {
			return sb.String(), fmt.Errorf("SSE_ACK 前响应过大（疑似异常流）")
		}
	}
}

// sseEventName 取 packet 的 event: 字段值。
func sseEventName(pkt string) string {
	for _, line := range strings.Split(pkt, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "event:") {
			return strings.TrimSpace(strings.TrimPrefix(t, "event:"))
		}
	}
	return ""
}

// readSSEPacket 读到空行为止，返回 packet 原文（含换行，行尾 \r 归一）。
func readSSEPacket(rd *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			sb.WriteString(strings.TrimRight(line, "\r"))
			sb.WriteString("\n")
		}
		if err != nil {
			return sb.String(), err
		}
		if strings.TrimRight(line, "\r\n") == "" {
			trimmed := strings.TrimRight(sb.String(), "\n")
			if trimmed == "" {
				sb.Reset() // 连续空行：继续等下一个 packet
				continue
			}
			return trimmed, nil
		}
	}
}

// pollChain 轮询一次会话链。
func (p *Provider) pollChain(ctx context.Context, a *model.Account, conversationID string) (*chainResponse, error) {
	ids := p.seedOf(a)
	q := commonQuery(ids, fpOf(a), regionOf(a), msTokenOf(a))
	body, err := json.Marshal(chainBody(conversationID))
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := p.dbPost(reqCtx, a, "/im/chain/single", q, "application/json; encoding=utf-8", body, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			(resp.StatusCode >= 300 && resp.StatusCode < 400) {
			return nil, authExpiredError(resp.StatusCode, string(raw))
		}
		return nil, fmt.Errorf("轮询失败 HTTP %d: %s", resp.StatusCode, truncateRunes(string(raw), 160))
	}
	var cr chainResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("轮询响应解析失败: %v", err)
	}
	return &cr, nil
}

// sendConfirm 发送确认轮消息（上游反问参数时）。
func (p *Provider) sendConfirm(ctx context.Context, a *model.Account, conversationID, text string) error {
	payload := confirmPayload(conversationID, text)
	_, err := p.submit(ctx, a, payload)
	return err
}

// ── ChatStream（语言模型）────────────────────────────────

// ChatStream OpenAI 兼容对话 → 豆包网页助手。
// 上游是「提交 + 轮询」模型，流式为轮询增量模拟（每次轮询到新文本即推 delta）；
// 非流式则同步跑完整个轮询返回完整 JSON。
func (p *Provider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	rm := resolveModel(modelName)
	var req struct {
		Stream   *bool `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		EnableThinking *bool `json:"enable_thinking"`
		Thinking       *struct {
			Enabled *bool `json:"enabled"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	prompt := flattenMessages(req.Messages)

	// 推理档位：模型表默认档为基准；客户端显式 thinking 开关只在同步模式内切换
	// 快速(0)↔专家(3)档。任务模式（Pro/Lite/Auto）的档位与 agent_mode 绑定，
	// 线上未验证 0 值形态，不跟随覆盖。
	if rm.AgentMode != 1 && (req.EnableThinking != nil || (req.Thinking != nil && req.Thinking.Enabled != nil)) {
		want := req.EnableThinking != nil && *req.EnableThinking
		if req.Thinking != nil && req.Thinking.Enabled != nil {
			want = *req.Thinking.Enabled
		}
		if want {
			rm.DeepThink = 3
		} else {
			rm.DeepThink = 0
		}
	}

	unlock := p.lockAccount(a.UID)
	unlock.Lock()
	defer unlock.Unlock()

	stream := req.Stream != nil && *req.Stream

	// ── 异步任务模式（2.1 Pro/Lite、Auto）：全文落会话链，SSE 只有片段 ──
	// 保持旧路径：提交即断流 + /im/chain/single 轮询。
	if rm.AgentMode == 1 {
		ack, err := p.submit(ctx, a, textSubmitPayload("", prompt, rm))
		if err != nil {
			return nil, err
		}
		countChat(a.UID)
		if !stream {
			text, cerr := p.awaitText(ctx, a, ack.ConversationID, 240*time.Second)
			if cerr != nil {
				return nil, cerr
			}
			out, _ := json.Marshal(openAICompletion(rm.Public, text))
			return &provider.ChatResult{
				Body:        io.NopCloser(bytes.NewReader(out)),
				ContentType: "application/json",
			}, nil
		}
		pr, pw := io.Pipe()
		go func() {
			p.streamText(ctx, a, ack.ConversationID, rm.Public, pw)
		}()
		return &provider.ChatResult{Body: pr, ContentType: "text/event-stream"}, nil
	}

	// ── 同步模式（快速/专家）：文本内联在 SSE 流里，保持连接实时转 chunk ──
	live, err := p.submitKeep(ctx, a, textSubmitPayload("", prompt, rm))
	if err != nil {
		return nil, err
	}
	countChat(a.UID)
	if !stream {
		defer live.body.Close()
		text, ierr := p.inlineAwait(ctx, live.read, 240*time.Second)
		if ierr != nil {
			return nil, ierr
		}
		out, _ := json.Marshal(openAICompletion(rm.Public, text))
		return &provider.ChatResult{
			Body:        io.NopCloser(bytes.NewReader(out)),
			ContentType: "application/json",
		}, nil
	}
	pr, pw := io.Pipe()
	go func() {
		p.streamInline(ctx, live, rm.Public, pw)
	}()
	return &provider.ChatResult{Body: pr, ContentType: "text/event-stream"}, nil
}

// inlineTextAcc 内联 SSE 文本累积器：notify 初始块、chunk patch_object=1 补丁、
// delta 增量三路都带文本分片（实测三路按到达顺序拼出全文），统一顺序拼接。
type inlineTextAcc struct {
	sb    strings.Builder
	brief string // SSE_REPLY_END end_type=1 权威全文
}

func (t *inlineTextAcc) text() string { return t.sb.String() }

// feedPacket 解析一个 SSE packet，返回（新增文本, 是否结束, 错误消息）。
func (t *inlineTextAcc) feedPacket(event, data string) (string, bool, string) {
	switch event {
	case "STREAM_ERROR":
		return "", true, truncateRunes(data, 300)
	case "STREAM_MSG_NOTIFY":
		var j struct {
			Content struct {
				ContentBlock []struct {
					BlockType int `json:"block_type"`
					Content   struct {
						TextBlock struct {
							Text string `json:"text"`
						} `json:"text_block"`
					} `json:"content"`
				} `json:"content_block"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(data), &j) != nil {
			return "", false, ""
		}
		var add string
		for _, b := range j.Content.ContentBlock {
			if b.BlockType == blockTypeText {
				add += b.Content.TextBlock.Text
			}
		}
		return add, false, ""
	case "STREAM_CHUNK":
		var j struct {
			PatchOp []struct {
				PatchObject int `json:"patch_object"`
				PatchValue  struct {
					ContentBlock []struct {
						BlockType int `json:"block_type"`
						Content   struct {
							TextBlock struct {
								Text string `json:"text"`
							} `json:"text_block"`
						} `json:"content"`
					} `json:"content_block"`
				} `json:"patch_value"`
			} `json:"patch_op"`
		}
		if json.Unmarshal([]byte(data), &j) != nil {
			return "", false, ""
		}
		var add string
		for _, po := range j.PatchOp {
			if po.PatchObject != 1 {
				continue // tts/状态类补丁
			}
			for _, b := range po.PatchValue.ContentBlock {
				if b.BlockType == blockTypeText {
					add += b.Content.TextBlock.Text
				}
			}
		}
		return add, false, ""
	case "CHUNK_DELTA":
		var j struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(data), &j) != nil {
			return "", false, ""
		}
		return j.Text, false, ""
	case "SSE_REPLY_END":
		var j struct {
			EndType       int    `json:"end_type"`
			MsgFinishAttr struct {
				Brief string `json:"brief"`
			} `json:"msg_finish_attr"`
		}
		if json.Unmarshal([]byte(data), &j) == nil && j.EndType == 1 {
			t.brief = j.MsgFinishAttr.Brief
			return "", true, ""
		}
		return "", false, ""
	}
	return "", false, ""
}

// reconcile 用权威 brief 校正累积文本，返回需补发的差额。
func (t *inlineTextAcc) reconcile() string {
	acc := t.text()
	switch {
	case t.brief == "":
		return ""
	case t.brief == acc:
		return ""
	case strings.HasPrefix(t.brief, acc):
		return t.brief[len(acc):]
	case acc == "":
		return t.brief
	default:
		// 三路拼接与 brief 对不上（如 brief 被上游截断、分片乱序）：信任已流出的文本。
		return ""
	}
}

// inlineAwait 非流式：读内联 SSE 到结束，返回全文。
func (p *Provider) inlineAwait(ctx context.Context, rd *bufio.Reader, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	t := &inlineTextAcc{}
	for {
		if time.Now().After(deadline) {
			if t.text() != "" || t.brief != "" {
				break
			}
			return "", &provider.Error{Kind: provider.ErrUpstream, Msg: "豆包回复超时"}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		pkt, err := readSSEPacket(rd)
		if pkt != "" {
			event, data := splitSSEPacket(pkt)
			add, done, errMsg := t.feedPacket(event, data)
			t.sb.WriteString(add)
			if errMsg != "" {
				if kw, quota := hitFailKeyword(errMsg); kw != "" {
					kind := provider.ErrUpstream
					if quota {
						kind = provider.ErrQuota
					}
					return "", &provider.Error{Kind: kind, Msg: "上游失败: " + truncateRunes(errMsg, 300)}
				}
				return "", &provider.Error{Kind: provider.ErrUpstream, Msg: "上游流内错误: " + truncateRunes(errMsg, 300)}
			}
			if done {
				break
			}
		}
		if err != nil {
			break // EOF：有文本就用
		}
	}
	text := t.text()
	if len(t.brief) > len(text) && strings.HasPrefix(t.brief, text) {
		text = t.brief
	}
	if text == "" && t.brief != "" {
		text = t.brief
	}
	if text == "" {
		return "", &provider.Error{Kind: provider.ErrUpstream, Msg: "豆包回复为空"}
	}
	if kw, quota := hitFailKeyword(text); kw != "" {
		kind := provider.ErrUpstream
		if quota {
			kind = provider.ErrQuota
		}
		return "", &provider.Error{Kind: kind, Msg: "上游失败: " + truncateRunes(text, 300)}
	}
	return text, nil
}

// splitSSEPacket 拆 packet 为 event 名与合并后的 data 文本。
func splitSSEPacket(pkt string) (string, string) {
	var event, data strings.Builder
	for _, line := range strings.Split(pkt, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "event:"):
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(t, "event:")))
		case strings.HasPrefix(t, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(t, "data:")))
		}
	}
	return event.String(), data.String()
}

// streamInline 同步模式流式：内联 SSE → OpenAI chunk（三路文本按到达顺序推 delta，
// 结束时用 brief 校正补尾）。负责关闭 live.body 与 pw。
func (p *Provider) streamInline(ctx context.Context, live *liveAck, modelName string, pw *io.PipeWriter) {
	defer live.body.Close()
	defer pw.CloseWithError(nil)

	chunkID := "chatcmpl-doubao-" + live.ack.ConversationID
	created := time.Now().Unix()
	emit := func(delta map[string]any, finish string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk := map[string]any{
			"id": chunkID, "object": "chat.completion.chunk", "model": modelName,
			"created": created, "choices": []any{choice},
		}
		raw, _ := json.Marshal(chunk)
		_, _ = pw.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
	}
	fail := func(msg string) {
		errPayload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}})
		_, _ = pw.Write(append(append([]byte("data: "), errPayload...), '\n', '\n'))
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
	}

	t := &inlineTextAcc{}
	deadline := time.Now().Add(240 * time.Second)
	for {
		if time.Now().After(deadline) {
			fail("豆包回复超时")
			return
		}
		if ctx.Err() != nil {
			return // 客户端断开：静默收尾
		}
		pkt, err := readSSEPacket(live.read)
		if pkt != "" {
			event, data := splitSSEPacket(pkt)
			add, done, errMsg := t.feedPacket(event, data)
			if errMsg != "" {
				fail("上游失败：" + errMsg)
				return
			}
			if add != "" {
				if kw, _ := hitFailKeyword(add); kw != "" {
					fail("上游失败：" + truncateRunes(t.text()+add, 200))
					return
				}
				t.sb.WriteString(add)
				emit(map[string]any{"content": add}, "")
			}
			if done {
				if tail := t.reconcile(); tail != "" {
					emit(map[string]any{"content": tail}, "")
				}
				emit(map[string]any{}, "stop")
				_, _ = pw.Write([]byte("data: [DONE]\n\n"))
				return
			}
		}
		if err != nil {
			// 连接提前断开：有文本就体面收尾
			if t.text() != "" || t.brief != "" {
				if tail := t.reconcile(); tail != "" {
					emit(map[string]any{"content": tail}, "")
				}
				emit(map[string]any{}, "stop")
				_, _ = pw.Write([]byte("data: [DONE]\n\n"))
				return
			}
			fail("豆包连接中断")
			return
		}
	}
}

// streamText 轮询文本并增量写 OpenAI SSE（出错时写错误事件后 [DONE]）。
func (p *Provider) streamText(ctx context.Context, a *model.Account, conversationID, modelName string, w io.Writer) {
	chunkID := "chatcmpl-doubao-" + conversationID
	created := time.Now().Unix()
	emit := func(delta map[string]any, finish string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk := map[string]any{
			"id": chunkID, "object": "chat.completion.chunk", "model": modelName,
			"created": created, "choices": []any{choice},
		}
		raw, _ := json.Marshal(chunk)
		_, _ = w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
	}
	sent := ""
	stable := 0
	deadline := time.Now().Add(240 * time.Second)
	confirmRounds := 0
	fail := func(msg string) {
		errPayload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}})
		_, _ = w.Write(append(append([]byte("data: "), errPayload...), '\n', '\n'))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	for time.Now().Before(deadline) {
		cr, err := p.pollChain(ctx, a, conversationID)
		if err != nil {
			if pe, ok := err.(*provider.Error); ok && pe.Kind == provider.ErrAuth {
				fail(err.Error())
				return
			}
			time.Sleep(3 * time.Second)
			continue
		}
		if readClarifyState(cr) && confirmRounds < 4 {
			confirmRounds++
			_ = p.sendConfirm(ctx, a, conversationID, "确认")
			time.Sleep(2 * time.Second)
			continue
		}
		text := parseConversationText(cr)
		if text != "" {
			if kw, _ := hitFailKeyword(text); kw != "" {
				fail("上游失败：" + text)
				return
			}
			if len(text) > len(sent) {
				delta := text[len(sent):]
				sent = text
				stable = 0
				emit(map[string]any{"content": delta}, "")
			} else if len(text) == len(sent) && sent != "" {
				// 文本连续两轮不变视为完成（豆包文本通常整段刷新）
				stable++
				if stable >= 2 {
					emit(map[string]any{}, "stop")
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2500 * time.Millisecond):
		}
	}
	fail("豆包回复超时")
}

// awaitText 阻塞轮询直到助手文本稳定（或超时）。
func (p *Provider) awaitText(ctx context.Context, a *model.Account, conversationID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	sent, stable := "", 0
	confirmRounds := 0
	for time.Now().Before(deadline) {
		cr, err := p.pollChain(ctx, a, conversationID)
		if err != nil {
			if pe, ok := err.(*provider.Error); ok && pe.Kind == provider.ErrAuth {
				return "", err
			}
			time.Sleep(3 * time.Second)
			continue
		}
		if readClarifyState(cr) && confirmRounds < 4 {
			confirmRounds++
			_ = p.sendConfirm(ctx, a, conversationID, "确认")
			time.Sleep(2 * time.Second)
			continue
		}
		text := parseConversationText(cr)
		if text != "" {
			if kw, quota := hitFailKeyword(text); kw != "" {
				kind := provider.ErrUpstream
				if quota {
					kind = provider.ErrQuota
				}
				return "", &provider.Error{Kind: kind, Msg: "上游失败: " + truncateRunes(text, 300)}
			}
			if text == sent {
				stable++
				if stable >= 2 {
					return sent, nil
				}
			} else {
				sent, stable = text, 0
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2500 * time.Millisecond):
		}
	}
	if sent != "" {
		return sent, nil
	}
	return "", &provider.Error{Kind: provider.ErrUpstream, Msg: "豆包回复超时"}
}

// flattenMessages OpenAI messages → 单条用户文本（网页协议无多轮结构，历史平铺）。
func flattenMessages(messages []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}) string {
	var parts []string
	hasSystem := false
	for _, m := range messages {
		text := flattenContent(m.Content)
		if text == "" {
			continue
		}
		if m.Role == "system" || m.Role == "developer" {
			hasSystem = true
			parts = append(parts, "system:\n\n"+text)
			continue
		}
		role := m.Role
		if role != "assistant" && role != "user" {
			role = "user"
		}
		parts = append(parts, role+": "+text)
	}
	joined := strings.Join(parts, "\n\n")
	if !hasSystem && joined != "" {
		joined = "system:\n\n" + joined
	}
	if joined == "" {
		joined = "你好"
	}
	return joined
}

// flattenContent OpenAI content（字符串或分段数组）→ 纯文本。
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
		for _, part := range arr {
			if part.Text != "" {
				out = append(out, part.Text)
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// openAICompletion 非流式响应体。
func openAICompletion(modelName, text string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-doubao-" + randomLocalId(),
		"object":  "chat.completion",
		"created": timeNowMs() / 1000,
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
}
