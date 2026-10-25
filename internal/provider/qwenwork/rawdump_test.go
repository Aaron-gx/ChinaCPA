package qwenwork

// 临时调试工具：CHINACPA_QW_DUMP=1 go test ./internal/provider/qwenwork/ -run TestDumpRawStream -v
// 用真实账号发一条流式请求，打印上游原始 SSE 行与 unwrapLine 输出，定位内层帧形状。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chinacpa/internal/model"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }

func TestDumpRawStream(t *testing.T) {
	if os.Getenv("CHINACPA_QW_DUMP") != "1" {
		t.Skip("需要 CHINACPA_QW_DUMP=1")
	}
	home, _ := os.UserHomeDir()
	_ = home
	matches, _ := filepath.Glob(filepath.Join("..", "..", "..", "data", "accounts", "qwenwork-*.json"))
	if len(matches) == 0 {
		t.Fatal("找不到 qwenwork 账号文件")
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var a model.Account
	if err := jsonUnmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	p := New()
	if _, err := p.RefreshToken(context.Background(), &a); err != nil {
		t.Logf("刷新 token 失败（继续用现有 access token）: %v", err)
	}
	if q, err := p.Quota(context.Background(), &a); err != nil {
		t.Logf("余额查询失败: %v", err)
	} else {
		t.Logf("余额: remaining=%v unit=%s detail=%v", q.Remaining, q.Unit, q.Detail)
	}
	// 完整余额响应 + 云端模型目录（price_factor）+ 用户/套餐/积分记录
	auth := map[string]string{"Authorization": "Bearer " + a.AccessToken, "Accept": "application/json",
		"Content-Type": "application/json"}
	dumpGet(t, webBase+pathBalance, auth)
	dumpGet(t, webBase+"/user/wallets", auth)
	// 触发登录态会话（网页端建立 connect-session），观察每日积分是否发放
	dumpPost(t, webBase+"/user/connect-session", auth, "{}")
	dumpGet(t, webBase+"/user/v2/account-context", auth)
	dumpGet(t, webBase+"/user/wallets", auth)
	dumpGet(t, webBase+pathBalance, auth)
	// 每日积分领取（桌面端协议）：POST /api/v1/adapter/user/daily-credit-notice/claim
	claimHdrs := map[string]string{
		"Authorization":              "Bearer " + a.AccessToken,
		"Accept":                     "application/json",
		"Content-Type":               "application/json",
		"X-QwenWork-Version":         "1.1.59",
		"X-QwenWork-Release-Version": "1.1.59",
		"X-QwenWork-Build":           "1159",
		"X-QwenWork-Platform":        "win32",
		"X-QwenWork-Arch":            "x64",
		"X-QwenWork-Channel":         "stable",
		"X-QwenWork-App-Mode":        "default",
	}
	for _, base := range []string{gatewayBase, "https://openapi.qwenwork.cn"} {
		// 网关要求：claim 不带 query、不带 body
		req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/adapter/user/daily-credit-notice/claim", nil)
		for k, v := range claimHdrs {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("claim 失败: %v", err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		t.Logf("CLAIM %s → %d\n%s", base, resp.StatusCode, string(b))
	}
	dumpGet(t, webBase+"/user/wallets", auth)
	dumpModelList(t, &a)
	for _, tier := range []string{"flash", "pro", "qwen3.8-max-preview"} {
		tryTier(t, p, &a, tier)
	}
}

func dumpGet(t *testing.T, url string, headers map[string]string) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("GET %s 失败: %v", url, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	t.Logf("GET %s → %d\n%s", url, resp.StatusCode, truncateStr(string(b), 1200))
}

func dumpPost(t *testing.T, url string, headers map[string]string, body string) {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("POST %s 失败: %v", url, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	t.Logf("POST %s → %d\n%s", url, resp.StatusCode, truncateStr(string(b), 1200))
}

func dumpModelList(t *testing.T, a *model.Account) {
	m, err := buildSignMaterial(a.AccessToken, a.UID, a.Nickname, a.Email)
	if err != nil {
		t.Fatal(err)
	}
	full := gatewayBase + pathModelList
	req, _ := http.NewRequest(http.MethodGet, full, nil)
	for k, v := range cosyStaticHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range signHeaders(m, full, "") {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("model list 失败: %v", err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
	t.Logf("GET %s → %d\n%s", full, resp.StatusCode, truncateStr(string(b), 3000))
}

func tryTier(t *testing.T, p *Provider, a *model.Account, modelName string) {
	obj := map[string]any{
		"model":      modelName,
		"stream":     true,
		"request_id": "dump-req-1",
		"session_id": "dump-sess-1",
		"business":   map[string]any{"product": "qoder_work"},
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}
	out, _ := jsonMarshal(obj)
	bodyStr := string(out)
	m, err := buildSignMaterial(a.AccessToken, a.UID, a.Nickname, a.Email)
	if err != nil {
		t.Fatal(err)
	}
	full := gatewayBase + pathChat
	req, err := http.NewRequest(http.MethodPost, full, strings.NewReader(bodyStr))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range cosyStaticHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-model-key", modelName)
	req.Header.Set("User-Agent", "node")
	for k, v := range signHeaders(m, full, bodyStr) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	t.Logf("[%s] HTTP %d content-type=%s", modelName, resp.StatusCode, resp.Header.Get("Content-Type"))
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("上游错误: %s", string(b))
	}
	buf := make([]byte, 32*1024)
	var tail []byte
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			tail = append(tail, buf[:n]...)
			for {
				idx := strings.IndexByte(string(tail), '\n')
				if idx < 0 {
					break
				}
				line := string(tail[:idx])
				tail = tail[idx+1:]
				dumpLine(t, line)
			}
		}
		if rerr != nil {
			if len(tail) > 0 {
				dumpLine(t, string(tail))
			}
			break
		}
	}
}

func dumpLine(t *testing.T, line string) {
	s := strings.TrimSpace(line)
	if s == "" {
		return
	}
	t.Logf("RAW(%4d) %s", len(s), truncateStr(s, 600))
	if out := unwrapLine(line); len(out) > 0 {
		t.Logf("  UNWRAP→ %s", truncateStr(strings.TrimSpace(string(out)), 400))
	} else {
		t.Logf("  UNWRAP→ <dropped>")
	}
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("…(+%d)", len(r)-n)
}

// TestClaimProbe 对比 claim 的鉴权/路由错误形态。
func TestClaimProbe(t *testing.T) {
	if os.Getenv("CHINACPA_QW_DUMP") != "1" {
		t.Skip("需要 CHINACPA_QW_DUMP=1")
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "..", "data", "accounts", "qwenwork-*.json"))
	if len(matches) == 0 {
		t.Fatal("找不到账号文件")
	}
	raw, _ := os.ReadFile(matches[0])
	var a model.Account
	_ = jsonUnmarshal(raw, &a)
	p := New()
	_, _ = p.RefreshToken(context.Background(), &a)
	claimURL := gatewayBase + "/api/v1/adapter/user/daily-credit-notice/claim"
	do := func(name string, headers map[string]string) {
		req, _ := http.NewRequest(http.MethodPost, claimURL, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("%s → ERR %v", name, err)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		t.Logf("%s → %d %s", name, resp.StatusCode, truncateStr(string(b), 300))
	}
	do("无token", map[string]string{"Accept": "application/json", "Content-Type": "application/json"})
	do("Bearer", map[string]string{"Accept": "application/json", "Content-Type": "application/json", "Authorization": "Bearer " + a.AccessToken})
	// COSY 签名形态
	m, err := buildSignMaterial(a.AccessToken, a.UID, a.Nickname, a.Email)
	if err != nil {
		t.Fatal(err)
	}
	hdrs := map[string]string{"Accept": "application/json", "Content-Type": "application/json"}
	for k, v := range cosyStaticHeaders() {
		hdrs[k] = v
	}
	for k, v := range signHeaders(m, claimURL, "") {
		hdrs[k] = v
	}
	do("COSY", hdrs)
}

// TestCheckinReal 真实账号签到验证（CHINACPA_QW_DUMP=1）。
func TestCheckinReal(t *testing.T) {
	if os.Getenv("CHINACPA_QW_DUMP") != "1" {
		t.Skip("需要 CHINACPA_QW_DUMP=1")
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "..", "data", "accounts", "qwenwork-*.json"))
	if len(matches) == 0 {
		t.Fatal("找不到账号文件")
	}
	raw, _ := os.ReadFile(matches[0])
	var a model.Account
	_ = jsonUnmarshal(raw, &a)
	p := New()
	res, err := p.Checkin(context.Background(), &a)
	t.Logf("Checkin 结果: res=%+v err=%v", res, err)
	if bal, ok := p.dailyCreditBalance(context.Background(), &a); ok {
		t.Logf("签到后 daily_credits=%d", bal)
	}
}
