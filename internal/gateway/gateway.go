// Package gateway OpenAI 兼容网关：/v1/models 与 /v1/chat/completions。
// 模型路由（provider 前缀或裸名模糊匹配）、多账号轮询失败转移、
// SSE 透传/转换、流内 usage 提取落账。
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"chinacpa/internal/model"
	"chinacpa/internal/pool"
	"chinacpa/internal/provider"
	"chinacpa/internal/usage"
)

// Gateway 网关。
type Gateway struct {
	pool     *pool.Pool
	usage    *usage.Tracker
	qwenProv qwenCounter
}

// qwenCounter Qwen 本地请求计数的最小接口（解耦）。
type qwenCounter interface {
	CountRequest(uid string)
}

// New 构造。
func New(p *pool.Pool, u *usage.Tracker) *Gateway {
	return &Gateway{pool: p, usage: u}
}

// SetQwenCounter 注入 Qwen 计数器。
func (g *Gateway) SetQwenCounter(c qwenCounter) { g.qwenProv = c }

// Register 挂载到 mux（调用方保证 /v1 前缀）。
func (g *Gateway) Register(mux *http.ServeMux, authKey string) {
	mux.HandleFunc("/v1/models", g.auth(g.handleModels, authKey))
	mux.HandleFunc("/v1/chat/completions", g.auth(g.handleChat, authKey))
	mux.HandleFunc("/v1/responses", g.auth(g.handleResponses, authKey))
	mux.HandleFunc("/v1/messages", g.auth(g.handleClaudeMessages, authKey))
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
	acct *model.Account
	pid  string
	body io.ReadCloser
}

// callOpenAIStream 失败转移调用：轮询 provider 可用账号直到成功。
// 请求体必须 stream=true；失败时已写错误响应，返回 nil。
func (g *Gateway) callOpenAIStream(ctx context.Context, w http.ResponseWriter, pid, mname string, body []byte) *openAIStream {
	pr, ok := g.pool.Provider(pid)
	if !ok {
		writeErr(w, http.StatusBadRequest, "provider_missing", pid)
		return nil
	}
	var lastStatus int
	var lastBody []byte
	var lastErr *provider.Error
	tried := map[string]bool{}
	for {
		a, perr := g.pool.Pick(pid)
		if perr != nil {
			break
		}
		if tried[a.UID] {
			break
		}
		tried[a.UID] = true
		if _, terr := g.pool.EnsureFreshToken(ctx, a); terr != nil {
			lastErr = provider.AsError(terr)
			continue
		}
		res, cerr := pr.ChatStream(ctx, a, mname, body)
		if cerr != nil {
			g.pool.MarkError(a, cerr)
			lastErr = provider.AsError(cerr)
			continue
		}
		if res.Err != nil || res.Status >= 400 {
			if res.Err != nil {
				g.pool.MarkError(a, res.Err)
				lastErr = res.Err
			}
			if res.Body != nil {
				res.Body.Close()
			}
			lastStatus, lastBody = res.Status, res.RespBody
			continue
		}
		g.pool.MarkSuccess(a)
		if pid == "qwen" && g.qwenProv != nil {
			g.qwenProv.CountRequest(a.UID)
		}
		return &openAIStream{acct: a, pid: pid, body: res.Body}
	}
	msg := "上游调用失败"
	if lastErr != nil {
		msg = lastErr.Error()
	} else if len(lastBody) > 0 {
		msg = string(lastBody[:min(300, len(lastBody))])
	}
	status := http.StatusBadGateway
	if lastStatus > 0 {
		status = lastStatus
	}
	writeErr(w, status, "upstream_error", msg)
	return nil
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	all, err := g.pool.AllModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "models_error", err.Error())
		return
	}
	type om struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
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
	pid, mname, err := g.pool.ResolveModel(r.Context(), cr.Model)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "model_not_found", err.Error())
		return
	}
	pr, ok := g.pool.Provider(pid)
	if !ok {
		writeErr(w, http.StatusBadRequest, "provider_missing", pid)
		return
	}

	// 失败转移：最多尝试该 provider 的全部可用账号
	var lastStatus int
	var lastBody []byte
	var lastErr *provider.Error
	tried := map[string]bool{}
	for {
		a, perr := g.pool.Pick(pid)
		if perr != nil {
			break
		}
		if tried[a.UID] {
			break // 一轮已过
		}
		tried[a.UID] = true
		// 临期 token 刷新
		if _, terr := g.pool.EnsureFreshToken(r.Context(), a); terr != nil {
			lastErr = provider.AsError(terr)
			continue
		}
		res, cerr := pr.ChatStream(r.Context(), a, mname, body)
		if cerr != nil {
			g.pool.MarkError(a, cerr)
			lastErr = provider.AsError(cerr)
			continue
		}
		if res.Err != nil || res.Status >= 400 {
			if res.Err != nil {
				g.pool.MarkError(a, res.Err)
				lastErr = res.Err
			}
			if res.Body != nil {
				res.Body.Close()
			}
			lastStatus, lastBody = res.Status, res.RespBody
			continue
		}
		// 成功：流式/非流式写出 + 记账
		g.pool.MarkSuccess(a)
		if pid == "qwen" && g.qwenProv != nil {
			g.qwenProv.CountRequest(a.UID)
		}
		g.serve(w, r, res, pid, a.UID, cr.Model, cr.Stream)
		return
	}
	// 全账号失败
	msg := "上游调用失败"
	if lastErr != nil {
		msg = lastErr.Error()
	} else if len(lastBody) > 0 {
		msg = string(lastBody[:min(300, len(lastBody))])
	}
	status := http.StatusBadGateway
	if lastStatus > 0 {
		status = lastStatus
	}
	writeErr(w, status, "upstream_error", msg)
}

// serve 写出响应并提取 usage 落账。
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, res *provider.ChatResult, pid, uid, model string, wantStream bool) {
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
		rc := &usageRecorder{
			src:   res.Body,
			pid:   pid,
			uid:   uid,
			model: model,
			g:     g,
		}
		defer rc.Close()
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
	}
	// 非流式 JSON
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	res.Body.Close()
	w.Write(raw)
	// 提取 usage 落账（openai 形态）
	var resp struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &resp) == nil && resp.Usage != nil {
		g.record(pid, uid, model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	}
}

func (g *Gateway) record(pid, uid, model string, prompt, completion, total int64) {
	g.usage.Record(usage.Record{
		Provider: pid, Account: uid, Model: model,
		PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total,
	})
}

// usageRecorder 包装响应流：按行扫 usage（OpenAI chunk.usage / 已转换流）。
type usageRecorder struct {
	src     io.ReadCloser
	pid     string
	uid     string
	model   string
	g       *Gateway
	pending []byte
	recorded atomic.Bool
}

func (u *usageRecorder) Read(p []byte) (int, error) {
	n, err := u.src.Read(p)
	if n > 0 {
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
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var chunk struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 || chunk.Usage.TotalTokens > 0 {
		u.g.record(u.pid, u.uid, u.model, chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens)
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
