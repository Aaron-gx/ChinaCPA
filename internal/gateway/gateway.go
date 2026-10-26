// Package gateway OpenAI 兼容网关：/v1/models 与 /v1/chat/completions。
// 模型路由（provider 前缀或裸名模糊匹配）、多账号轮询失败转移、
// SSE 透传/转换、流内 usage 提取落账。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/pool"
	"chinacpa/internal/provider"
	"chinacpa/internal/reqlog"
	"chinacpa/internal/usage"
)

// Gateway 网关。
type Gateway struct {
	pool       *pool.Pool
	usage      *usage.Tracker
	qwenProv   qwenCounter
	rlog       *reqlog.DB
	videoTasks *videoTaskStore // 视频生成任务表（内存态）
}

// qwenCounter Qwen 本地请求计数的最小接口（解耦）。
type qwenCounter interface {
	CountRequest(uid string)
}

// New 构造。
func New(p *pool.Pool, u *usage.Tracker) *Gateway {
	return &Gateway{pool: p, usage: u, videoTasks: &videoTaskStore{tasks: map[string]*videoTask{}}}
}

// SetQwenCounter 注入 Qwen 计数器。
func (g *Gateway) SetQwenCounter(c qwenCounter) { g.qwenProv = c }

// SetReqLog 注入请求日志（nil = 不记录）。
func (g *Gateway) SetReqLog(l *reqlog.DB) { g.rlog = l }

// logReq 记录一条请求日志（费用按对外模型单价估算）。
func (g *Gateway) logReq(proto, pubModel, pid, uid string, stream bool, u *usageSnapshot, ok bool, errMsg string, attempts []string, firstMS, totalMS int64, reported, finish string) {
	if g.rlog == nil {
		return
	}
	e := reqlog.Entry{
		Protocol: proto, Model: pubModel, Provider: pid, Account: uid,
		Stream: stream, OK: ok, Error: errMsg, Attempts: attempts,
		FirstMS: firstMS, TotalMS: totalMS, ReportedModel: reported, Finish: finish,
	}
	if u != nil {
		e.PromptTokens = u.PromptTokens
		e.CompletionTokens = u.CompletionTokens
		e.TotalTokens = u.TotalTokens
		e.CacheTokens = u.cacheTokens()
		e.CostCNY = g.usage.CostWithCache(pubModel, e.PromptTokens, e.CompletionTokens, e.CacheTokens)
	}
	g.rlog.Add(e)
}

// Register 挂载到 mux（调用方保证 /v1 前缀）。
func (g *Gateway) Register(mux *http.ServeMux, authKey string) {
	mux.HandleFunc("/v1/models", g.auth(g.handleModels, authKey))
	mux.HandleFunc("/v1/chat/completions", g.auth(g.handleChat, authKey))
	mux.HandleFunc("/v1/responses", g.auth(g.handleResponses, authKey))
	mux.HandleFunc("/v1/messages", g.auth(g.handleClaudeMessages, authKey))
	g.registerMedia(mux, authKey) // 图片 / 视频（含火山方舟 /api/v3 路径）
}

func (g *Gateway) auth(next http.HandlerFunc, key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key != "" {
			got := r.Header.Get("Authorization")
			got = strings.TrimPrefix(got, "Bearer ")
			if got != key {
				// 兼容 x-api-key（Anthropic 客户端习惯）
				if r.Header.Get("x-api-key") != key {
					writeErr(w, http.StatusUnauthorized, "invalid_api_key", "API 密钥错误（配置文件 api_key）")
					return
				}
			}
		}
		next(w, r)
	}
}

// ── /v1/models ───────────────────────────────────────────

// openAIStream 一次成功的 provider 调用（body 恒为 OpenAI 兼容 SSE 流）。
type openAIStream struct {
	acct     *model.Account
	pid      string
	body     io.ReadCloser
	attempts []string // 渠道尝试链路（含失败转移记录）
}

