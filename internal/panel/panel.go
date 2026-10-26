// Package panel 管理面板：账号/登录/签到/额度/用量 API + 内嵌单页仪表板。
package panel

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/config"
	"chinacpa/internal/gateway"
	"chinacpa/internal/model"
	"chinacpa/internal/pool"
	"chinacpa/internal/pubmodel"
	"chinacpa/internal/reqlog"
	"chinacpa/internal/scheduler"
	"chinacpa/internal/store"
	"chinacpa/internal/usage"
)

//go:embed static/index.html
var staticFS embed.FS

// Panel 管理面板。
type Panel struct {
	cfg   *config.Config
	pool  *pool.Pool
	st    *store.Store
	usage *usage.Tracker
	sched *scheduler.Scheduler
	pub   *pubmodel.Manager
	rlog  *reqlog.DB

	mu     sync.Mutex
	tokens map[string]*loginTicket // 登录票据（轮询用）
}

type loginTicket struct {
	provider  string
	session   *model.LoginSession
	result    *model.PollResult
	done      bool
	createdAt time.Time
}

// purgeTicketsLocked 清理过期登录票据（调用方持锁，30 分钟）。
func (p *Panel) purgeTicketsLocked() {
	for id, t := range p.tokens {
		if time.Since(t.createdAt) > 30*time.Minute {
			delete(p.tokens, id)
		}
	}
}

// New 构造。
func New(cfg *config.Config, p *pool.Pool, st *store.Store, u *usage.Tracker, sched *scheduler.Scheduler, pub *pubmodel.Manager, rl *reqlog.DB) *Panel {
	return &Panel{
		cfg:    cfg,
		pool:   p,
		st:     st,
		usage:  u,
		sched:  sched,
		pub:    pub,
		rlog:   rl,
		tokens: map[string]*loginTicket{},
	}
}

// Register 挂载全部路由（含网关）。
func (p *Panel) Register(mux *http.ServeMux, gw *gateway.Gateway) {
	gw.Register(mux, p.cfg.APIKey)

	mux.HandleFunc("/", p.handleIndex)
	mux.HandleFunc("/trae/callback", p.handleTraeCallback)
	mux.HandleFunc("/authorize", p.handleTraeCallback)               // Trae 官方回调路径
	mux.HandleFunc("/doubao/qrlogin", p.handleDoubaoQRPage)          // 豆包扫码页
	mux.HandleFunc("/doubao/qrlogin/img", p.handleDoubaoQRImg)       // 二维码图片
	mux.HandleFunc("/doubao/qrlogin/status", p.handleDoubaoQRStatus) // 扫码状态（驱动登录）
	mux.HandleFunc("/api/auth", p.handleAuth)
	mux.HandleFunc("/api/overview", p.authed(p.handleOverview))
	mux.HandleFunc("/api/accounts", p.authed(p.handleAccounts))
	mux.HandleFunc("/api/accounts/action", p.authed(p.handleAccountAction))
	mux.HandleFunc("/api/models", p.authed(p.handleModels))
	mux.HandleFunc("/api/login/start", p.authed(p.handleLoginStart))
	mux.HandleFunc("/api/login/poll", p.authed(p.handleLoginPoll))
	mux.HandleFunc("/api/import", p.authed(p.handleImport))
	mux.HandleFunc("/api/usage", p.authed(p.handleUsage))
	mux.HandleFunc("/api/checkin_all", p.authed(p.handleCheckinAll))
	mux.HandleFunc("/api/refresh_all_quotas", p.authed(p.handleRefreshAllQuotas))
	mux.HandleFunc("/api/publish", p.authed(p.handlePublish))
	mux.HandleFunc("/api/requests", p.authed(p.handleRequests))
}

// ── 面板鉴权 ─────────────────────────────────────────────

// sessionToken 无状态会话令牌：sha256(密码 + 固定盐)。
// 服务重启不掉线（不依赖内存会话表）；改密码即全部失效。
func (p *Panel) sessionToken() string {
	sum := sha256.Sum256([]byte("ccpa-panel-session:" + p.cfg.PanelPassword))
	return hex.EncodeToString(sum[:])
}

