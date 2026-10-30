package trae

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestSoloToOpenAIStream 验证 SOLO SSE → OpenAI SSE 转换：
// output 增量 → chat.completion.chunk；token_usage → 带 usage 的块；done → finish + [DONE]。
func TestSoloToOpenAIStream(t *testing.T) {
	src := strings.Join([]string{
		"id:1",
		"event:metadata",
		`data:{"session_id":"s1"}`,
		"",
		"event:output",
		`data:{"response":"你好","reasoning_content":"思考"}`,
		"",
		"event:output",
		`data:{"response":"，世界"}`,
		"",
		"event:token_usage",
		`data:{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`,
		"",
		"event:done",
		`data:{"finish_reason":"stop"}`,
		"",
	}, "\n")

	p := New()
	s := newSoloToOpenAIStream(io.NopCloser(strings.NewReader(src)), "u1", p, "glm-5.2")
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(s)
	out := buf.String()

	if !strings.Contains(out, `"content":"你好"`) {
		t.Errorf("缺少内容增量: %s", out)
	}
	if !strings.Contains(out, `"reasoning_content":"思考"`) {
		t.Errorf("缺少思考链增量: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":10`) {
		t.Errorf("缺少 usage 块: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("缺少 finish_reason: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("缺少 [DONE]: %s", out)
	}
	// 每个事件块都必须是合法 OpenAI chunk
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &chunk); err != nil {
			t.Fatalf("非法 chunk JSON %q: %v", line, err)
		}
		if chunk["object"] != "chat.completion.chunk" {
			t.Fatalf("object 字段错误: %v", chunk["object"])
		}
	}
}

// TestAggregateSolo 非流式聚合：内容拼接 + usage。
func TestAggregateSolo(t *testing.T) {
	src := strings.Join([]string{
		"event:output",
		`data:{"response":"A"}`,
		"event:output",
		`data:{"response":"B"}`,
		"event:token_usage",
		`data:{"prompt_tokens":7,"completion_tokens":3}`,
		"event:done",
		`data:{"finish_reason":"stop"}`,
	}, "\n")
	p := New()
	raw, err := aggregateSolo(strings.NewReader(src), "u1", p, "glm-5.2")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "AB" {
		t.Fatalf("内容聚合错误: %s", raw)
	}
	if resp.Usage.PromptTokens != 7 || resp.Usage.TotalTokens != 10 {
		t.Fatalf("usage 聚合错误: %+v", resp.Usage)
	}
}

// TestPrepareBody OpenAI → SOLO 请求体转换。
func TestPrepareBody(t *testing.T) {
	in := `{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	out := prepareBody([]byte(in), "glm-5.2", fallbackScene)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if obj["function"] != fallbackScene {
		t.Errorf("function 字段错误: %v", obj["function"])
	}
	if obj["stream"] != true {
		t.Errorf("stream 必须强制 true: %v", obj["stream"])
	}
	if obj["config_name"] != "glm-5.2" {
		t.Errorf("config_name 错误: %v", obj["config_name"])
	}
	msgs := obj["messages"].([]any)
	m := msgs[0].(map[string]any)
	content, ok := m["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content 应转为数组: %v", m["content"])
	}
	if content[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("content 文本错误: %v", content)
	}
}

// TestBuildLoginURL 登录 URL 关键参数。
func TestBuildLoginURL(t *testing.T) {
	u := buildLoginURL("m123", "d456", "http://127.0.0.1:8313/trae/callback", "trace123", "verifier-abc")
	for _, want := range []string{
		"auth_callback_url=", "machine_id=m123", "device_id=d456", "client_id=" + clientID, "code_challenge=", "login_trace_id=trace123",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("URL 缺少 %s: %s", want, u)
		}
	}
}

// TestSumPacks 权益包汇总：credits_limit<=0 的包（仅功能开关）必须跳过，
// 否则会把无数值的包算进 limit 分母。
func TestSumPacks(t *testing.T) {
	mk := func(limit int64, used float64) entPack {
		var p entPack
		p.EntitlementBaseInfo.Quota.CreditsLimit = limit
		p.Usage.CreditsAmount = used
		return p
	}
	list := []entPack{
		mk(100, 0),    // 签到奖励 100/100
		mk(500, 500),  // 每月登录赠送 0/500（已用完）
		mk(2000, 2000),// 新用户福利 0/2000（已用完）
		mk(0, 0),      // 纯功能开关包：无数值，跳过
	}
	remain, limit, used, packs := sumPacks(list)
	if packs != 3 {
		t.Errorf("应只计 3 个数值包，得 %d", packs)
	}
	if limit != 2600 || used != 2500 || remain != 100 {
		t.Errorf("汇总错误: limit=%d used=%d remain=%d，期望 2600/2500/100", limit, used, remain)
	}
	// 全为开关包时 packs=0，调用方才会回落签到池
	_, _, _, p0 := sumPacks([]entPack{mk(0, 0)})
	if p0 != 0 {
		t.Errorf("无数值包时 packs 应为 0，得 %d", p0)
	}
}