// peekStreamError 探测「HTTP 200 但错误内嵌在流里」的情况：
// 部分平台（Qoder 等）上游出错时不返回非 2xx，而是返回 200 + 错误事件流，
// 若不探测会吞掉跨渠道失败转移。返回消费掉的原始字节（需回放）、接续 reader
// （bufio 缓冲中可能已包含后续数据）、是否错误、错误消息。
func peekStreamError(rc io.ReadCloser) (peeked []byte, cont io.Reader, isErr bool, msg string) {
	br := bufio.NewReader(rc)
	var firstData []byte
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			peeked = append(peeked, line...)
			t := strings.TrimSpace(string(line))
			if t != "" {
				if strings.HasPrefix(t, "data:") {
					payload := strings.TrimSpace(strings.TrimPrefix(t, "data:"))
					if payload != "" && payload != "[DONE]" {
						firstData = []byte(payload)
						break
					}
				} else if strings.HasPrefix(t, "{") {
					// 非 SSE 的单行 JSON（非流式响应）
					firstData = []byte(t)
					break
				} else if !strings.HasPrefix(t, "event:") && !strings.HasPrefix(t, "retry:") && !strings.HasPrefix(t, ":") {
					break // 未知行，放行
				}
			}
		}
		if err != nil {
			break
		}
	}
	if firstData == nil {
		return peeked, br, false, ""
	}
	var probe struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(firstData, &probe) == nil && probe.Error != nil {
		return peeked, br, true, probe.Error.Message
	}
	return peeked, br, false, ""
}

// replayRC 回放探测期消费的字节后接续原流。
type replayRC struct {
	r      io.Reader
	closer io.Closer
}

func (p *replayRC) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *replayRC) Close() error               { return p.closer.Close() }