func (p *Panel) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Password != p.cfg.PanelPassword {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "密码错误"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "ccpa_session", Value: p.sessionToken(), Path: "/", HttpOnly: true, MaxAge: 7 * 24 * 3600})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) authed(next http.HandlerFunc) http.HandlerFunc {
	want := p.sessionToken()
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("ccpa_session"); err == nil && c.Value == want {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
	}
}

// ── 首页 ─────────────────────────────────────────────────

func (p *Panel) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// 面板单页应用：禁缓存，保证升级后浏览器总是拿到最新 HTML
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "panel asset missing", http.StatusInternalServerError)
		return
	}
	tpl, err := template.New("i").Parse(string(raw))
	if err != nil {
		http.Error(w, "panel template error", http.StatusInternalServerError)
		return
	}
	_ = tpl.Execute(w, map[string]any{})
}

// ── 总览（仪表板数据） ───────────────────────────────────

func (p *Panel) handleOverview(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(contextWithTimeout(r.Context(), 45*time.Second))
	type acctView struct {
		Provider       string               `json:"provider"`
		ProviderName   string               `json:"provider_name"`
		UID            string               `json:"uid"`
		Nickname       string               `json:"nickname"`
		Disabled       bool                 `json:"disabled"`
		NeedsRelogin   bool                 `json:"needs_relogin"`
		ExpiresAt      int64                `json:"expires_at"`
		TokenDaysLeft  float64              `json:"token_days_left"`
		CooldownUntil  int64                `json:"cooldown_until"`
		CooldownModels []string             `json:"cooldown_models,omitempty"` // 冷却中的模型（""=账号级）
		Quota          *model.QuotaInfo     `json:"quota,omitempty"`
		CheckinToday   *model.CheckinResult `json:"checkin_today,omitempty"`
		Status         string               `json:"status"` // ok / relogin / cooldown / disabled / expired
	}
	accounts := p.pool.Accounts()
	var views []acctView
	var totalCredits, totalIdeCredits, totalQoderCredits, totalQwenWorkCredits float64
	var qwenRemaining float64
	providerCount := map[string]int{}
	for _, a := range accounts {
		pr, _ := p.pool.Provider(a.Provider)
		name := a.Provider
		if pr != nil {
			name = pr.DisplayName()
		}
		providerCount[a.Provider]++
		v := acctView{
			Provider: a.Provider, ProviderName: name,
			UID: a.UID, Nickname: a.Nickname, Disabled: a.Disabled,
			NeedsRelogin: a.NeedsRelogin, ExpiresAt: a.ExpiresAt,
		}
		if a.NeedsRelogin {
			v.Status = "relogin"
		}
		if a.ExpiresAt > 0 {
			v.TokenDaysLeft = float64(a.ExpiresAt-time.Now().Unix()) / 86400
			if a.ExpiresAt < time.Now().Unix() {
				v.Status = "expired"
			}
		}
		if until := p.pool.CooldownUntil(a.Provider, a.UID); until.After(time.Now()) {
			v.CooldownUntil = until.Unix()
			if v.Status == "" {
				v.Status = "cooldown"
			}
			for m := range p.pool.CooldownModels(a.Provider, a.UID) {
				label := m
				if label == "" {
					label = "全部模型"
				}
				v.CooldownModels = append(v.CooldownModels, label)
			}
			sort.Strings(v.CooldownModels)
		}
		if a.Disabled {
			v.Status = "disabled"
		}
		if v.Status == "" {
			v.Status = "ok"
		}
		if q := p.st.GetQuota(a.Provider, a.UID); q != nil {
			v.Quota = q
			switch a.Provider {
			case "workbuddy", "workbuddy-intl":
				totalCredits += float64(q.Credits)
			case "trae":
				totalIdeCredits += q.Remaining
			case "qwen":
				qwenRemaining += q.Remaining
			case "qoder", "qoder-intl":
				totalQoderCredits += q.Remaining
			case "qwenwork":
				totalQwenWorkCredits += q.Remaining
			}
		}
		if ci, ok := p.st.TodayCheckin(a.Provider, a.UID); ok {
			c := ci
			v.CheckinToday = &c
		}
		views = append(views, v)
	}
	today := p.usage.TodayWithCost()
	total := usage.Sum(today)
	// 模型计数
	modelCount := map[string]int{}
	if all, err := p.pool.AllModels(r.Context()); err == nil {
		for pid, ms := range all {
			if ms != nil {
				modelCount[pid] = len(ms)
			}
		}
	}
	// 近 7/30 日 token 汇总
	days := p.usage.HistoryDays(30)
	sumDays := func(list []usage.DayTotal) map[string]int64 {
		var reqs, pr, cp, tt, ca int64
		for _, d := range list {
			reqs += d.Requests
			pr += d.Prompt
			cp += d.Complete
			tt += d.Total
			ca += d.Cache
		}
		return map[string]int64{"requests": reqs, "prompt_tokens": pr, "completion_tokens": cp, "total_tokens": tt, "cache_tokens": ca}
	}
	usageDays := map[string]any{"d7": sumDays(days[len(days)-7:]), "d30": sumDays(days)}
	// 签到调度信息
	schedule := map[string]any{
		"checkin_enabled":       p.cfg.Schedule.CheckinEnabled,
		"checkin_hours":         p.cfg.Schedule.CheckinHours,
		"tasks_enabled":         p.cfg.Schedule.TasksEnabled,
		"next_checkin":          p.nextCheckinUnix(),
		"quota_refresh_minutes": p.cfg.Schedule.QuotaRefreshMinutes,
		"token_refresh_hours":   p.cfg.Schedule.TokenRefreshHours,
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":           views,
		"account_total":      len(views),
		"provider_count":     providerCount,
		"model_count":        modelCount,
		"today":              total,
		"today_by_model":     today,
		"usage_days":         usageDays,
		"schedule":           schedule,
		"total_credits":      totalCredits,
		"total_ide_credits":  totalIdeCredits,
		"qwen_remaining":     qwenRemaining,
		"qoder_remaining":    totalQoderCredits,
		"qwenwork_remaining": totalQwenWorkCredits,
		"usd_cny":            p.usage.USDCNY(),
		"gateway_url":        "/v1",
		"listen":             p.cfg.Listen,
		"api_key":            p.cfg.APIKey,
		"data_dir":           p.cfg.DataDir,
		"now":                time.Now().Unix(),
	})
}

