package provider

import "testing"

// TestIsRiskGate 风控闸门识别：业务码、报文关键词（大小写不敏感）都能命中，
// 普通错误不应被误判。
func TestIsRiskGate(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want bool
	}{
		{"业务码 11128", 11128, "", true},
		{"智谱原文大写 Illegal", 0, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, true},
		{"报文里只出现 11128", 0, `{"code":11128}`, true},
		{"小写 illegal api invocation", 0, "illegal api invocation from an unapproved channel", true},
		{"11140 凭证类不应命中", 0, `{"code":11140,"msg":"token expired"}`, false},
		{"普通 401 报文", 0, `{"msg":"unauthorized"}`, false},
		{"空报文", 0, "", false},
		{"数字 11128 出现在无关上下文", 0, `{"msg":"balance 111281"}`, true}, // 宽容匹配，宁可多试一次
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRiskGate(c.code, c.body); got != c.want {
				t.Errorf("IsRiskGate(%d, %q) = %v, want %v", c.code, c.body, got, c.want)
			}
		})
	}
}

// TestRiskGateError 闸门错误必须带 Retryable 且不触发罚号（Kind != ErrAuth）。
func TestRiskGateError(t *testing.T) {
	e := RiskGateError(400)
	if !e.Retryable {
		t.Fatal("风控闸门错误必须标记 Retryable")
	}
	if e.Kind == ErrAuth {
		t.Fatal("风控闸门不是凭证问题，不能标 ErrAuth（否则账号会被标记需重新登录）")
	}
	if e.Code != "11128" {
		t.Fatalf("Code = %q, want 11128", e.Code)
	}
	if !IsRetryable(e) {
		t.Fatal("IsRetryable 应识别 RiskGateError")
	}
}

// TestIsRetryableNonGate 普通错误与 nil 不应被当成可重试。
func TestIsRetryableNonGate(t *testing.T) {
	if IsRetryable(nil) {
		t.Fatal("nil 不应可重试")
	}
	if IsRetryable(&Error{Kind: ErrAuth}) {
		t.Fatal("凭证错误不应可重试")
	}
	if IsRetryable(&Error{Kind: ErrRate}) {
		t.Fatal("限流错误走冷却策略，不在此重试路径")
	}
}
