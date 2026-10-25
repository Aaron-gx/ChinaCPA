// qrlogin.go 豆包扫码登录（字节 passport 协议，纯 HTTP，无需浏览器）：
//
//	GET /                                          收基础 Cookie（ttwid 等）
//	GET /passport/safe/csrf_token/?aid=497858      → passport_csrf_token
//	GET /passport/web/get_qrcode/?next=<enc>&aid=497858（x-tt-passport-csrf-token 头）
//	                                              → data.token + data.qrcode（base64 PNG）
//	GET /passport/web/check_qrconnect/?next&token  → data.status: new → scanned → confirmed
//	confirmed 后手动跟随 redirect_url 重定向链，逐跳收集 Set-Cookie → sessionid 全套
//
// s_v_web_id 是客户端生成值（verify_ 前缀），本地合成。流程口径来自社区 doubao2api
// （LiveSub QRLogin.cpp 移植版）实测。面板体验与 Trae 一致：发起 → 打开链接扫码
// → 手机确认 → 自动回调纳管。
package doubao

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
)

// 扫码会话状态机。
const (
	qrInit      = "waiting_scan" // 初始（含 new）
	qrScanned   = "scanned"      // 已扫码待确认
	qrConfirmed = "confirmed"    // 已确认（账号已就绪）
	qrExpired   = "expired"      // 二维码过期
	qrError     = "error"        // 流程错误
)

// qrSession 一次扫码登录会话（状态由 PollLogin 驱动，多路轮询共享节流）。
type qrSession struct {
	mu        sync.Mutex
	id        string
	token     string
	csrf      string
	cookies   map[string]string
	qrPNG     []byte
	qrRemote  string // 上游直接给二维码 URL 时的兜底
	state     string
	errMsg    string
	account   *model.Account
	createdAt time.Time
	lastCheck time.Time
}

