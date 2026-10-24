// Package pool 账号池：按 provider 分组管理多账号，轮询调度 + 错误冷却 + 模型路由。
package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
	"chinacpa/internal/store"
)

// 冷却时长。
const (
	rateCooldown  = 60 * time.Second   // 软限流
	upstreamCool  = 30 * time.Second   // 上游故障
	quotaCooldown = 2 * time.Hour      // 配额耗尽（到下次额度刷新自动解冻）
)

// Pool 全 provider 账号池。
type Pool struct {
	store *store.Store
	mu    sync.RWMutex
	accts []*model.Account
	provs map[string]provider.Provider
	// cooldown: provider|uid -> 解冻时刻；负值表示禁用直到手动启用
	cooldown map[string]time.Time
	// rr 每个 provider 的轮询游标
	rr map[string]int
	// modelAliases 模型别名表（config.model_aliases）
	modelAliases map[string]string
	// 模型缓存：provider -> models（带过期）
	modelsMu sync.Mutex
	models   map[string]*modelCache
	// OnAccountChanged 凭证更新回调（落盘）
	OnAccountChanged func(a *model.Account)
}

type modelCache struct {
	models   []model.ModelInfo
	fetchedAt time.Time
}

// New 构造并加载已有账号。
func New(st *store.Store, provs []provider.Provider) (*Pool, error) {
	p := &Pool{
		store:    st,
		provs:    map[string]provider.Provider{},
		cooldown: map[string]time.Time{},
		rr:           map[string]int{},
		modelAliases: map[string]string{},
		models:   map[string]*modelCache{},
	}
	for _, pr := range provs {
		p.provs[pr.ID()] = pr
	}
	accts, err := st.LoadAccounts()
	if err != nil {
		return nil, err
	}
	p.accts = accts
	return p, nil
}

// Providers 返回全部 provider 实例。
func (p *Pool) Providers() []provider.Provider {
	out := make([]provider.Provider, 0, len(p.provs))
	for _, pr := range p.provs {
		out = append(out, pr)
	}
	return out
}

// Provider 按 ID 取 provider。
func (p *Pool) Provider(id string) (provider.Provider, bool) {
	pr, ok := p.provs[id]
	return pr, ok
}

func key(a *model.Account) string { return a.Provider + "|" + a.UID }

// AddAccount 纳管账号（落盘 + 入池）。
func (p *Pool) AddAccount(a *model.Account) error {
	if err := p.store.SaveAccount(a); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	replaced := false
	for i, ex := range p.accts {
		if ex.Provider == a.Provider && ex.UID == a.UID {
			p.accts[i] = a
			replaced = true
			break
		}
	}
	if !replaced {
		p.accts = append(p.accts, a)
	}
	delete(p.cooldown, key(a))
	return nil
}

// RemoveAccount 删除账号。
func (p *Pool) RemoveAccount(providerID, uid string) error {
	if err := p.store.DeleteAccount(providerID, uid); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, ex := range p.accts {
		if ex.Provider == providerID && ex.UID == uid {
			p.accts = append(p.accts[:i], p.accts[i+1:]...)
			break
		}
	}
	delete(p.cooldown, providerID+"|"+uid)
	return nil
}

// Accounts 账号列表副本。
func (p *Pool) Accounts() []*model.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*model.Account, len(p.accts))
	copy(out, p.accts)
	return out
}