// nextCheckinUnix 下一次自动签到时刻（未启用返回 0）。
func (p *Panel) nextCheckinUnix() int64 {
	if !p.cfg.Schedule.CheckinEnabled || len(p.cfg.Schedule.CheckinHours) == 0 {
		return 0
	}
	hours := append([]int(nil), p.cfg.Schedule.CheckinHours...)
	sort.Ints(hours)
	now := time.Now()
	for _, h := range hours {
		if t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location()); t.After(now) {
			return t.Unix()
		}
	}
	return time.Date(now.Year(), now.Month(), now.Day(), hours[0], 0, 0, 0, now.Location()).AddDate(0, 0, 1).Unix()
}

// ── 账号操作 ─────────────────────────────────────────────

func (p *Panel) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		var body struct {
			Provider string `json:"provider"`
			UID      string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Provider == "" || body.UID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider/uid 必填"})
			return
		}
		if err := p.pool.RemoveAccount(body.Provider, body.UID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	// GET：单独账号列表（兼容简单客户端）
	writeJSON(w, http.StatusOK, map[string]any{"accounts": p.pool.Accounts()})
}

func (p *Panel) handleAccountAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
		UID      string `json:"uid"`
		Action   string `json:"action"` // checkin / quota / refresh_token / toggle
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Provider == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	var a *model.Account
	for _, x := range p.pool.Accounts() {
		if x.Provider == body.Provider && x.UID == body.UID {
			a = x
			break
		}
	}
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pr, _ := p.pool.Provider(a.Provider)
	switch body.Action {
	case "checkin":
		res := p.sched.CheckinAccount(a)
		writeJSON(w, http.StatusOK, res)
	case "quota":
		if _, err := p.pool.EnsureFreshToken(ctx, a); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "token 刷新失败: " + err.Error()})
			return
		}
		q, err := pr.Quota(ctx, a)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		p.st.SetQuota(q)
		writeJSON(w, http.StatusOK, q)
	case "refresh_token":
		changed, err := p.pool.EnsureFreshToken(ctx, a)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": changed, "expires_at": a.ExpiresAt})
	case "toggle":
		newDisabled := !a.Disabled
		if err := p.pool.SetDisabled(a.Provider, a.UID, newDisabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": newDisabled})
	case "resume":
		// 恢复需重新登录的账号（清除标记并重新纳入调度）
		if !p.pool.ClearNeedsRelogin(a.Provider, a.UID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "账号无需恢复"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "needs_relogin": false})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知 action"})
	}
}