// sessionKeyOf 从请求体提取会话标识（黏性选号用）：
// OpenAI 的 prompt_cache_key / user，Claude 的 metadata.user_id。
func sessionKeyOf(body []byte) string {
	var probe struct {
		PromptCacheKey string `json:"prompt_cache_key"`
		User           string `json:"user"`
		Metadata       struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	if probe.PromptCacheKey != "" {
		return "pck:" + probe.PromptCacheKey
	}
	if probe.Metadata.UserID != "" {
		return "uid:" + probe.Metadata.UserID
	}
	if probe.User != "" {
		return "user:" + probe.User
	}
	return ""
}

// tryProviderStream 单渠道调用：该 provider 内账号轮询直到成功。
// 失败返回聚合错误信息与账号级尝试轨迹（不写响应，供上层跨渠道重试）。
// trail 形如 "a4ad5f80 ✗429…; f029fc7a ✗429…"，写入请求日志的失败转移链路，
// 让「渠道内切账号」在面板上可见。
func (g *Gateway) tryProviderStream(ctx context.Context, pid, mname string, body []byte, sessionKey string) (*openAIStream, *provider.Error, int, []byte, string) {
	pr, ok := g.pool.Provider(pid)
	if !ok {
		return nil, &provider.Error{Kind: provider.ErrClient, Msg: "未知平台 " + pid}, 0, nil, ""
	}
	var lastStatus int
	var lastBody []byte
	var lastErr *provider.Error
	var failTrail []string
	addTrail := func(uid string, note string) {
		failTrail = append(failTrail, uidShort(uid)+" ✗"+note)
	}
	tried := map[string]bool{}
	for {
		a, perr := g.pool.Pick(pid, mname, sessionKey)
		if perr != nil {
			break
		}
		if tried[a.UID] {
			break
		}
		tried[a.UID] = true
		if _, terr := g.pool.EnsureFreshToken(ctx, a); terr != nil {
			lastErr = provider.AsError(terr)
			addTrail(a.UID, lastErr.Error())
			continue
		}
		res, cerr := pr.ChatStream(ctx, a, mname, body)
		if cerr != nil {
			g.pool.MarkError(a, mname, cerr)
			lastErr = provider.AsError(cerr)
			addTrail(a.UID, lastErr.Error())
			continue
		}
		if res.Err != nil || res.Status >= 400 {
			note := ""
			if res.Err != nil {
				g.pool.MarkError(a, mname, res.Err)
				lastErr = res.Err
				note = res.Err.Error()
			} else {
				note = fmt.Sprintf("HTTP %d", res.Status)
				if lastErr == nil {
					lastErr = &provider.Error{Kind: provider.ErrUpstream, Status: res.Status, Msg: note}
				}
			}
			if res.Body != nil {
				res.Body.Close()
			}
			lastStatus, lastBody = res.Status, res.RespBody
			addTrail(a.UID, note)
			continue
		}
		// 200 但错误内嵌流首行（Qoder 等）：探测以实现账号/渠道级失败转移
		if res.Body != nil {
			peeked, cont, isErr, emsg := peekStreamError(res.Body)
			if isErr {
				pe := &provider.Error{Kind: provider.ErrUpstream, Msg: "上游流内错误: " + emsg}
				g.pool.MarkError(a, mname, pe)
				lastErr = pe
				res.Body.Close()
				addTrail(a.UID, pe.Error())
				continue
			}
			if len(peeked) > 0 {
				res.Body = &replayRC{r: io.MultiReader(bytes.NewReader(peeked), cont), closer: res.Body}
			}
		}
		g.pool.MarkSuccess(a, mname)
		if pid == "qwen" && g.qwenProv != nil {
			g.qwenProv.CountRequest(a.UID)
		}
		trail := ""
		if len(failTrail) > 0 {
			trail = "账号 " + uidShort(a.UID) + "（跳过: " + strings.Join(failTrail, "; ") + "）"
		}
		return &openAIStream{acct: a, pid: pid, body: res.Body}, nil, 0, nil, trail
	}
	if lastErr == nil {
		lastErr = &provider.Error{Kind: provider.ErrUpstream, Msg: fmt.Sprintf("平台 %s 无可用账号", pid)}
	}
	trail := ""
	if len(failTrail) > 0 {
		trail = "账号尝试: " + strings.Join(failTrail, "; ")
	}
	return nil, lastErr, lastStatus, lastBody, trail
}

// uidShort 账号短标识（日志用）。
func uidShort(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// callRoutedStream 多渠道失败转移：按路由顺序尝试渠道，每渠道内部再账号轮询。
// proto: chat / responses / messages（请求日志用）。失败时已写错误响应，返回 nil。
func (g *Gateway) callRoutedStream(ctx context.Context, w http.ResponseWriter, route *pool.Route, body []byte, proto string, sessionKey string) *openAIStream {
	t0 := time.Now()
	var lastErr *provider.Error
	var lastStatus int
	var lastBody []byte
	var attempts []string
	for _, ch := range route.Channels {
		st, err, status, respBody, trail := g.tryProviderStream(ctx, ch.PID, ch.Model, body, sessionKey)
		if st != nil {
			g.bumpChannel(route, ch, true)
			st.attempts = append(attempts, reqlog.Attempt(ch.PID, ch.Model, true, trail))
			return st
		}
		g.bumpChannel(route, ch, false)
		note := ""
		if err != nil {
			note = err.Error()
		}
		if trail != "" {
			if note != "" {
				note += "｜"
			}
			note += trail
		}
		attempts = append(attempts, reqlog.Attempt(ch.PID, ch.Model, false, note))
		lastErr, lastStatus, lastBody = err, status, respBody
	}
	msg := "上游调用失败"
	if lastErr != nil && lastErr.Error() != "" {
		msg = lastErr.Error()
	} else if len(lastBody) > 0 {
		msg = string(lastBody[:min(300, len(lastBody))])
	}
	status := http.StatusBadGateway
	if lastStatus > 0 {
		status = lastStatus
	}
	writeErr(w, status, "upstream_error", msg)
	g.logReq(proto, route.Public, "", "", false, nil, false, msg, attempts, 0, time.Since(t0).Milliseconds(), "", "")
	return nil
}

// bumpChannel 发布模型渠道调用计数。
func (g *Gateway) bumpChannel(route *pool.Route, ch pool.ChannelTarget, ok bool) {
	if !route.Published {
		return
	}
	if pm := g.pool.Published(); pm != nil {
		pm.BumpChannel(route.Public, ch.PID, ch.Model, ok)
	}
}

// callOpenAIStream 兼容旧签名：单渠道调用（失败已写响应）。
func (g *Gateway) callOpenAIStream(ctx context.Context, w http.ResponseWriter, pid, mname string, body []byte) *openAIStream {
	route := &pool.Route{Public: mname, Channels: []pool.ChannelTarget{{PID: pid, Model: mname}}}
	return g.callRoutedStream(ctx, w, route, body, "chat", sessionKeyOf(body))
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	type om struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	// 发布模型优先：对外只暴露用户配置的模型
	if pm := g.pool.Published(); pm != nil && !pm.Empty() {
		data := []om{}
		for _, mo := range pm.Get().Models {
			owned := "chinacpa"
			if len(mo.Channels) > 0 {
				owned = mo.Channels[0].Provider
			}
			data = append(data, om{ID: mo.ID, Object: "model", OwnedBy: owned})
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
		return
	}
	all, err := g.pool.AllModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "models_error", err.Error())
		return
	}
	data := []om{}
	seen := map[string]bool{}
	for pid, ms := range all {
		if ms == nil {
			continue
		}
		for _, m := range ms {
			// 带前缀形态（路由唯一）
			id := pid + ":" + m.ID
			if !seen[id] {
				seen[id] = true
				data = append(data, om{ID: id, Object: "model", OwnedBy: pid})
			}
			// 裸名形态（跨平台唯一时才暴露）
			if !seen[m.ID] {
				seen[m.ID] = true
				data = append(data, om{ID: m.ID, Object: "model", OwnedBy: pid})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ── /v1/chat/completions ─────────────────────────────────

type chatReq struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Body   json.RawMessage `json:"-"`
}

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	var cr chatReq
	if err := json.Unmarshal(body, &cr); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON")
		return
	}
	route, err := g.pool.ResolveRoute(r.Context(), cr.Model)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "model_not_found", err.Error())
		return
	}
	// 类型错配守卫：图片/视频模型不能走对话端点（正确调用方式见提示）
	switch route.Type {
	case "image":
		writeErr(w, http.StatusBadRequest, "wrong_endpoint",
			"模型 "+route.Public+" 是图片生成模型：请用 POST /v1/images/generations（body 含 prompt），不是 /v1/chat/completions")
		return
	case "video":
		writeErr(w, http.StatusBadRequest, "wrong_endpoint",
			"模型 "+route.Public+" 是视频生成模型：请用 POST /api/v3/contents/generations/tasks（火山方舟任务协议），不是 /v1/chat/completions")
		return
	}
	t0 := time.Now()
	st := g.callRoutedStream(r.Context(), w, route, body, "chat", sessionKeyOf(body))
	if st == nil {
		return
	}
	res := &provider.ChatResult{Body: st.body, ContentType: "text/event-stream"}
	g.serve(w, r, res, st.pid, st.acct.UID, route.Public, cr.Stream, "chat", st.attempts, t0)
}

