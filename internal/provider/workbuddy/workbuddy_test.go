package workbuddy

import (
	"net/http"
	"strings"
	"testing"

	"chinacpa/internal/provider"
)

// riskGateBody 是智谱实际返回的报文（截断形式，字段顺序与线上一致）。
const riskGateBody = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","requestId":"702ee7cde0a8c23c85eec6f435d7aa47","displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"}}`

// TestClassifyHTTPRiskGate 线上 11128 走的是 HTTP 400 分支（错误体为原始 JSON），
// 必须被识别为可重试的风控闸门，而不是普通客户端错误。
func TestClassifyHTTPRiskGate(t *testing.T) {
	pe := classifyHTTP(http.StatusBadRequest, []byte(riskGateBody))
	if !pe.Retryable {
		t.Fatalf("11128 应标记 Retryable，实际 %+v", pe)
	}
	if pe.Kind == provider.ErrAuth {
		t.Fatal("11128 不是凭证问题，ErrAuth 会把账号标记需重新登录")
	}
	if pe.Code != "11128" {
		t.Fatalf("Code = %q, want 11128", pe.Code)
	}
}

// TestClassifyBizRiskGate 业务码分支同样命中，且优先于 11140/illegal 判定。
func TestClassifyBizRiskGate(t *testing.T) {
	pe := classifyBiz(11128, "Illegal API invocation from an unapproved channel")
	if !pe.Retryable || pe.Kind == provider.ErrAuth {
		t.Fatalf("业务码 11128 分类错误: %+v", pe)
	}
	// 报文含大写 Illegal，若先走 illegal 分支会被误判成 ErrAuth
	if strings.Contains(pe.Msg, "凭证") {
		t.Fatalf("不应给出凭证类提示: %q", pe.Msg)
	}
}

// TestClassifyBizIllegalCaseInsensitive 凭证类 illegal 报文大小写都要能识别。
func TestClassifyBizIllegalCaseInsensitive(t *testing.T) {
	for _, msg := range []string{"illegal request", "Illegal Request", "ILLEGAL"} {
		pe := classifyBiz(11140, msg)
		if pe.Kind != provider.ErrAuth {
			t.Errorf("classifyBiz(11140, %q).Kind = %v, want ErrAuth", msg, pe.Kind)
		}
	}
}

// TestClassifyUnaffected 其他错误分类不受影响（防回归）。
func TestClassifyUnaffected(t *testing.T) {
	if pe := classifyHTTP(http.StatusUnauthorized, []byte(`{"msg":"nope"}`)); pe.Kind != provider.ErrAuth {
		t.Errorf("401 应为 ErrAuth，实际 %v", pe.Kind)
	}
	if pe := classifyHTTP(http.StatusTooManyRequests, nil); pe.Kind != provider.ErrRate {
		t.Errorf("429 应为 ErrRate，实际 %v", pe.Kind)
	}
	if pe := classifyHTTP(http.StatusServiceUnavailable, nil); pe.Kind != provider.ErrUpstream {
		t.Errorf("503 应为 ErrUpstream，实际 %v", pe.Kind)
	}
	if pe := classifyHTTP(http.StatusBadRequest, []byte(`{"msg":"bad param"}`)); pe.Kind != provider.ErrClient {
		t.Errorf("普通 400 应为 ErrClient，实际 %v", pe.Kind)
	}
	if pe := classifyBiz(10001, "今天已签到"); pe.Kind != provider.ErrNone {
		t.Errorf("10001 应为 ErrNone，实际 %v", pe.Kind)
	}
	if pe := classifyBiz(12153, "token expired"); pe.Kind != provider.ErrAuth {
		t.Errorf("12153 应为 ErrAuth，实际 %v", pe.Kind)
	}
	// 普通客户端错误不应被误标为可重试
	if pe := classifyHTTP(http.StatusBadRequest, []byte(`{"msg":"bad param"}`)); pe.Retryable {
		t.Error("普通 400 不应标记 Retryable")
	}
}
