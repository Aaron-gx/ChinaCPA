// sse.go SOLO SSE → OpenAI SSE 转换 + 非流式聚合（独立文件，便于维护）。
package trae

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	hasTools bool   // 本次流出现过 tool_calls（done 时修正 finish_reason）
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
	// 上游 token_usage 帧的缓存字段（Anthropic 命名），转 OpenAI 形态输出
	CacheReadTokens  int64 `json:"-"`
	CacheWriteTokens int64 `json:"-"`
}

// toMap 输出 OpenAI 兼容 usage：缓存命中放 prompt_tokens_details.cached_tokens
// （网关 usageSnapshot 与标准客户端都认这个字段）。
func (u *openAIUsage) toMap() map[string]any {
	m := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
		m["prompt_tokens_details"] = map[string]any{
			"cached_tokens":     u.CacheReadTokens,
			"cache_write_tokens": u.CacheWriteTokens,
		}
	}
	return m
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
		obj["usage"] = usage.toMap()
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
		// SOLO 工具调用流式增量（function_call 老式形状）→ OpenAI tool_calls delta
		if tcs, ok := raw["tool_calls"].([]any); ok && len(tcs) > 0 {
			var ocs []any
			for _, tci := range tcs {
				tc, ok := tci.(map[string]any)
				if !ok {
					continue
				}
				oc := map[string]any{}
				if v, ok := tc["id"].(string); ok && v != "" {
					oc["id"] = v
				}
				if v, ok := tc["type"].(string); ok && v != "" {
					oc["type"] = v
				}
				if idx, ok := tc["index"].(float64); ok {
					oc["index"] = int(idx)
				}
				if fc, ok := tc["function_call"].(map[string]any); ok {
					fn := map[string]any{}
					if v, ok := fc["name"].(string); ok && v != "" {
						fn["name"] = v
					}
					if v, ok := fc["arguments"].(string); ok && v != "" {
						fn["arguments"] = v
					}
					if len(fn) > 0 {
						oc["function"] = fn
					}
				}
				if len(oc) > 0 {
					ocs = append(ocs, oc)
				}
			}
			if len(ocs) > 0 {
				delta["tool_calls"] = ocs
				s.hasTools = true
			}
		}
		if len(delta) > 0 {
			out = append(out, sseWrap(s.chunk(delta, nil, nil))...)
		}
	case "token_usage":
		u := &openAIUsage{}
		applyTraeUsage(u, raw)
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
		if s.hasTools && finish == "stop" {
			finish = "tool_calls" // OpenAI 语义：有工具调用时 finish 应为 tool_calls
		}
		out = append(out, sseWrap(s.chunk(map[string]any{}, finish, nil))...)
		out = append(out, []byte("data: [DONE]\n\n")...)
		s.done = true
		s.src.Close()
	case "error":
		msg := ""
		if v, ok := raw["message"].(string); ok {
			msg = v
		} else if v, ok := raw["error"].(string); ok {
			msg = v
		}
		if msg == "" {
			// 上游未给错误详情：附带原始报文便于排查
			rawBytes, _ := json.Marshal(raw)
			msg = "upstream error (raw: " + string(rawBytes) + ")"
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

// aggTool 非流式聚合中的工具调用累积。
type aggTool struct {
	id   string
	name string
	args string
}

// aggregateSolo 非流式：聚合 SOLO SSE 为完整 OpenAI 响应 JSON。
func aggregateSolo(body io.Reader, uid string, p *Provider, modelName string) ([]byte, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var content, reasoning strings.Builder
	var usage openAIUsage
	var finish = "stop"
	errMsg := ""
	evName := ""
	aggTools := map[string]*aggTool{}
	var toolOrder []string
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
			if tcs, ok := raw["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					fc, _ := tc["function_call"].(map[string]any)
					name, _ := fc["name"].(string)
					args, _ := fc["arguments"].(string)
					id, _ := tc["id"].(string)
					key := "0"
					if idx, ok := tc["index"].(float64); ok {
						key = fmt.Sprintf("%d", int(idx))
					}
					agg := aggTools[key]
					if agg == nil {
						agg = &aggTool{}
						aggTools[key] = agg
						toolOrder = append(toolOrder, key)
					}
					if id != "" {
						agg.id = id
					}
					agg.name += name
					agg.args += args
				}
			}
		case "token_usage":
			applyTraeUsage(&usage, raw)
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
	var toolCalls []any
	for _, k := range toolOrder {
		agg := aggTools[k]
		if agg == nil || agg.name == "" {
			continue
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   agg.id,
			"type": "function",
			"function": map[string]any{
				"name":      agg.name,
				"arguments": agg.args,
			},
		})
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if content.Len() == 0 {
			msg["content"] = nil
		}
		if finish == "stop" {
			finish = "tool_calls"
		}
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
		"usage": usage.toMap(),
	}
	return json.Marshal(resp)
}

// applyTraeUsage 解析上游 token_usage 帧：基础三件套 + 缓存字段（多命名兼容）。
// 实测帧形如 {"cache_creation_input_tokens":N,"cache_read_input_tokens":N,
// "prompt_tokens":N,"completion_tokens":N,"total_tokens":N,...}。
func applyTraeUsage(u *openAIUsage, raw map[string]any) {
	num := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := raw[k].(float64); ok {
				return int64(v)
			}
		}
		return 0
	}
	u.PromptTokens = num("prompt_tokens")
	u.CompletionTokens = num("completion_tokens")
	u.TotalTokens = num("total_tokens")
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	u.CacheReadTokens = num("cache_read_input_tokens", "cached_tokens", "input_cached_tokens", "prompt_cache_hit_tokens")
	u.CacheWriteTokens = num("cache_creation_input_tokens", "cache_write_tokens")
}

// ── 工具 ─────────────────────────────────────────────────
