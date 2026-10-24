// Package scheduler 定时任务：每日自动签到、WorkBuddy 任务中心自动完成、
// 额度定时刷新、token 保活。轻量 tick 轮询（每分钟），无需三方 cron 库。
package scheduler

import (
	"context"
	"log"
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

	lastCheckinDay string
	lastTokenDay   string
	lastQuotaTick  time.Time
	quit           chan struct{}
}

// New 构造。
func New(cfg *config.Config, p *pool.Pool, st *store.Store) *Scheduler {
	return &Scheduler{
		cfg:  cfg,
		pool: p,
		st:   st,
		quit: make(chan struct{}),
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

// Stop 停止。
func (s *Scheduler) Stop() { close(s.quit) }

func (s *Scheduler) tick(now time.Time) {
	cfg := s.cfg.Schedule
	// 1) 额度刷新
	if now.Sub(s.lastQuotaTick) >= time.Duration(cfg.QuotaRefreshMinutes)*time.Minute {
		s.lastQuotaTick = now
		s.refreshAllQuotas()
	}
	hour := now.Hour()
	day := now.Format("2006-01-02")
	// 2) 每日签到（在配置小时内的第一次 tick 执行一次）
	if cfg.CheckinEnabled && s.lastCheckinDay != day && containsHour(cfg.CheckinHours, hour) {
		s.lastCheckinDay = day
		go s.runAllCheckins()
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

// runAllCheckins 全账号签到（跳过禁用）。
func (s *Scheduler) runAllCheckins() {
	for _, a := range s.pool.Accounts() {
		if a.Disabled {
			continue
		}
		s.CheckinAccount(a)
	}
}

// CheckinAccount 单账号签到（面板手动触发同一入口）：
// 签到 → WorkBuddy 任务中心自动接受/领奖 → 立即刷新额度。
func (s *Scheduler) CheckinAccount(a *model.Account) *model.CheckinResult {
	pr, ok := s.pool.Provider(a.Provider)
	if !ok {
		return &model.CheckinResult{OK: false, Message: "未知平台"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// 临期 token 先刷新
	if _, err := s.pool.EnsureFreshToken(ctx, a); err != nil {
		log.Printf("SCHED: [%s] token 刷新失败: %v", a.Provider+"/"+label(a), err)
	}
	var res *model.CheckinResult
	if r, err := pr.Checkin(ctx, a); err != nil {
		if err == provider.ErrNotSupported {
			res = &model.CheckinResult{OK: false, Message: "该平台无每日签到"}
		} else {
			res = &model.CheckinResult{OK: false, Message: err.Error()}
		}
	} else {
		res = r
	}
	// WorkBuddy：任务中心自动完成
	if s.cfg.Schedule.TasksEnabled {
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
	// 签到后立刻刷新额度
	go s.refreshQuota(a)
	return res
}

// label 账号展示标签。
func label(a *model.Account) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	return a.UID
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
