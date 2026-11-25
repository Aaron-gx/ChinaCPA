// Package scheduler 定时任务：每日自动签到、WorkBuddy 任务中心自动完成、
// 额度定时刷新、token 保活。轻量 tick 轮询（每分钟），无需三方 cron 库。
package scheduler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"chinacpa/internal/config"
	"chinacpa/internal/model"
	"chinacpa/internal/pool"
	"chinacpa/internal/provider"
	"chinacpa/internal/store"
)

// Scheduler 定时器。
type Scheduler struct {
	cfg  *config.Config
	pool *pool.Pool
	st   *store.Store

	// 定时签到、导入账号和面板按钮共用同一账号的在途操作。
	checkinMu     sync.Mutex
	checkins      map[[2]string]*checkinFlight
	lastTokenDay  string
	lastQuotaTick time.Time
	quit          chan struct{}
	stopOnce      sync.Once
}

type checkinFlight struct {
	done   chan struct{}
	result model.CheckinResult
}

// New 构造。
func New(cfg *config.Config, p *pool.Pool, st *store.Store) *Scheduler {
	return &Scheduler{
		cfg:      cfg,
		pool:     p,
		st:       st,
		checkins: map[[2]string]*checkinFlight{},
		quit:     make(chan struct{}),
	}
}

// Start 启动（阻塞前先跑一次额度刷新）。
func (s *Scheduler) Start() {
	go func() {
		// 启动即刷一轮额度
		s.refreshAllQuotas()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-s.quit:
				return
			case now := <-t.C:
				s.tick(now)
			}
		}
	}()
}

// Stop 停止（幂等）。
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.quit) })
}

func (s *Scheduler) tick(now time.Time) {
	cfg := s.cfg.Schedule
	// 1) 额度刷新
	if now.Sub(s.lastQuotaTick) >= time.Duration(cfg.QuotaRefreshMinutes)*time.Minute {
		s.lastQuotaTick = now
		s.refreshAllQuotas()
	}
	hour := now.Hour()
	day := now.Format("2006-01-02")
	// 2) 每日签到：配置小时窗口内启动；失败的账号不记日期，下个 tick 自动重试
	if cfg.CheckinEnabled && containsHour(cfg.CheckinHours, hour) {
		s.tickCheckins()
	}
	// 3) token 保活
	if s.lastTokenDay != day && containsHour(cfg.TokenRefreshHours, hour) {
		s.lastTokenDay = day
		go s.runTokenKeepalive()
	}
}

func containsHour(hours []int, h int) bool {
	for _, x := range hours {
		if x == h {
			return true
		}
	}
	return false
}

// tickCheckins 按账号检查落盘的签到记录，每个账号独立重试。
func (s *Scheduler) tickCheckins() {
	for _, a := range s.pool.Accounts() {
		if a.Disabled || a.NeedsRelogin {
			continue
		}
		if r, ok := s.st.TodayCheckin(a.Provider, a.UID); ok && (r.OK || r.Already) {
			continue
		}
		go s.CheckinAccount(a)
	}
}

// runAllCheckins 全账号签到（跳过禁用与需重新登录）。
func (s *Scheduler) runAllCheckins() {
	for _, a := range s.pool.Accounts() {
		if a.Disabled || a.NeedsRelogin {
			continue
		}
		s.CheckinAccount(a)
	}
}

// CheckinAccount 单账号签到（面板手动触发同一入口）：
// 签到 → WorkBuddy 任务中心自动接受/领奖 → 立即刷新额度。
func (s *Scheduler) CheckinAccount(a *model.Account) *model.CheckinResult {
	key := [2]string{a.Provider, a.UID}
	s.checkinMu.Lock()
	if inFlight := s.checkins[key]; inFlight != nil {
		s.checkinMu.Unlock()
		<-inFlight.done
		result := inFlight.result
		return &result
	}
	if r, ok := s.st.TodayCheckin(a.Provider, a.UID); ok && (r.OK || r.Already) {
		s.checkinMu.Unlock()
		return &r
	}
	inFlight := &checkinFlight{done: make(chan struct{})}
	s.checkins[key] = inFlight
	s.checkinMu.Unlock()

	res := s.checkinAccount(a)
	s.checkinMu.Lock()
	inFlight.result = *res
	delete(s.checkins, key)
	close(inFlight.done)
	s.checkinMu.Unlock()
	return res
}

func (s *Scheduler) checkinAccount(a *model.Account) *model.CheckinResult {
	pr, ok := s.pool.Provider(a.Provider)
	if !ok {
		return &model.CheckinResult{OK: false, Message: "未知平台"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var res *model.CheckinResult
	if _, err := s.pool.EnsureFreshToken(ctx, a); err != nil {
		res = &model.CheckinResult{Message: err.Error()}
	} else {
		r, err := pr.Checkin(ctx, a)
		switch {
		case errors.Is(err, provider.ErrNotSupported):
			res = &model.CheckinResult{Info: true, Message: "该平台无每日签到"}
		case err != nil:
			if provider.AsError(err).Kind == provider.ErrAuth {
				s.pool.MarkError(a, "", err)
			}
			res = &model.CheckinResult{Message: err.Error()}
		default:
			if r == nil {
				res = &model.CheckinResult{Message: "平台未返回签到结果"}
			} else {
				res = r
			}
		}
	}
	// WorkBuddy：任务中心自动完成
	if s.cfg.Schedule.TasksEnabled && !a.NeedsRelogin {
		if wb, ok := pr.(interface {
			RunTasks(ctx context.Context, a *model.Account) (int64, string, error)
		}); ok {
			if earned, summary, err := wb.RunTasks(ctx, a); err == nil && earned > 0 {
				res.Message += "；任务中心 +" + itoa(earned) + " 积分（" + summary + "）"
				res.CreditsEarned += earned
			}
		}
	}
	s.st.RecordCheckin(a.Provider, a.UID, *res)
	log.Printf("SCHED: [%s] 签到: %s", a.Provider+"/"+label(a), res.Message)
	if res.OK || res.Already || res.Info {
		go s.refreshQuota(a)
	}
	return res
}

// label 账号展示标签。
func label(a *model.Account) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	return a.UID
}

// RefreshAllQuotas 手动触发全账号额度刷新。
func (s *Scheduler) RefreshAllQuotas() {
	s.refreshAllQuotas()
}

// refreshAllQuotas 全账号额度刷新。
func (s *Scheduler) refreshAllQuotas() {
	for _, a := range s.pool.Accounts() {
		if a.Disabled {
			continue
		}
		s.refreshQuota(a)
	}
}

// refreshQuota 单账号额度刷新（失败静默，保留旧快照）。
func (s *Scheduler) refreshQuota(a *model.Account) {
	pr, ok := s.pool.Provider(a.Provider)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.pool.EnsureFreshToken(ctx, a); err != nil {
		return
	}
	q, err := pr.Quota(ctx, a)
	if err != nil {
		log.Printf("SCHED: [%s] 额度刷新失败: %v", a.Provider+"/"+label(a), err)
		return
	}
	s.st.SetQuota(q)
}

// runTokenKeepalive token 保活刷新。
func (s *Scheduler) runTokenKeepalive() {
	for _, a := range s.pool.Accounts() {
		if a.Disabled {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := s.pool.EnsureFreshToken(ctx, a); err != nil {
			log.Printf("SCHED: [%s] 保活刷新失败: %v", a.Provider+"/"+label(a), err)
		}
		cancel()
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
