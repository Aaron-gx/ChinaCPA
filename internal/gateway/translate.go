// translate.go 网关多协议接入层：
//   - /v1/responses  OpenAI Responses API（Codex CLI 原生协议）
//   - /v1/messages   Anthropic Messages API（Claude Code 原生协议）
//
// 入站请求先翻译为 OpenAI Chat Completions 调 provider（恒流式出站），
// 响应流逐行翻译回原协议。翻译器完整移植自 CLIProxyAPI。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"chinacpa/internal/provider"
	claudetl "chinacpa/internal/translator/openai/claude"
	responsestl "chinacpa/internal/translator/openai/responses"
)

// forceStream 改写请求体为恒流式 + stream_options.include_usage。
func forceStream(body []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	obj["stream"] = true
	obj["stream_options"] = map[string]any{"include_usage": true}
	return json.Marshal(obj)
}

// ── /v1/responses（Codex CLI） ───────────────────────────

func (g *Gateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	inbound, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(inbound, &probe); err != nil || probe.Model == "" {
		writeErr(w, http.StatusBadRequest, "invalid_json", "请求体非法或缺少 model")
		return
	}
	openAIBody, err := responsestl.ConvertOpenAIResponsesRequestToOpenAIChatCompletions(probe.Model, inbound, true)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "translate_error", err.Error())
		return
	}
	route, rerr := g.pool.ResolveRoute(r.Context(), probe.Model)
	if rerr != nil {
		writeErr(w, http.StatusBadRequest, "model_not_found", rerr.Error())
		return
	}
	streamBody, ferr := forceStream(openAIBody)
	if ferr != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", ferr.Error())
		return
	}
	t0 := time.Now()
	client := clientFromUA(r.Header.Get("User-Agent"))
	st := g.callRoutedStream(r.Context(), w, route, streamBody, "responses", sessionKeyOf(inbound), client)
	if st == nil {
		return
	}
	defer st.release() // 请求结束释放账号负载计数
	pub := route.Public
	var param any
	if probe.Stream {
		writeSSEHeaders(w)
		rc := newProtocolStream(st, st.pid, pub, g, func(line []byte) [][]byte {
			return responsestl.ConvertOpenAIChatCompletionsResponseToOpenAIResponses(
				context.Background(), pub, inbound, openAIBody, line, &param)
		}, t0)
		defer rc.Close()
		pumpStream(w, rc)
		g.logReq("responses", pub, st.pid, st.acct.UID, client, true, rc.captured, true, "", st.attempts, rc.firstMS, time.Since(t0).Milliseconds(), rc.reported, rc.finish)
		return
	}
	full, reported, afinish, aerr := aggregateOpenAIJSON(st.body, pub)
	st.body.Close()
	if aerr != nil {
		writeErr(w, http.StatusBadGateway, "upstream_error", aerr.Error())
		return
	}
	u := g.recordFromJSON(st.pid, st.acct.UID, probe.Model, full)
	totalMS := time.Since(t0).Milliseconds()
	g.logReq("responses", pub, st.pid, st.acct.UID, client, false, u, true, "", st.attempts, totalMS, totalMS, reported, afinish)
	out := responsestl.ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(
		context.Background(), pub, inbound, openAIBody, full, &param)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// ── /v1/messages（Claude Code） ──────────────────────────

func (g *Gateway) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST only")
		return
	}
	inbound, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(inbound, &probe); err != nil || probe.Model == "" {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求体非法或缺少 model")
		return
	}
	openAIBody, err := claudetl.ConvertClaudeRequestToOpenAI(probe.Model, inbound, true)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	route, rerr := g.pool.ResolveRoute(r.Context(), probe.Model)
	if rerr != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", rerr.Error())
		return
	}
	streamBody, ferr := forceStream(openAIBody)
	if ferr != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_json", ferr.Error())
		return
	}
	t0 := time.Now()
	client := clientFromUA(r.Header.Get("User-Agent"))
	st := g.callRoutedStream(r.Context(), w, route, streamBody, "messages", sessionKeyOf(inbound), client)
	if st == nil {
		return
	}
	defer st.release() // 请求结束释放账号负载计数
	pub := route.Public
	var param any
	if probe.Stream {
		writeSSEHeaders(w)
		rc := newProtocolStream(st, st.pid, pub, g, func(line []byte) [][]byte {
			return claudetl.ConvertOpenAIResponseToClaude(
				context.Background(), pub, inbound, openAIBody, line, &param)
		}, t0)
		defer rc.Close()
		pumpStream(w, rc)
		g.logReq("messages", pub, st.pid, st.acct.UID, client, true, rc.captured, true, "", st.attempts, rc.firstMS, time.Since(t0).Milliseconds(), rc.reported, rc.finish)
		return
	}
	full, reported, afinish, aerr := aggregateOpenAIJSON(st.body, pub)
	st.body.Close()
	if aerr != nil {
		writeAnthropicErr(w, http.StatusBadGateway, "api_error", aerr.Error())
		return
	}
	u := g.recordFromJSON(st.pid, st.acct.UID, probe.Model, full)
	totalMS := time.Since(t0).Milliseconds()
	g.logReq("messages", pub, st.pid, st.acct.UID, client, false, u, true, "", st.attempts, totalMS, totalMS, reported, afinish)
	out := claudetl.ConvertOpenAIResponseToClaudeNonStream(
		context.Background(), pub, inbound, openAIBody, full, &param)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// ── 工具 ─────────────────────────────────────────────────

