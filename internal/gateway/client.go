// client.go 请求来源客户端识别：从 User-Agent 归一化成可读的客户端类型，
// 记录到请求日志，面板据此展示「谁在调用」。
package gateway

import "strings"

// clientFromUA 从 User-Agent 识别客户端类型。识别不到的返回原始 UA 截断，
// 空 UA 返回 ""（面板显示「未知」）。
func clientFromUA(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}
	low := strings.ToLower(ua)
	// 优先级从具体到宽泛：先命中 zcode/claude-code/codex 等知名客户端，
	// 再按 UA 特征兜底分类。
	switch {
	case strings.Contains(low, "zcode"):
		return "zcode"
	case strings.Contains(low, "claude-code") || strings.Contains(low, "claude-cli"):
		return "claude-code"
	case strings.Contains(low, "claude"):
		return "claude"
	case strings.Contains(low, "codex"):
		return "codex"
	case strings.Contains(low, "cursor"):
		return "cursor"
	case strings.Contains(low, "windsurf"):
		return "windsurf"
	case strings.Contains(low, "openai"):
		return "openai"
	case strings.Contains(low, "go-http-client") || strings.Contains(low, "go/") && !strings.Contains(low, "browser"):
		return "go"
	case strings.Contains(low, "curl"):
		return "curl"
	case strings.Contains(low, "python-requests") || strings.Contains(low, "python"):
		return "python"
	case strings.Contains(low, "node"):
		return "node"
	case strings.Contains(low, "postman"):
		return "postman"
	case strings.Contains(low, "mozilla") || strings.Contains(low, "chrome") || strings.Contains(low, "safari"):
		return "browser"
	default:
		// 截断长 UA，只留前 40 字符
		if len(ua) > 40 {
			return ua[:40]
		}
		return ua
	}
}
