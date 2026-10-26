package gateway

import "testing"

// TestClientFromUA User-Agent 归一化识别。
func TestClientFromUA(t *testing.T) {
	cases := []struct {
		ua   string
		want string
	}{
		{"zcode/1.2.3 (Windows)", "zcode"},
		{"claude-cli/1.0.94 (claude-code)", "claude-code"},
		{"claude-code/2.1.0", "claude-code"},
		{"codex_cli/0.5.0 (OpenAI)", "codex"},
		{"curl/8.5.0", "curl"},
		{"python-requests/2.31.0", "python"},
		{"go-http-client/1.1", "go"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36", "browser"},
		{"OpenAI/Python 1.2.3", "openai"},
		{"", ""},
		{"CustomClient/9.9", "CustomClient/9.9"},
	}
	for _, c := range cases {
		if got := clientFromUA(c.ua); got != c.want {
			t.Errorf("clientFromUA(%q) = %q, want %q", c.ua, got, c.want)
		}
	}
}