func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func writeAnthropicErr(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// pumpStream 把 reader 全部写出并逐块 flush。
func pumpStream(w http.ResponseWriter, rc io.ReadCloser) {
	flusher, _ := w.(http.Flusher)
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

// recordFromJSON 从聚合后的 OpenAI JSON 提取 usage 落账，返回 usage（请求日志用）。
func (g *Gateway) recordFromJSON(pid, uid, modelName string, full []byte) *usageSnapshot {
	var resp struct {
		Usage *usageSnapshot `json:"usage"`
	}
	if json.Unmarshal(full, &resp) == nil && resp.Usage != nil && (resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 || resp.Usage.TotalTokens > 0) {
		g.record(pid, uid, modelName, resp.Usage)
		return resp.Usage
	}
	g.record(pid, uid, modelName, nil)
	return nil
}

// ── 流转换器：OpenAI SSE → 目标协议 SSE ──────────────────

// protocolStream 逐行读取 provider 的 OpenAI SSE 流：
// 每条完整 data: 行先旁路提取 usage 落账，再交给协议转换器产出目标事件块。
type protocolStream struct {
	src      io.ReadCloser
	sc       *bufio.Scanner
	convert  func(line []byte) [][]byte
	out      []byte
	done     bool
	g        *Gateway
	pid      string
	uid      string
	model    string
	recorded atomic.Bool
	start    time.Time
	firstMS  int64
	captured *usageSnapshot
	reported string // 上游自报模型名
	finish   string // 结束原因
}

func newProtocolStream(st *openAIStream, pid, modelName string, g *Gateway, convert func([]byte) [][]byte, start time.Time) *protocolStream {
	sc := bufio.NewScanner(st.body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	return &protocolStream{src: st.body, sc: sc, convert: convert, g: g, pid: pid, uid: st.acct.UID, model: modelName, start: start}
}

func (s *protocolStream) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.done {
			return 0, io.EOF
		}
		if !s.sc.Scan() {
			s.done = true
			_ = s.src.Close()
			s.recordFallback()
			return 0, io.EOF
		}
		if s.firstMS == 0 && !s.start.IsZero() {
			s.firstMS = time.Since(s.start).Milliseconds()
		}
		line := s.sc.Bytes()
		s.captureModel(line)
		s.scanUsage(line)
		for _, ev := range s.convert(line) {
			s.out = append(s.out, ev...)
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// captureModel 捕获首个含 model 字段的原始 chunk（上游自报模型名）与结束原因。
func (s *protocolStream) captureModel(line []byte) {
	if !bytes.Contains(line, []byte(`"model"`)) && !bytes.Contains(line, []byte(`"finish_reason"`)) {
		return
	}
	t := bytes.TrimSpace(line)
	payload := []byte(t)
	if p, ok := bytes.CutPrefix(t, []byte("data:")); ok {
		payload = bytes.TrimSpace(p)
	}
	var pm struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &pm) != nil {
		return
	}
	if s.reported == "" && pm.Model != "" {
		s.reported = pm.Model
	}
	for _, ch := range pm.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finish = *ch.FinishReason
		}
	}
}

func (s *protocolStream) scanUsage(line []byte) {
	if s.recorded.Load() {
		return
	}
	t := strings.TrimSpace(string(line))
	if !strings.HasPrefix(t, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(t, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var chunk struct {
		Usage *usageSnapshot `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 || chunk.Usage.TotalTokens > 0 {
		s.g.record(s.pid, s.uid, s.model, chunk.Usage)
		s.captured = chunk.Usage
		s.recorded.Store(true)
	}
}

func (s *protocolStream) recordFallback() {
	if !s.recorded.Load() {
		s.g.record(s.pid, s.uid, s.model, nil)
		s.recorded.Store(true)
	}
}

func (s *protocolStream) Close() error {
	s.recordFallback()
	return s.src.Close()
}

// ── OpenAI SSE 聚合为完整 JSON ──────────────────────────

// aggregateOpenAIJSON 聚合 OpenAI SSE 为完整 JSON，返回聚合体与上游自报模型名。
// 工具调用分片按 index 合并（见 provider.AggregateOpenAISSE）。
func aggregateOpenAIJSON(body io.Reader, modelName string) ([]byte, string, string, error) {
	return provider.AggregateOpenAISSE(body, modelName)
}
