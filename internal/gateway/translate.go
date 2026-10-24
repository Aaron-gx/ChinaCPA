// translate.go 网关多协议接入层：
//   - /v1/responses  OpenAI Responses API（Codex CLI 原生协议）
//   - /v1/messages   Anthropic Messages API（Claude Code 原生协议）
// 入站请求先翻译为 OpenAI Chat Completions 调 provider（恒流式出站），
// 响应流逐行翻译回原协议。翻译器完整移植自 CLIProxyAPI。
package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

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
	pid, mname, rerr := g.pool.ResolveModel(r.Context(), probe.Model)
	if rerr != nil {
		writeErr(w, http.StatusBadRequest, "model_not_found", rerr.Error())
		return
	}
	streamBody, ferr := forceStream(openAIBody)
	if ferr != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", ferr.Error())
		return
	}
	st := g.callOpenAIStream(r.Context(), w, pid, mname, streamBody)
	if st == nil {
		return
	}
	var param any
	if probe.Stream {
		writeSSEHeaders(w)
		rc := newProtocolStream(st, pid, probe.Model, g, func(line []byte) [][]byte {
			return responsestl.ConvertOpenAIChatCompletionsResponseToOpenAIResponses(
				context.Background(), mname, inbound, openAIBody, line, &param)
		})
		defer rc.Close()
		pumpStream(w, rc)
		return
	}
	full, aerr := aggregateOpenAIJSON(st.body, mname)
	st.body.Close()
	if aerr != nil {
		writeErr(w, http.StatusBadGateway, "upstream_error", aerr.Error())
		return
	}
	g.recordFromJSON(st.pid, st.acct.UID, probe.Model, full)
	out := responsestl.ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(
		context.Background(), mname, inbound, openAIBody, full, &param)
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
	pid, mname, rerr := g.pool.ResolveModel(r.Context(), probe.Model)
	if rerr != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", rerr.Error())
		return
	}
	streamBody, ferr := forceStream(openAIBody)
	if ferr != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", ferr.Error())
		return
	}
	st := g.callOpenAIStream(r.Context(), w, pid, mname, streamBody)
	if st == nil {
		return
	}
	var param any
	if probe.Stream {
		writeSSEHeaders(w)
		rc := newProtocolStream(st, pid, probe.Model, g, func(line []byte) [][]byte {
			return claudetl.ConvertOpenAIResponseToClaude(
				context.Background(), mname, inbound, openAIBody, line, &param)
		})
		defer rc.Close()
		pumpStream(w, rc)
		return
	}
	full, aerr := aggregateOpenAIJSON(st.body, mname)
	st.body.Close()
	if aerr != nil {
		writeAnthropicErr(w, http.StatusBadGateway, "api_error", aerr.Error())
		return
	}
	g.recordFromJSON(st.pid, st.acct.UID, probe.Model, full)
	out := claudetl.ConvertOpenAIResponseToClaudeNonStream(
		context.Background(), mname, inbound, openAIBody, full, &param)
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

// recordFromJSON 从聚合后的 OpenAI JSON 提取 usage 落账。
func (g *Gateway) recordFromJSON(pid, uid, modelName string, full []byte) {
	var resp struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(full, &resp) == nil && resp.Usage != nil {
		g.record(pid, uid, modelName, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
		return
	}
	g.record(pid, uid, modelName, 0, 0, 0)
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
}

func newProtocolStream(st *openAIStream, pid, modelName string, g *Gateway, convert func([]byte) [][]byte) *protocolStream {
	sc := bufio.NewScanner(st.body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	return &protocolStream{src: st.body, sc: sc, convert: convert, g: g, pid: pid, uid: st.acct.UID, model: modelName}
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
		line := s.sc.Bytes()
		s.scanUsage(line)
		for _, ev := range s.convert(line) {
			s.out = append(s.out, ev...)
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
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
		s.g.record(s.pid, s.uid, s.model, chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens)
		s.recorded.Store(true)
	}
}

func (s *protocolStream) recordFallback() {
	if !s.recorded.Load() {
		s.g.record(s.pid, s.uid, s.model, 0, 0, 0)
		s.recorded.Store(true)
	}
}

func (s *protocolStream) Close() error {
	s.recordFallback()
	return s.src.Close()
}

// ── OpenAI SSE 聚合为完整 JSON ──────────────────────────

func aggregateOpenAIJSON(body io.Reader, modelName string) ([]byte, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var content, reasoning strings.Builder
	var toolCalls []any
	var usage map[string]any
	finish := "stop"
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(t, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(t, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []any  `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
			reasoning.WriteString(c.Delta.ReasoningContent)
			toolCalls = append(toolCalls, c.Delta.ToolCalls...)
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	resp := map[string]any{
		"id":      "chatcmpl-chinacpa",
		"object":  "chat.completion",
		"created": 0,
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return json.Marshal(resp)
}
