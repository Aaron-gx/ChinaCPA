package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"chinacpa/internal/provider"
)

// TestAggregateToolCalls 工具调用分片按 index 合并。
func TestAggregateToolCalls(t *testing.T) {
	sse := "" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get\",\"arguments\":\"{\\\"a\\\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":1}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"set\",\"arguments\":\"{\\\"b\\\":2}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	out, _, _, err := provider.AggregateOpenAISSE(strings.NewReader(sse), "pub-m")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	tc := resp.Choices[0].Message.ToolCalls
	if len(tc) != 2 {
		t.Fatalf("期望 2 个合并后的工具调用，实际 %d: %+v", len(tc), tc)
	}
	if tc[0].ID != "call_1" || tc[0].Function.Name != "get" || tc[0].Function.Arguments != "{\"a\":1}" {
		t.Fatalf("工具调用 0 合并错误: %+v", tc[0])
	}
	if tc[1].ID != "call_2" || tc[1].Function.Arguments != "{\"b\":2}" {
		t.Fatalf("工具调用 1 合并错误: %+v", tc[1])
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("有工具调用且无 content 时应 finish=tool_calls，实际 %q", resp.Choices[0].FinishReason)
	}
}

// TestAggregateContent 普通文本聚合 + usage 保留。
func TestAggregateContent(t *testing.T) {
	sse := "" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	out, _, finish, err := provider.AggregateOpenAISSE(strings.NewReader(sse), "m")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Choices[0].Message.Content != "你好" {
		t.Fatalf("内容聚合错误: %q", resp.Choices[0].Message.Content)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 2 {
		t.Fatalf("usage 保留错误: %+v", resp.Usage)
	}
	if finish != "stop" {
		t.Fatalf("finish 错误: %q", finish)
	}
}