// AccountsBy 某 provider 的可用（未禁用未冷却）账号。
func (p *Pool) AccountsBy(providerID string) []*model.Account {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []*model.Account
	for _, a := range p.accts {
		if a.Provider != providerID || a.Disabled {
			continue
		}
		if until, ok := p.cooldown[key(a)]; ok && until.After(now) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// AllAccountsBy 某 provider 全部账号（含冷却/禁用，面板用）。
func (p *Pool) AllAccountsBy(providerID string) []*model.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []*model.Account
	for _, a := range p.accts {
		if a.Provider == providerID {
			out = append(out, a)
		}
	}
	return out
}

// SetDisabled 启用/禁用账号。
func (p *Pool) SetDisabled(providerID, uid string, disabled bool) error {
	p.mu.Lock()
	var target *model.Account
	for _, a := range p.accts {
		if a.Provider == providerID && a.UID == uid {
			a.Disabled = disabled
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return fmt.Errorf("账号不存在")
	}
	return p.store.SaveAccount(target)
}

// Pick 按 provider 轮询取一个可用账号；无可用返回错误。
func (p *Pool) Pick(providerID string) (*model.Account, error) {
	avail := p.AccountsBy(providerID)
	if len(avail) == 0 {
		return nil, fmt.Errorf("provider %s 无可用账号（未添加/已禁用/冷却中）", providerID)
	}
	p.mu.Lock()
	p.rr[providerID] = (p.rr[providerID] + 1) % len(avail)
	idx := p.rr[providerID]
	p.mu.Unlock()
	return avail[idx%len(avail)], nil
}

// MarkSuccess 调用成功：清冷却 + 记录使用时间。
func (p *Pool) MarkSuccess(a *model.Account) {
	p.mu.Lock()
	delete(p.cooldown, key(a))
	changed := time.Now().Unix()-a.LastUsedAt > 60
	if changed {
		a.LastUsedAt = time.Now().Unix()
	}
	p.mu.Unlock()
	if changed && p.OnAccountChanged != nil {
		p.OnAccountChanged(a)
	}
}

// MarkError 调用失败：按错误类型冷却。
func (p *Pool) MarkError(a *model.Account, err error) {
	pe := provider.AsError(err)
	d := time.Duration(0)
	switch pe.Kind {
	case provider.ErrRate:
		d = rateCooldown
	case provider.ErrQuota:
		d = quotaCooldown
	case provider.ErrUpstream:
		d = upstreamCool
	case provider.ErrAuth:
		// 凭证失效：冷却 30 分钟（等重新登录），面板标红
		d = 30 * time.Minute
	default:
		return
	}
	p.mu.Lock()
	p.cooldown[key(a)] = time.Now().Add(d)
	p.mu.Unlock()
}

// CooldownUntil 查询账号冷却截止时刻（未冷却返回零值）。
func (p *Pool) CooldownUntil(providerID, uid string) time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cooldown[providerID+"|"+uid]
}

// SaveAccount 保存账号凭证变更。
func (p *Pool) SaveAccount(a *model.Account) error {
	if err := p.store.SaveAccount(a); err != nil {
		return err
	}
	p.mu.Lock()
	replaced := false
	for i, ex := range p.accts {
		if ex.Provider == a.Provider && ex.UID == a.UID {
			p.accts[i] = a
			replaced = true
			break
		}
	}
	if !replaced {
		p.accts = append(p.accts, a)
	}
	p.mu.Unlock()
	return nil
}

// ── 模型缓存与路由 ──────────────────────────────────────

// RefreshModels 拉取某 provider 全部可用账号的模型（用第一个可用账号探测，10 分钟缓存）。
func (p *Pool) RefreshModels(ctx context.Context, providerID string, force bool) ([]model.ModelInfo, error) {
	p.modelsMu.Lock()
	c, ok := p.models[providerID]
	if ok && !force && time.Since(c.fetchedAt) < 10*time.Minute {
		p.modelsMu.Unlock()
		return c.models, nil
	}
	p.modelsMu.Unlock()
	pr, ok := p.provs[providerID]
	if !ok {
		return nil, fmt.Errorf("unknown provider %s", providerID)
	}
	accts := p.AccountsBy(providerID)
	if len(accts) == 0 {
		accts = p.AllAccountsBy(providerID)
	}
	if len(accts) == 0 {
		return nil, fmt.Errorf("provider %s 无账号", providerID)
	}
	var (
		models []model.ModelInfo
		err    error
	)
	for _, a := range accts {
		models, err = pr.Models(ctx, a)
		if err == nil && len(models) > 0 {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	p.modelsMu.Lock()
	p.models[providerID] = &modelCache{models: models, fetchedAt: time.Now()}
	p.modelsMu.Unlock()
	return models, nil
}

// AllModels 全部 provider 模型（含 provider 前缀形态）。
func (p *Pool) AllModels(ctx context.Context) (map[string][]model.ModelInfo, error) {
	out := map[string][]model.ModelInfo{}
	for id := range p.provs {
		ms, err := p.RefreshModels(ctx, id, false)
		if err != nil {
			out[id] = nil
			continue
		}
		out[id] = ms
	}
	return out, nil
}

// SetModelAliases 注入模型别名表（客户端固定模型名 → ChinaCPA 模型名）。
func (p *Pool) SetModelAliases(m map[string]string) {
	p.modelAliases = m
}

// ResolveModel 模型名解析：
//   - 别名表命中（Codex/Claude Code 固定模型名 → 实际模型）
//   - "provider:model" → 直接路由
//   - 裸名 → 在全部 provider 模型表中查找；唯一命中即用，多命中/未命中报错
func (p *Pool) ResolveModel(ctx context.Context, name string) (providerID, modelName string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fmt.Errorf("缺少 model 参数")
	}
	p.mu.RLock()
	alias := p.modelAliases[name]
	p.mu.RUnlock()
	if alias != "" {
		name = alias
	}
	if i := strings.Index(name, ":"); i > 0 {
		pid, mname := name[:i], name[i+1:]
		if _, ok := p.provs[pid]; ok {
			return pid, mname, nil
		}
	}
	all, err := p.AllModels(ctx)
	if err != nil {
		return "", "", err
	}
	var hits []string
	for pid, ms := range all {
		if ms == nil {
			continue
		}
		for _, m := range ms {
			if strings.EqualFold(m.ID, name) {
				hits = append(hits, pid)
				break
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], name, nil
	case 0:
		return "", "", fmt.Errorf("模型 %q 不存在；可用前缀 workbuddy:/qwen:/trae: 或查看 GET /v1/models", name)
	default:
		return "", "", fmt.Errorf("模型 %q 在多个平台存在（%s），请用 provider:model 指定", name, strings.Join(hits, ", "))
	}
}

// EnsureFreshToken 对账号做临期 token 刷新（返回是否落盘）。
func (p *Pool) EnsureFreshToken(ctx context.Context, a *model.Account) (bool, error) {
	pr, ok := p.provs[a.Provider]
	if !ok {
		return false, nil
	}
	changed, err := pr.RefreshToken(ctx, a)
	if err != nil {
		p.MarkError(a, err)
		return false, err
	}
	if changed && p.OnAccountChanged != nil {
		p.OnAccountChanged(a)
	}
	return changed, nil
}