func (p *Panel) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	go func() {
		for _, a := range p.pool.Accounts() {
			if !a.Disabled {
				p.sched.CheckinAccount(a)
			}
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已发起全账号签到"})
}

func (p *Panel) handleRefreshAllQuotas(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	go p.sched.RefreshAllQuotas()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已触发全账号额度刷新"})
}

// ── 模型列表 ─────────────────────────────────────────────

func (p *Panel) handleModels(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(contextWithTimeout(r.Context(), 60*time.Second))
	force := r.URL.Query().Get("refresh") == "1"
	type mv struct {
		ID          string `json:"id"`
		Name        string `json:"name,omitempty"`
		Description string `json:"description,omitempty"`
		Credits     string `json:"credits,omitempty"`
		Context     int64  `json:"context_window,omitempty"`
		MaxTokens   int64  `json:"max_tokens,omitempty"`
		Kind        string `json:"kind,omitempty"` // chat / image / video
	}
	out := map[string][]mv{}
	for _, pr := range p.pool.Providers() {
		ms, err := p.pool.RefreshModels(r.Context(), pr.ID(), force)
		if err != nil {
			out[pr.ID()] = []mv{}
			continue
		}
		list := make([]mv, 0, len(ms))
		for _, m := range ms {
			list = append(list, mv{ID: m.ID, Name: m.Name, Description: m.Description,
				Credits: m.Credits, Context: m.ContextWindow, MaxTokens: m.MaxTokens, Kind: m.Kind})
		}
		out[pr.ID()] = list
	}
	writeJSON(w, http.StatusOK, out)
}

// ── 登录（OAuth 发起 + 轮询） ───────────────────────────

// deriveCallbackBase 推导回调基地址：显式指定且非回环（远程部署）时尊重用户；
// 否则用「请求 Host 的主机名 + 服务端真实监听端口」——浏览器地址栏端口可能
// 是代理或手误，回调必须指回真实服务端口（Trae 授权页会校验回调可达性）。
func (p *Panel) deriveCallbackBase(r *http.Request, explicit string) string {
	if explicit != "" && p.looksExternal(explicit) {
		return explicit
	}
	host := "127.0.0.1"
	if h := r.Host; h != "" {
		host = hostOnly(h)
	}
	// Listen 可能是 ":8318" 或 "127.0.0.1:8318"，都取端口部分
	port := strings.TrimPrefix(p.cfg.Listen, ":")
	if _, p2, err := net.SplitHostPort(p.cfg.Listen); err == nil && p2 != "" {
		port = p2
	}
	return "http://" + host + ":" + port
}

// hostOnly 去掉 Host 头的端口部分（兼容 IPv6 字面量）。
func hostOnly(hostPort string) string {
	if h, _, err := net.SplitHostPort(hostPort); err == nil && h != "" {
		return h
	}
	return hostPort
}

// looksExternal 显式指定了非本机回环地址（远程部署场景）。
func (p *Panel) looksExternal(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h != "127.0.0.1" && h != "localhost" && h != "::1"
}

func (p *Panel) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Provider     string `json:"provider"`
		CallbackBase string `json:"callback_base"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	pr, ok := p.pool.Provider(body.Provider)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知平台: " + body.Provider})
		return
	}
	s, err := pr.StartLogin(r.Context(), p.deriveCallbackBase(r, body.CallbackBase))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	t := &loginTicket{provider: body.Provider, session: s, createdAt: time.Now()}
	p.mu.Lock()
	p.purgeTicketsLocked()
	p.tokens[s.ID] = t
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": s.ID, "url": s.URL, "user_code": s.UserCode, "provider": s.Provider,
	})
}

func (p *Panel) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	provID := r.URL.Query().Get("provider")
	id := r.URL.Query().Get("id")
	p.mu.Lock()
	p.purgeTicketsLocked()
	t, ok := p.tokens[id]
	p.mu.Unlock()
	if !ok || t.provider != provID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "登录会话不存在或已过期"})
		return
	}
	if t.done {
		writeJSON(w, http.StatusOK, t.result)
		return
	}
	pr, _ := p.pool.Provider(provID)
	res, err := pr.PollLogin(r.Context(), t.session)
	if err != nil {
		writeJSON(w, http.StatusOK, &model.PollResult{Status: model.LoginFailed, Message: err.Error()})
		return
	}
	if res.Status == model.LoginDone && res.Account != nil {
		if err := p.pool.AddAccount(res.Account); err != nil {
			writeJSON(w, http.StatusOK, &model.PollResult{Status: model.LoginFailed, Message: "账号保存失败: " + err.Error()})
			return
		}
		// 登录即签到 + 刷额度
		go func() {
			p.sched.CheckinAccount(res.Account)
		}()
		log.Printf("PANEL: 新账号纳管 %s %s", res.Account.Provider, res.Account.Nickname)
	}
	t.done = res.Status != model.LoginPending
	t.result = res
	writeJSON(w, http.StatusOK, res)
}

// handleTraeCallback Trae 登录回调（浏览器重定向，无需面板会话）。
func (p *Panel) handleTraeCallback(w http.ResponseWriter, r *http.Request) {
	tp := p.traeProvider()
	if tp == nil {
		http.Error(w, "trae provider unavailable", http.StatusInternalServerError)
		return
	}
	res, err := tp.HandleCallback(r.URL.Query())
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "<h3>❌ Trae 登录失败</h3><p>%s</p><p><a href=\"/\">返回面板</a></p>", err.Error())
		return
	}
	doneNow := res.Account != nil
	if doneNow {
		if err := p.pool.AddAccount(res.Account); err == nil {
			go func() {
				p.sched.CheckinAccount(res.Account)
			}()
			log.Printf("PANEL: Trae 回调纳管 %s", res.Account.UID)
		}
	}
	// 同步完成（票据丢失的兜底路径）：票据直接标记完成，轮询立刻成功，不再二次 AddAccount。
	// 异步路径（res 仍是 pending）：票据保持轮询，后台令牌交换完成后轮询自然拿到结果。
	if doneNow {
		p.mu.Lock()
		for _, tk := range p.tokens {
			if tk.provider == "trae" && !tk.done {
				tk.done, tk.result = true, res
			}
		}
		p.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	title, sub := "✅ Trae 登录成功，账号已纳管", "正在返回面板…（本页 4 秒后自动跳转）"
	if !doneNow {
		title, sub = "✅ 已收到 Trae 授权", "正在后台交换令牌并纳管，本页可直接关闭或稍候自动返回面板"
	}
	fmt.Fprint(w, `<!DOCTYPE html><html><head><meta charset="utf-8">`+
		`<meta http-equiv="refresh" content="4;url=/">`+
		`<style>body{background:#faf9f5;color:#1f1e1d;font-family:"Microsoft YaHei",sans-serif;padding:60px;text-align:center}a{color:#c96442}</style></head><body>`+
		`<h3>`+title+`</h3><p>`+sub+`</p><p><a href="/">立即返回面板</a></p>`+
		`<script>try{if(window.opener&&window.opener!==window&&`+fmt.Sprintf("%t", doneNow)+`){window.opener.postMessage({type:'ccpa:login-done'},location.origin)}}catch(e){}</script>`+
		`</body></html>`)
}

