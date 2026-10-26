// Package provider 定义 ChinaCPA 的平台 Provider 抽象：
// 登录纳管、凭证导入、token 刷新、额度查询、签到、模型列表、对话转发。
package provider

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// ── OpenAI SSE 聚合 ──────────────────────────────────────

// aggToolCall 按 index 聚合的单个工具调用。
type aggToolCall struct {
	id   string
	name string
	args string
}

// AggregateOpenAISSE 聚合 OpenAI 格式 SSE 流为完整 chat.completion JSON。
// 返回聚合体、上游自报模型名、结束原因。工具调用按 delta.tool_calls[].index
// 合并（分片 name/arguments 累加拼接），content 为空且存在工具调用时
// finish_reason 归一为 tool_calls。
// 供网关非流式路径与各 provider 的本地聚合复用。
func AggregateOpenAISSE(body io.Reader, modelName string) ([]byte, string, string, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var content, reasoning strings.Builder
	tools := map[int]*aggToolCall{}
	var toolOrder []int
	var usage map[string]any
	reported := ""
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
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string            `json:"content"`
					ReasoningContent string            `json:"reasoning_content"`
					ToolCalls        []json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Model != "" && reported == "" {
			reported = chunk.Model
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
			reasoning.WriteString(c.Delta.ReasoningContent)
			mergeToolCalls(tools, &toolOrder, c.Delta.ToolCalls)
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if tcs := buildToolCalls(tools, toolOrder); len(tcs) > 0 {
		msg["tool_calls"] = tcs
		if content.Len() == 0 && finish == "stop" {
			finish = "tool_calls"
		}
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
	out, err := json.Marshal(resp)
	return out, reported, finish, err
}

// mergeToolCalls 把一帧里的工具调用增量合并进按 index 的累加器。
// 无 index 时按出现顺序分配 0..n-1（与大多数上游行为一致）。
func mergeToolCalls(acc map[int]*aggToolCall, order *[]int, raw []json.RawMessage) {
	for i, tc := range raw {
		var t struct {
			Index    *int `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal(tc, &t) != nil {
			continue
		}
		idx := i
		if t.Index != nil {
			idx = *t.Index
		}
		agg := acc[idx]
		if agg == nil {
			agg = &aggToolCall{}
			acc[idx] = agg
			*order = append(*order, idx)
		}
		if t.ID != "" {
			agg.id = t.ID
		}
		if t.Function.Name != "" {
			agg.name += t.Function.Name
		}
		agg.args += t.Function.Arguments
	}
}

// buildToolCalls 按出现顺序输出标准 OpenAI tool_calls 数组。
func buildToolCalls(acc map[int]*aggToolCall, order []int) []map[string]any {
	if len(order) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(order))
	for _, idx := range order {
		agg := acc[idx]
		if agg == nil {
			continue
		}
		out = append(out, map[string]any{
			"id":   agg.id,
			"type": "function",
			"function": map[string]any{
				"name":      agg.name,
				"arguments": agg.args,
			},
		})
	}
	return out
}