// qrGet 带会话 Cookie 的 GET（不自动跟随重定向，返回状态码/响应体/Location）。
func (p *Provider) qrGet(ctx context.Context, u string, sess *qrSession, withCSRF bool) (int, []byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("User-Agent", dbUserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", dbOrigin+"/chat/login")
	req.Header.Set("Origin", dbOrigin)
	if ck := serializeCookies(sess.cookies); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	if withCSRF && sess.csrf != "" {
		req.Header.Set("x-tt-passport-csrf-token", sess.csrf)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	qrAbsorbCookies(resp, sess)
	return resp.StatusCode, body, resp.Header.Get("Location"), nil
}

// qrAbsorbCookies 合并 Set-Cookie 到会话（空值 = 删除，对齐 passport 语义）。
func qrAbsorbCookies(resp *http.Response, sess *qrSession) {
	for _, c := range resp.Header.Values("Set-Cookie") {
		kv := strings.SplitN(strings.SplitN(c, ";", 2)[0], "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		if v == "" {
			delete(sess.cookies, k)
			continue
		}
		sess.cookies[k] = v
	}
}

func serializeCookies(m map[string]string) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

// resolveLocation 相对 Location → 绝对 URL。
func resolveLocation(base, loc string) string {
	if loc == "" {
		return ""
	}
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return loc
	}
	b, err := url.Parse(base)
	if err != nil {
		return loc
	}
	r, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	return b.ResolveReference(r).String()
}

// ── 设备标识生成（对齐上游风控 SDK 形态）─────────────────

const fpCharset = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// genVerifyFP s_v_web_id：verify_ + 8-4-4-4-12 位随机字母数字。
func genVerifyFP() string {
	part := func(n int) string {
		out := make([]byte, n)
		for i := range out {
			v, _ := rand.Int(rand.Reader, big.NewInt(int64(len(fpCharset))))
			out[i] = fpCharset[v.Int64()]
		}
		return string(out)
	}
	return "verify_" + part(8) + "-" + part(4) + "-" + part(4) + "-" + part(4) + "-" + part(12)
}

// genDeviceID device_id：16 位数字、首位 7。
func genDeviceID() string { return "7" + randomDigits(15) }

// genWebID web_id / tea_uuid：19 位数字、前缀 76。
func genWebID() string { return "76" + randomDigits(17) }

// ── StartLogin / PollLogin ───────────────────────────────

// StartLogin 发起扫码登录：取 CSRF + 二维码，返回扫码页地址（面板展示给用户打开）。
func (p *Provider) StartLogin(ctx context.Context, callbackBase string) (*model.LoginSession, error) {
	sess := &qrSession{
		id: newUUID(), cookies: map[string]string{},
		state: qrInit, createdAt: time.Now(),
	}
	// ① 基础 Cookie
	if _, _, _, err := p.qrGet(ctx, dbOrigin+"/", sess, false); err != nil {
		return nil, fmt.Errorf("访问豆包首页失败: %w", err)
	}
	// ② passport_csrf_token（Cookie 优先，缺则请求签发接口）
	csrf := pick(sess.cookies["passport_csrf_token"], sess.cookies["passport_csrf_token_default"])
	if csrf == "" {
		status, body, _, err := p.qrGet(ctx, dbOrigin+"/passport/safe/csrf_token/?aid="+dbAid, sess, false)
		if err == nil && status == http.StatusOK {
			var data struct {
				Data struct {
					Token string `json:"passport_csrf_token"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &data) == nil {
				csrf = data.Data.Token
			}
		}
		csrf = pick(csrf, sess.cookies["passport_csrf_token"], sess.cookies["passport_csrf_token_default"])
	}
	if csrf == "" {
		return nil, fmt.Errorf("获取 passport_csrf_token 失败（上游风控？稍后重试）")
	}
	sess.csrf = csrf
	// ③ 二维码
	next := url.QueryEscape(dbOrigin)
	qrURL := dbOrigin + "/passport/web/get_qrcode/?next=" + next + "&aid=" + dbAid
	status, body, _, err := p.qrGet(ctx, qrURL, sess, true)
	if err != nil {
		return nil, fmt.Errorf("获取二维码失败: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("获取二维码失败 HTTP %d: %s", status, truncateRunes(string(body), 160))
	}
	var qrResp struct {
		Data struct {
			ErrorCode int    `json:"error_code"`
			Message   string `json:"message"`
			Token     string `json:"token"`
			QRCode    string `json:"qrcode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &qrResp); err != nil {
		return nil, fmt.Errorf("二维码响应解析失败: %v", err)
	}
	if qrResp.Data.Token == "" || qrResp.Data.ErrorCode != 0 {
		return nil, fmt.Errorf("获取二维码失败: %s", pick(qrResp.Data.Message, "上游未返回 token"))
	}
	sess.token = qrResp.Data.Token
	png, remote, derr := decodeQRField(qrResp.Data.QRCode)
	if derr != nil {
		return nil, derr
	}
	sess.qrPNG, sess.qrRemote = png, remote

	p.qrMu.Lock()
	// 顺带清理 30 分钟前的旧会话
	cutoff := time.Now().Add(-30 * time.Minute)
	for id, old := range p.qrSessions {
		if old.createdAt.Before(cutoff) {
			delete(p.qrSessions, id)
		}
	}
	p.qrSessions[sess.id] = sess
	p.qrMu.Unlock()

	page := strings.TrimSuffix(callbackBase, "") + "/doubao/qrlogin?sid=" + url.QueryEscape(sess.id)
	return &model.LoginSession{Provider: p.ID(), ID: sess.id, URL: page}, nil
}

// decodeQRField 二维码字段三形态：data: URI / http URL / 裸 base64 PNG。
func decodeQRField(field string) ([]byte, string, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return nil, "", fmt.Errorf("上游未返回二维码")
	}
	if strings.HasPrefix(field, "data:") {
		if i := strings.Index(field, ","); i > 0 {
			field = field[i+1:]
		}
	}
	if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
		return nil, field, nil
	}
	png, err := base64.StdEncoding.DecodeString(field)
	if err != nil {
		return nil, "", fmt.Errorf("二维码 base64 解码失败: %v", err)
	}
	return png, "", nil
}

// PollLogin 驱动扫码状态机（面板每 3 秒调用；内部节流 1.2s 防并发重复打上游）。
func (p *Provider) PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error) {
	p.qrMu.Lock()
	sess := p.qrSessions[s.ID]
	p.qrMu.Unlock()
	if sess == nil {
		return &model.PollResult{Status: model.LoginFailed, Message: "登录会话不存在或已过期，请重新发起"}, nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.account != nil {
		return &model.PollResult{Status: model.LoginDone, Account: sess.account}, nil
	}
	if sess.state == qrExpired {
		return &model.PollResult{Status: model.LoginFailed, Message: "二维码已过期，请重新发起登录"}, nil
	}
	if sess.state == qrError {
		return &model.PollResult{Status: model.LoginFailed, Message: sess.errMsg}, nil
	}
	if time.Since(sess.createdAt) > 3*time.Minute {
		sess.state = qrExpired
		return &model.PollResult{Status: model.LoginFailed, Message: "二维码已过期，请重新发起登录"}, nil
	}
	if time.Since(sess.lastCheck) < 1200*time.Millisecond {
		return qrPending(sess.state), nil
	}
	sess.lastCheck = time.Now()

	next := url.QueryEscape(dbOrigin)
	checkURL := dbOrigin + "/passport/web/check_qrconnect/?next=" + next + "&token=" + url.QueryEscape(sess.token) + "&aid=" + dbAid
	status, body, _, err := p.qrGet(ctx, checkURL, sess, true)
	if err != nil {
		return &model.PollResult{Status: model.LoginPending, Message: "检查登录状态失败，重试中"}, nil
	}
	if status != http.StatusOK {
		return &model.PollResult{Status: model.LoginPending,
			Message: fmt.Sprintf("检查登录状态 HTTP %d，重试中", status)}, nil
	}
	var check struct {
		Data struct {
			Status      string `json:"status"`
			RedirectURL string `json:"redirect_url"`
			Description string `json:"description"`
			ErrorCode   int    `json:"error_code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &check); err != nil {
		return &model.PollResult{Status: model.LoginPending, Message: "状态响应解析失败，重试中"}, nil
	}
	switch check.Data.Status {
	case "confirmed":
		a, aerr := p.qrFinish(ctx, sess, check.Data.RedirectURL)
		if aerr != nil {
			sess.state, sess.errMsg = qrError, aerr.Error()
			return &model.PollResult{Status: model.LoginFailed, Message: aerr.Error()}, nil
		}
		sess.state = qrConfirmed
		sess.account = a
		return &model.PollResult{Status: model.LoginDone, Account: a}, nil
	case "scanned":
		sess.state = qrScanned
		return &model.PollResult{Status: model.LoginPending, Message: "已扫码，请在手机上点击确认"}, nil
	case "expired":
		sess.state = qrExpired
		return &model.PollResult{Status: model.LoginFailed, Message: "二维码已过期，请重新发起登录"}, nil
	case "new", "":
		sess.state = qrInit
		return &model.PollResult{Status: model.LoginPending, Message: "请使用豆包 App 扫码（对话 → ＋ → 扫一扫）"}, nil
	default:
		return &model.PollResult{Status: model.LoginPending, Message: "状态: " + check.Data.Status}, nil
	}
}

// qrPending 按状态生成 pending 结果。
func qrPending(state string) *model.PollResult {
	if state == qrScanned {
		return &model.PollResult{Status: model.LoginPending, Message: "已扫码，请在手机上点击确认"}
	}
	return &model.PollResult{Status: model.LoginPending, Message: "请使用豆包 App 扫码（对话 → ＋ → 扫一扫）"}
}

// qrFinish 确认后收尾：跟随重定向链收全 Cookie → 组装账号并校验登录态。
func (p *Provider) qrFinish(ctx context.Context, sess *qrSession, redirectURL string) (*model.Account, error) {
	if redirectURL != "" {
		u := redirectURL
		for i := 0; i < 12; i++ {
			status, _, loc, err := p.qrGet(ctx, u, sess, false)
			if err != nil || status < 300 || status > 399 || loc == "" {
				break
			}
			u = resolveLocation(u, loc)
		}
	}
	if sess.cookies["sessionid"] == "" && sess.cookies["sid_guard"] == "" {
		var names []string
		for k := range sess.cookies {
			names = append(names, k)
		}
		return nil, fmt.Errorf("登录链路未返回 sessionid（获得 Cookie: %s）", truncateRunes(strings.Join(names, ","), 160))
	}
	// s_v_web_id 为客户端生成值：缺失则本地合成（风控指纹）
	if sess.cookies["s_v_web_id"] == "" {
		sess.cookies["s_v_web_id"] = genVerifyFP()
	}
	a := &model.Account{
		Provider:  p.ID(),
		Source:    "web",
		WebCookie: serializeCookies(sess.cookies),
	}
	ids := deviceIDs{DeviceID: genDeviceID(), WebID: genWebID()}
	if raw, err := json.Marshal(ids); err == nil {
		a.DoubaoSeed = string(raw)
	}
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := p.validate(vctx, a); err != nil {
		return nil, err
	}
	if a.UID == "" {
		a.UID = "doubao-" + hashCookie(a.WebCookie)
	}
	return a, nil
}

// hashCookie Cookie 串短哈希（UID 兜底）。
func hashCookie(cookie string) string {
	sum := sha256.Sum256([]byte("doubao:" + pick(cookieValue(cookie, "sessionid"), cookie)))
	return fmt.Sprintf("%x", sum[:4])
}

// ── 面板扫码页访问接口 ───────────────────────────────────

// QRLoginImage 会话二维码（PNG 字节或远程 URL），供面板扫码页/弹窗展示。
func (p *Provider) QRLoginImage(sid string) ([]byte, string, bool) {
	p.qrMu.Lock()
	sess := p.qrSessions[sid]
	p.qrMu.Unlock()
	if sess == nil || (len(sess.qrPNG) == 0 && sess.qrRemote == "") {
		return nil, "", false
	}
	return sess.qrPNG, sess.qrRemote, true
}

// QRLoginStatus 会话当前状态与错误信息（只读，不打上游）。
func (p *Provider) QRLoginStatus(sid string) (string, string) {
	p.qrMu.Lock()
	sess := p.qrSessions[sid]
	p.qrMu.Unlock()
	if sess == nil {
		return "unknown", "会话不存在或已过期"
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.account != nil {
		return qrConfirmed, ""
	}
	return sess.state, sess.errMsg
}
