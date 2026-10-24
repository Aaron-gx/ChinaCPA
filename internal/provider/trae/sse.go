// sse.go SOLO SSE → OpenAI SSE 转换 + 非流式聚合（独立文件，便于维护）。
package trae

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	"chinacpa/internal/provider"
)

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