// serve 写出响应并提取 usage 落账。
// serve 写出响应并提取 usage 落账 + 请求日志。
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, res *provider.ChatResult, pid, uid, model string, wantStream bool, proto string, attempts []string, start time.Time) {
	ct := res.ContentType
	if ct == "" {
		ct = "application/json"
	}
	if strings.Contains(ct, "event-stream") || wantStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		mr := newModelRewriter(res.Body, model)
		rc := &usageRecorder{
			// modelRewriter：把响应自报的 model 改写回请求名（上游渠道返回真实模型 ID，
			// 如发布模型 deepseek-v4.1-flash 经 Qoder 渠道会自报 dfmodel，客户端会校验不一致）
			src:   mr,
			pid:   pid,
			uid:   uid,
			model: model,
			g:     g,
			start: start,
		}
		defer rc.Close()
		func() {
			defer func() {
				// 防御：流式路径的 panic 不再静默掐断连接（记录到 stderr，且其后的请求日志仍会执行）
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "gateway: stream panic: %v\n", r)
				}
			}()
			buf := make([]byte, 32*1024)
			for {
				n, err := rc.Read(buf)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil {
						return
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
				if err != nil {
					return
				}
			}
		}()
		g.logReq(proto, model, pid, uid, true, rc.captured, true, "", attempts, rc.firstMS, time.Since(start).Milliseconds(), mr.reported, rc.finish)
		return
	}
	// 非流式 JSON
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	res.Body.Close()
	var probeObj struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	reported := ""
	finish := ""
	if json.Unmarshal(raw, &probeObj) == nil {
		reported = probeObj.Model
		for _, ch := range probeObj.Choices {
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
		}
	}
	raw = rewriteModelJSON(raw, model)
	w.Write(raw)
	// 提取 usage 落账（openai 形态）
	var resp struct {
		Model string         `json:"model"`
		Usage *usageSnapshot `json:"usage"`
	}
	totalMS := time.Since(start).Milliseconds()
	if json.Unmarshal(raw, &resp) == nil && resp.Usage != nil {
		g.record(pid, uid, model, resp.Usage)
		g.logReq(proto, model, pid, uid, false, resp.Usage, true, "", attempts, totalMS, totalMS, reported, finish)
	} else {
		g.logReq(proto, model, pid, uid, false, nil, true, "", attempts, totalMS, totalMS, reported, finish)
	}
}