func (p *Panel) traeProvider() traeProviderIface {
	pr, ok := p.pool.Provider("trae")
	if !ok {
		return nil
	}
	tp, ok := pr.(traeProviderIface)
	if !ok {
		return nil
	}
	return tp
}

// traeProviderIface Trae 回调能力接口。
type traeProviderIface interface {
	HandleCallback(q url.Values) (*model.PollResult, error)
}

// ── 豆包扫码登录页（sid 即登录票据，无需面板会话）────────

func (p *Panel) handleDoubaoQRPage(w http.ResponseWriter, r *http.Request) {
	sid := sanitizeSID(r.URL.Query().Get("sid"))
	if sid == "" {
		http.Error(w, "missing sid", http.StatusBadRequest)
		return
	}
	// 不用 Fprintf：页面 CSS 含 % 字符（如 calc(100% - 40px)），会被当格式动词
	page := strings.ReplaceAll(doubaoQRPageHTML, "__SID__", sid)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

func (p *Panel) handleDoubaoQRImg(w http.ResponseWriter, r *http.Request) {
	qi := p.doubaoQRIface()
	if qi == nil {
		http.Error(w, "doubao provider unavailable", http.StatusServiceUnavailable)
		return
	}
	png, remote, ok := qi.QRLoginImage(sanitizeSID(r.URL.Query().Get("sid")))
	if !ok {
		http.Error(w, "二维码不存在或已过期", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(png) > 0 {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
		return
	}
	http.Redirect(w, r, remote, http.StatusFound)
}

// handleDoubaoQRStatus 扫码页状态轮询：顺带驱动登录状态机（与面板弹窗轮询
// 共用 provider 内的会话锁与节流），确认后直接纳管 —— 扫码页独立即可完成登录。
func (p *Panel) handleDoubaoQRStatus(w http.ResponseWriter, r *http.Request) {
	qi := p.doubaoQRIface()
	if qi == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error", "message": "doubao provider unavailable"})
		return
	}
	sid := sanitizeSID(r.URL.Query().Get("sid"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	res, _ := qi.PollLogin(ctx, &model.LoginSession{Provider: "doubao", ID: sid})
	cancel()
	state, msg := qi.QRLoginStatus(sid)
	if state == "confirmed" && res != nil && res.Status == model.LoginDone && res.Account != nil {
		p.mu.Lock()
		tk, ok := p.tokens[sid]
		if ok && !tk.done && tk.provider == "doubao" {
			if err := p.pool.AddAccount(res.Account); err == nil {
				go p.sched.CheckinAccount(res.Account)
				tk.done, tk.result = true, res
				log.Printf("PANEL: 豆包扫码纳管 %s", res.Account.UID)
			}
		}
		p.mu.Unlock()
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"status": state, "message": msg})
}

func (p *Panel) doubaoQRIface() doubaoQRIface {
	pr, ok := p.pool.Provider("doubao")
	if !ok {
		return nil
	}
	qi, ok := pr.(doubaoQRIface)
	if !ok {
		return nil
	}
	return qi
}

// doubaoQRIface 豆包扫码登录展示/驱动能力接口。
type doubaoQRIface interface {
	PollLogin(ctx context.Context, s *model.LoginSession) (*model.PollResult, error)
	QRLoginImage(sid string) ([]byte, string, bool)
	QRLoginStatus(sid string) (string, string)
}

// sanitizeSID 会话 ID 白名单（防注入：仅字母数字与连字符）。
func sanitizeSID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// doubaoQRPageHTML 扫码页（__SID__ 占位，写入前 ReplaceAll；sid 已白名单化）。
const doubaoQRPageHTML = `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>豆包扫码登录 · ChinaCPA</title>
<style>
body{background:#faf9f5;color:#1f1e1d;font-family:"Segoe UI","Microsoft YaHei",sans-serif;min-height:100vh;display:flex;align-items:center;justify-content:center;margin:0}
.card{background:#fff;border:1px solid #e8e4d9;border-radius:16px;box-shadow:0 2px 8px rgba(31,30,29,.06);padding:32px 36px;text-align:center;max-width:380px;width:calc(100% - 40px)}
h3{margin:0 0 6px;font-size:17px}
.sub{color:#8a867b;font-size:13px;margin:0 0 18px;line-height:1.8}
.qr{border:1px solid #e8e4d9;border-radius:12px;padding:10px;margin-bottom:14px}
.qr img{width:220px;height:220px;display:block;margin:0 auto}
.st{font-size:14px;margin:0 0 6px}
.st.ok{color:#5c8a5e}.st.err{color:#b3564e}
.tip{color:#aca899;font-size:12px;margin-top:12px;line-height:1.8}
</style></head><body><div class="card">
<h3>豆包扫码登录</h3>
<p class="sub">打开豆包 App → 对话 → 右上角 ＋ → 扫一扫</p>
<div class="qr"><img id="qr" src="/doubao/qrlogin/img?sid=__SID__" alt="二维码加载中…"></div>
<p class="st" id="st">请使用豆包 App 扫描二维码</p>
<p class="tip">手机确认后自动纳管账号，本页可关闭</p>
</div>
<script>
var sid="__SID__";
function setSt(t,c){var e=document.getElementById('st');e.textContent=t;if(c)e.className='st '+c;}
function tick(){
  fetch('/doubao/qrlogin/status?sid='+encodeURIComponent(sid),{cache:'no-store'})
   .then(function(r){return r.json()}).then(function(j){
     if(j.status==='confirmed'){
       setSt('✅ 登录成功，账号已自动纳管，本页可以关闭','ok');
       try{if(window.opener&&window.opener!==window)window.opener.postMessage({type:'ccpa:login-done'},location.origin)}catch(e){}
       return;
     }
     if(j.status==='scanned'){setSt('已扫码，请在手机上点击确认');}
     else if(j.status==='expired'){setSt('二维码已过期，请回面板重新发起登录','err');return;}
     else if(j.status==='error'){setSt('登录失败：'+(j.message||''),'err');return;}
     else if(j.status==='unknown'){setSt('会话不存在，请回面板重新发起登录','err');return;}
     else{setSt('请使用豆包 App 扫描二维码');}
     setTimeout(tick,2000);
   }).catch(function(){setTimeout(tick,3000);});
}
tick();
</script></body></html>`

// ── 凭证导入 ─────────────────────────────────────────────

func (p *Panel) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Provider   string `json:"provider"`
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Credential == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	pr, ok := p.pool.Provider(body.Provider)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知平台"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	a, err := pr.ImportCredential(ctx, body.Credential)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := p.pool.AddAccount(a); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	go func() {
		p.sched.CheckinAccount(a)
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": a.UID, "nickname": a.Nickname})
}

// ── 用量 ─────────────────────────────────────────────────

func (p *Panel) handleUsage(w http.ResponseWriter, r *http.Request) {
	days := 7
	if d := r.URL.Query().Get("days"); d != "" {
		if _, err := fmt.Sscanf(d, "%d", &days); err != nil || days < 1 || days > 90 {
			days = 7
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"history": p.usage.HistoryDays(days),
		"today":   p.usage.TodayWithCost(),
		"totals":  usage.Sum(p.usage.TodayWithCost()),
	})
}

// contextWithTimeout 上游拉取兜底超时。
func contextWithTimeout(parent context.Context, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(parent, d)
	_ = cancel // 请求结束即释放
	return ctx
}

// ── 发布模型（对外模型 → 多渠道） ────────────────────────

// handlePublish 发布模型配置读写：GET 返回配置+渠道计数，POST 保存并热生效。
func (p *Panel) handlePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg := p.pub.Get()
		writeJSON(w, http.StatusOK, map[string]any{
			"models":        cfg.Models,
			"usd_cny":       cfg.USDCNY,
			"channel_stats": p.pub.ChannelStats(),
		})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET/POST only"})
		return
	}
	var cfg pubmodel.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	if err := p.pub.Save(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 计价热生效（发布模型单价优先于 pricing.json）
	pub := map[string]usage.PubPrice{}
	for k, v := range p.pub.PricingMap() {
		pub[strings.ToLower(k)] = usage.PubPrice{Input: v.Input, Output: v.Output, Cache: v.Cache, PerCall: v.PerCall}
	}
	p.usage.SetPublishedPricing(pub, p.pub.USDCNY())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(p.pub.Get().Models)})
}

