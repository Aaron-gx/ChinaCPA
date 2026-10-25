// Package store 负责 ChinaCPA 落盘数据：账号凭证、额度快照、签到历史、用量日志。
// 全部 JSON / JSONL，原子写（tmp+rename），零三方依赖。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
)

// Store 数据目录管理器。
type Store struct {
	dir string
	mu  sync.Mutex
	// quotaCache uid -> 最新额度快照（内存镜像 + state.json 落盘）
	quotaCache map[string]*model.QuotaInfo
	// checkinLog provider:uid:date -> 结果（用于面板展示与幂等提示）
	checkinLog map[string]model.CheckinResult
}

// Open 初始化数据目录并加载既有状态。
func Open(dir string) (*Store, error) {
	s := &Store{
		dir:        dir,
		quotaCache: map[string]*model.QuotaInfo{},
		checkinLog: map[string]model.CheckinResult{},
	}
	for _, sub := range []string{"", "accounts", "usage"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("init data dir: %w", err)
		}
	}
	s.loadState()
	return s, nil
}

// Dir 返回数据目录。
func (s *Store) Dir() string { return s.dir }

// ── 账号凭证 ──────────────────────────────────────────────

func (s *Store) accountFile(provider, uid string) string {
	safe := sanitize(provider) + "-" + sanitize(uid)
	return filepath.Join(s.dir, "accounts", safe+".json")
}

// SaveAccount 保存账号凭证（原子写）。
func (s *Store) SaveAccount(a *model.Account) error {
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.accountFile(a.Provider, a.UID), raw)
}

// DeleteAccount 删除账号凭证文件。
func (s *Store) DeleteAccount(provider, uid string) error {
	err := os.Remove(s.accountFile(provider, uid))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s.mu.Lock()
	delete(s.quotaCache, provider+":"+uid)
	delete(s.checkinLog, provider+":"+uid+":"+time.Now().Format("2006-01-02"))
	s.mu.Unlock()
	return nil
}

// LoadAccounts 扫描 accounts 目录加载全部账号。
func (s *Store) LoadAccounts() ([]*model.Account, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "accounts"))
	if err != nil {
		return nil, err
	}
	var out []*model.Account
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, "accounts", e.Name()))
		if err != nil {
			continue
		}
		var a model.Account
		if err := json.Unmarshal(raw, &a); err != nil || a.UID == "" {
			continue
		}
		if a.Provider == "" {
			// 从文件名还原 provider
			base := strings.TrimSuffix(e.Name(), ".json")
			if i := strings.Index(base, "-"); i > 0 {
				a.Provider = base[:i]
			}
		}
		out = append(out, &a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider+out[i].UID < out[j].Provider+out[j].UID })
	return out, nil
}

// ── 状态（额度快照 + 签到历史） ───────────────────────────

type stateFile struct {
	Quotas     map[string]*model.QuotaInfo    `json:"quotas"`
	CheckinLog map[string]model.CheckinResult `json:"checkin_log"`
}

func (s *Store) statePath() string { return filepath.Join(s.dir, "state.json") }

func (s *Store) loadState() {
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		return
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	if st.Quotas != nil {
		s.quotaCache = st.Quotas
	}
	if st.CheckinLog != nil {
		s.checkinLog = st.CheckinLog
	}
}

func (s *Store) persistStateLocked() {
	st := stateFile{Quotas: s.quotaCache, CheckinLog: s.checkinLog}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWrite(s.statePath(), raw)
}

// SetQuota 更新额度快照（内存 + 落盘）。
func (s *Store) SetQuota(q *model.QuotaInfo) {
	q.FetchedAt = time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotaCache[q.Provider+":"+q.UID] = q
	s.persistStateLocked()
}

// GetQuota 读取额度快照（无缓存返回 nil）。
func (s *Store) GetQuota(provider, uid string) *model.QuotaInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q, ok := s.quotaCache[provider+":"+uid]; ok {
		cp := *q
		return &cp
	}
	return nil
}

// AllQuotas 返回全部快照副本。
func (s *Store) AllQuotas() map[string]*model.QuotaInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*model.QuotaInfo, len(s.quotaCache))
	for k, v := range s.quotaCache {
		cp := *v
		out[k] = &cp
	}
	return out
}

// RecordCheckin 记录今日签到结果。
func (s *Store) RecordCheckin(provider, uid string, r model.CheckinResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + ":" + uid + ":" + time.Now().Format("2006-01-02")
	s.checkinLog[key] = r
	// 只保留最近 7 天
	cutoff := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	for k := range s.checkinLog {
		if d := k[strings.LastIndex(k, ":")+1:]; d < cutoff {
			delete(s.checkinLog, k)
		}
	}
	s.persistStateLocked()
}

// TodayCheckin 返回今日签到结果。
func (s *Store) TodayCheckin(provider, uid string) (model.CheckinResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.checkinLog[provider+":"+uid+":"+time.Now().Format("2006-01-02")]
	return r, ok
}

// ── 工具 ─────────────────────────────────────────────────

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