// modelRewriter 响应流改写器：把 OpenAI SSE 行里的顶层 model 字段改写为请求的对外模型名。
// 上游渠道会自报真实模型 ID（如 Qoder 的 dfmodel），与客户端请求名不一致时严格客户端会报
// model mismatch。仅改写成功解析的 JSON 行，内容增量行零开销透传。
type modelRewriter struct {
	sc       *bufio.Scanner
	src      io.ReadCloser
	want     string
	reported string // 上游自报的模型名（改写前捕获，请求日志用）
	out      []byte
	done     bool
}

func newModelRewriter(src io.ReadCloser, want string) *modelRewriter {
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	return &modelRewriter{sc: sc, src: src, want: want}
}

func (m *modelRewriter) Read(p []byte) (int, error) {
	for len(m.out) == 0 {
		if m.done {
			return 0, io.EOF
		}
		if !m.sc.Scan() {
			m.done = true
			return 0, io.EOF
		}
		m.out = append(m.out, m.rewriteLine(m.sc.Bytes())...)
		m.out = append(m.out, '\n')
	}
	n := copy(p, m.out)
	m.out = m.out[n:]
	return n, nil
}

func (m *modelRewriter) Close() error { return m.src.Close() }

func (m *modelRewriter) rewriteLine(line []byte) []byte {
	if !bytes.Contains(line, []byte(`"model"`)) {
		return line
	}
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(t, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return line
	}
	if cur, ok := obj["model"].(string); ok {
		if m.reported == "" {
			m.reported = cur
		}
		if cur != m.want {
			obj["model"] = m.want
			if raw, err := json.Marshal(obj); err == nil {
				return append([]byte("data: "), raw...)
			}
		}
	}
	return line
}

// rewriteModelJSON 非流式响应：改写顶层 model 字段为请求的对外模型名。
func rewriteModelJSON(raw []byte, want string) []byte {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	if cur, ok := obj["model"].(string); ok && cur != want {
		obj["model"] = want
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
	}
	return raw
}