// handleRequests 请求日志：时间范围 + 模型/状态筛选 + 分页。
// GET ?range=today|yesterday|7d|30d|all&model=&status=ok|fail&limit=100&offset=0
func (p *Panel) handleRequests(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	from, to := int64(0), now.Unix()
	switch r.URL.Query().Get("range") {
	case "today":
		from = todayStart.Unix()
	case "yesterday":
		from = todayStart.AddDate(0, 0, -1).Unix()
		to = todayStart.Unix() - 1
	case "7d":
		from = todayStart.AddDate(0, 0, -6).Unix()
	case "30d":
		from = todayStart.AddDate(0, 0, -29).Unix()
	default: // all
	}
	limit := 100
	if n, err := fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit); err != nil || n < 1 || limit < 1 || limit > 1000 {
		limit = 100
	}
	offset := 0
	if n, err := fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset); err != nil || n < 1 || offset < 0 {
		offset = 0
	}
	q := reqlog.QueryOpts{
		From:   from,
		To:     to,
		Model:  r.URL.Query().Get("model"),
		Status: r.URL.Query().Get("status"),
		Limit:  limit,
		Offset: offset,
	}
	entries, stats, err := p.rlog.Query(q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"stats":   stats,
		"query":   map[string]any{"range": r.URL.Query().Get("range"), "limit": limit, "offset": offset, "total": stats.Total},
	})
}

// ── 工具 ─────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj)
}
