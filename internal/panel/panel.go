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
	"net/http"
	"net/url"
	"sync"
	"time"

	"chinacpa/internal/config"
	"chinacpa/internal/gateway"
	"chinacpa/internal/model"
	"chinacpa/internal/pool"
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

	mu       sync.Mutex
	sessions map[string]*model.LoginSession // login session id -> session
	tokens   map[string]*loginTicket        // 登录票据（轮询用）
}

type loginTicket struct {
	provider string
	session  *model.LoginSession
	result   *model.PollResult
	done     bool
}

// New 构造。
func New(cfg *config.Config, p *pool.Pool, st *store.Store, u *usage.Tracker, sched *scheduler.Scheduler) *Panel {
	return &Panel{
		cfg:     cfg,
		pool:    p,
		st:      st,
		usage:   u,
		sched:   sched,
		sessions: map[string]*model.LoginSession{},
		tokens:   map[string]*loginTicket{},
	}
}

// Register 挂载全部路由（含网关）。
func (p *Panel) Register(mux *http.ServeMux, gw *gateway.Gateway) {
	gw.Register(mux, p.cfg.APIKey)

	mux.HandleFunc("/", p.handleIndex)
	mux.HandleFunc("/trae/callback", p.handleTraeCallback)
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
	type acctView struct {
		Provider      string             `json:"provider"`
		ProviderName  string             `json:"provider_name"`
		UID           string             `json:"uid"`
		Nickname      string             `json:"nickname"`
		Disabled      bool               `json:"disabled"`
		ExpiresAt     int64              `json:"expires_at"`
		TokenDaysLeft float64            `json:"token_days_left"`
		CooldownUntil int64              `json:"cooldown_until"`
		Quota         *model.QuotaInfo   `json:"quota,omitempty"`
		CheckinToday  *model.CheckinResult `json:"checkin_today,omitempty"`
		Status        string             `json:"status"` // ok / cooldown / disabled / expired
	}
	accounts := p.pool.Accounts()
	var views []acctView
	var totalCredits, totalIdeCredits, totalQoderCredits float64
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
			ExpiresAt: a.ExpiresAt,
		}
		if a.ExpiresAt > 0 {
			v.TokenDaysLeft = float64(a.ExpiresAt-time.Now().Unix()) / 86400
			if a.ExpiresAt < time.Now().Unix() {
				v.Status = "expired"
			}
		}
		if until := p.pool.CooldownUntil(a.Provider, a.UID); until.After(time.Now()) {
			v.CooldownUntil = until.Unix()
			v.Status = "cooldown"
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
			case "workbuddy":
				totalCredits += float64(q.Credits)
			case "trae":
				totalIdeCredits += q.Remaining
			case "qwen":
				qwenRemaining += q.Remaining
			case "qoder":
				totalQoderCredits += q.Remaining
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
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":          views,
		"account_total":     len(views),
		"provider_count":    providerCount,
		"model_count":       modelCount,
		"today":             total,
		"today_by_model":    today,
		"total_credits":     totalCredits,
		"total_ide_credits": totalIdeCredits,
		"qwen_remaining":    qwenRemaining,
		"qoder_remaining":   totalQoderCredits,
		"usd_cny":           p.usage.USDCNY(),
		"gateway_url":       "/v1",
		"listen":            p.cfg.Listen,
		"now":               time.Now().Unix(),
	})
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
			p.pool.MarkError(a, err)
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

// ── 模型列表 ─────────────────────────────────────────────

func (p *Panel) handleModels(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"
	type mv struct {
		ID          string `json:"id"`
		Name        string `json:"name,omitempty"`
		Description string `json:"description,omitempty"`
		Credits     string `json:"credits,omitempty"`
		Context     int64  `json:"context_window,omitempty"`
		MaxTokens   int64  `json:"max_tokens,omitempty"`
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
			list = append(list, mv{ID: m.ID, Name: m.Name, Description: m.Description, Credits: m.Credits, Context: m.ContextWindow, MaxTokens: m.MaxTokens})
		}
		out[pr.ID()] = list
	}
	writeJSON(w, http.StatusOK, out)
}

// ── 登录（OAuth 发起 + 轮询） ───────────────────────────

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
	s, err := pr.StartLogin(r.Context(), body.CallbackBase)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	t := &loginTicket{provider: body.Provider, session: s}
	p.mu.Lock()
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
	if res.Account != nil {
		if err := p.pool.AddAccount(res.Account); err == nil {
			go func() {
				p.sched.CheckinAccount(res.Account)
			}()
			log.Printf("PANEL: Trae 回调纳管 %s", res.Account.UID)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html><html><head><meta charset="utf-8">`+
		`<meta http-equiv="refresh" content="2;url=/">`+
		`<style>body{background:#0d1117;color:#e6edf3;font-family:"Microsoft YaHei",sans-serif;padding:60px;text-align:center}a{color:#58a6ff}</style></head><body>`+
		`<h3>✅ Trae 登录成功，账号已纳管</h3><p>2 秒后自动返回面板…</p><p><a href="/">立即返回</a></p></body></html>`)
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

// ── 工具 ─────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj)
}