// usageSnapshot OpenAI 形态 usage 对象（含缓存命中字段，兼容多上游形状）。
type usageSnapshot struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	PromptDetails    *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CacheReadTokens int64 `json:"cache_read_input_tokens"`
}

// cacheTokens 缓存命中的输入 tokens。
func (u *usageSnapshot) cacheTokens() int64 {
	if u.PromptDetails != nil && u.PromptDetails.CachedTokens > 0 {
		return u.PromptDetails.CachedTokens
	}
	return u.CacheReadTokens
}

func (g *Gateway) record(pid, uid, model string, u *usageSnapshot) {
	if u == nil {
		g.usage.Record(usage.Record{Provider: pid, Account: uid, Model: model})
		return
	}
	g.usage.Record(usage.Record{
		Provider: pid, Account: uid, Model: model,
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
		TotalTokens: u.TotalTokens, CacheTokens: u.cacheTokens(),
	})
}

// usageRecorder 包装响应流：按行扫 usage（OpenAI chunk.usage / 已转换流），
// 同时记录首字延迟、捕获的 usage 与结束原因（请求日志用）。
type usageRecorder struct {
	src      io.ReadCloser
	pid      string
	uid      string
	model    string
	g        *Gateway
	pending  []byte
	recorded atomic.Bool
	start    time.Time
	firstMS  int64
	captured *usageSnapshot
	reported string
	finish   string
}

func (u *usageRecorder) Read(p []byte) (int, error) {
	n, err := u.src.Read(p)
	if n > 0 {
		if u.firstMS == 0 && !u.start.IsZero() {
			u.firstMS = time.Since(u.start).Milliseconds()
		}
		u.pending = append(u.pending, p[:n]...)
		// 按行切出完整 JSON 行
		for {
			i := indexByte(u.pending, '\n')
			if i < 0 {
				break
			}
			line := string(u.pending[:i])
			u.pending = u.pending[i+1:]
			u.scanLine(line)
		}
		// 防爆内存：pending 超大时丢弃头部
		if len(u.pending) > 1<<20 {
			u.pending = u.pending[len(u.pending)-64<<10:]
		}
	}
	if err != nil && !u.recorded.Load() {
		// 流结束兜底：若无 usage 行，记一次请求数（0 token）
		u.g.usage.Record(usage.Record{Provider: u.pid, Account: u.uid, Model: u.model})
		u.recorded.Store(true)
	}
	return n, err
}

func (u *usageRecorder) scanLine(line string) {
	if u.recorded.Load() {
		return
	}
	trimmed := strings.TrimSpace(line)
	// 兼容两种形态：SSE 行（data: {...}）与 provider 聚合返回的裸 JSON 单行
	// （trae 等平台客户端非流式时上游整体聚合为一条 JSON，Content-Type 被上层
	// 统一标成 event-stream，但行本身无 data: 前缀）。
	payload := trimmed
	if strings.HasPrefix(trimmed, "data:") {
		payload = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	} else if !strings.HasPrefix(trimmed, "{") {
		return
	}
	if payload == "" || payload == "[DONE]" {
		return
	}
	var chunk struct {
		Usage   *usageSnapshot `json:"usage"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			u.finish = *ch.FinishReason
		}
	}
	if chunk.Usage == nil {
		return
	}
	if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 || chunk.Usage.TotalTokens > 0 {
		u.g.record(u.pid, u.uid, u.model, chunk.Usage)
		u.captured = chunk.Usage
		u.recorded.Store(true)
	}
}

func (u *usageRecorder) Close() error {
	if !u.recorded.Load() {
		u.g.usage.Record(usage.Record{Provider: u.pid, Account: u.uid, Model: u.model})
		u.recorded.Store(true)
	}
	return u.src.Close()
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// ── 响应工具 ─────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj)
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": typ},
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
