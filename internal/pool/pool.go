// Package pool 账号池：按 provider 分组管理多账号，轮询调度 + 错误冷却 + 模型路由。
package pool

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
	"chinacpa/internal/pubmodel"
	"chinacpa/internal/store"
)

// 冷却时长：按「账号 + 模型」粒度冷却，单个模型失败不影响同账号其他模型。
// 冷却是软性的：全部账号都在冷却时仍会尝试（避免冷却窗口内请求被硬拒），
// 但 auth 冷却时长远大于其他类型，配合 NeedsRelogin 标记停用失效账号。
const (
	rateCooldown  = 5 * time.Second  // 软限流
	upstreamCool  = 5 * time.Second  // 上游故障
	quotaCooldown = 30 * time.Second // 配额耗尽
	authCooldown  = 30 * time.Minute // 凭证失效（账号将被标记需重新登录）
)

// coolKey 冷却键：model 为空 = 账号级冷却（如 token 刷新失败），对该账号全部模型生效。
func coolKey(providerID, uid, model string) string {
	return providerID + "|" + uid + "|" + model
}

// rrKey 轮询游标键：provider|model。
func rrKey(providerID, model string) string { return providerID + "|" + model }

// Pool 全 provider 账号池。
type Pool struct {
	store *store.Store
	mu    sync.RWMutex
	accts []*model.Account
	provs map[string]provider.Provider
	// cooldown: provider|uid|model -> 解冻时刻（model 为空 = 账号级）
	cooldown map[string]time.Time
	// sticky 会话黏性：provider|sessionKey -> 会话条目（同一会话固定同一账号，保上游前缀缓存）
	stickyMu sync.Mutex
	sticky   map[string]*stickyEntry
	// rr 轮询游标：provider|model -> 下一分配序号（sessionKey 为空时按此分摊）
	rrMu sync.Mutex
	rr   map[string]uint64
	// inflight 账号实时负载：provider|uid -> 当前进行中的请求数（路由/面板用）
	inflightMu sync.Mutex
	inflight   map[string]int64
	// live 进行中链路：client|provider -> 请求数（拓扑图实时并发可视化用）
	liveMu sync.Mutex
	live   map[string]int64
	// refreshMu 单账号串行刷新（WorkBuddy 有防重放，并发刷新会被拒）
	refreshMu  sync.Mutex
	refreshers map[string]*sync.Mutex
	// modelAliases 模型别名表（config.model_aliases）
	modelAliases map[string]string
	// published 发布模型（对外名 → 多渠道）；nil = 未启用
	published *pubmodel.Manager
	// 模型缓存：provider -> models（带过期）
	modelsMu sync.Mutex
	models   map[string]*modelCache
	// OnAccountChanged 凭证更新回调（落盘）
	OnAccountChanged func(a *model.Account)
}

// stickyEntry 黏性会话条目（带最近使用时间，过期清理防无限增长）。
type stickyEntry struct {
	uid      string
	lastSeen time.Time
}

// stickyTTL 黏性会话保留时长：超过未活动的会话绑定过期（上游前缀缓存也早过了）。
const stickyTTL = 24 * time.Hour

type modelCache struct {
	models    []model.ModelInfo
	fetchedAt time.Time
}

// New 构造并加载已有账号。
func New(st *store.Store, provs []provider.Provider) (*Pool, error) {
	p := &Pool{
		store:        st,
		provs:        map[string]provider.Provider{},
		cooldown:     map[string]time.Time{},
		sticky:       map[string]*stickyEntry{},
		rr:           map[string]uint64{},
		inflight:     map[string]int64{},
		live:         map[string]int64{},
		refreshers:   map[string]*sync.Mutex{},
		modelAliases: map[string]string{},
		models:       map[string]*modelCache{},
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

// Providers 返回全部 provider 实例（按 ID 排序，保证遍历顺序确定）。
func (p *Pool) Providers() []provider.Provider {
	out := make([]provider.Provider, 0, len(p.provs))
	for _, pr := range p.provs {
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Provider 按 ID 取 provider。
func (p *Pool) Provider(id string) (provider.Provider, bool) {
	pr, ok := p.provs[id]
	return pr, ok
}

func key(a *model.Account) string { return a.Provider + "|" + a.UID }

// AddAccount 纳管账号（落盘 + 入池）。重新导入同一账号时视为凭证更新，
// 清除「需重新登录」标记与冷却。
func (p *Pool) AddAccount(a *model.Account) error {
	if err := p.store.SaveAccount(a); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	replaced := false
	for i, ex := range p.accts {
		if ex.Provider == a.Provider && ex.UID == a.UID {
			a.NeedsRelogin = false
			p.accts[i] = a
			replaced = true
			break
		}
	}
	if !replaced {
		a.NeedsRelogin = false
		p.accts = append(p.accts, a)
	}
	p.clearCooldownLocked(a.Provider, a.UID)
	return nil
}

// clearCooldownLocked 清掉该账号的全部冷却（调用方持锁）。
func (p *Pool) clearCooldownLocked(providerID, uid string) {
	prefix := providerID + "|" + uid + "|"
	for k := range p.cooldown {
		if strings.HasPrefix(k, prefix) {
			delete(p.cooldown, k)
		}
	}
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
	p.clearCooldownLocked(providerID, uid)
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

// AccountsBy 某 provider 的可用账号（未禁用、未标记需重新登录、无账号级冷却）。
// 忽略按模型的冷却：模型探测/面板场景不知道具体模型。
func (p *Pool) AccountsBy(providerID string) []*model.Account {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []*model.Account
	for _, a := range p.accts {
		if a.Provider != providerID || a.Disabled || a.NeedsRelogin {
			continue
		}
		if until, ok := p.cooldown[coolKey(providerID, a.UID, "")]; ok && until.After(now) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// AccountsByModel 某 provider 下调用指定模型可用的账号（额外排除该模型的冷却）。
func (p *Pool) AccountsByModel(providerID, modelName string) []*model.Account {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []*model.Account
	for _, a := range p.accts {
		if a.Provider != providerID || a.Disabled || a.NeedsRelogin {
			continue
		}
		if until, ok := p.cooldown[coolKey(providerID, a.UID, "")]; ok && until.After(now) {
			continue
		}
		if until, ok := p.cooldown[coolKey(providerID, a.UID, modelName)]; ok && until.After(now) {
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

// Pick 按 provider + 模型取一个可用账号；无可用返回错误。
// 黏性会话：sessionKey（prompt_cache_key / metadata.user_id）相同的请求固定同一账号，
// 保住上游提示词前缀缓存；新会话按轮询分摊到各账号；黏住的账号进入冷却/禁用时
// 自动换下一个并重新黏住。sessionKey 为空（简单调用）时优先选「实时负载最低」的账号
// （进行中请求数最少），负载相同再按 provider+model 轮询，避免全部流量压到第一个账号。
// 冷却是软性的：所有账号都在冷却中时仍照常尝试，避免冷却窗口内的请求被硬拒。
func (p *Pool) Pick(providerID, modelName, sessionKey string) (*model.Account, error) {
	avail := p.AccountsByModel(providerID, modelName)
	if len(avail) == 0 {
		avail = p.AccountsBy(providerID)
	}
	if len(avail) == 0 {
		return nil, fmt.Errorf("provider %s 无可用账号（未添加、已禁用或需重新登录）", providerID)
	}
	if sessionKey == "" {
		return p.pickLeastInflight(providerID, modelName, avail), nil
	}
	mapKey := providerID + "|" + sessionKey
	now := time.Now()
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()
	// 顺带清理过期会话绑定（防 map 无限增长；直连 model 名也可能撑大 cooldown，见 MarkError）
	if len(p.sticky) > 256 {
		for k, e := range p.sticky {
			if now.Sub(e.lastSeen) > stickyTTL {
				delete(p.sticky, k)
			}
		}
	}
	if e, ok := p.sticky[mapKey]; ok && now.Sub(e.lastSeen) <= stickyTTL {
		e.lastSeen = now
		for _, a := range avail {
			if a.UID == e.uid {
				return a, nil
			}
		}
	}
	// 新会话或黏住的账号当前不可用：选负载最低的账号并重新黏住
	a := p.pickLeastInflight(providerID, modelName, avail)
	p.sticky[mapKey] = &stickyEntry{uid: a.UID, lastSeen: now}
	return a, nil
}

// roundRobin 按 provider|model 轮询游标从 avail 中取账号（调用方保证 avail 非空）。
func (p *Pool) roundRobin(providerID, modelName string, avail []*model.Account) *model.Account {
	key := rrKey(providerID, modelName)
	p.rrMu.Lock()
	n := p.rr[key]
	p.rr[key] = n + 1
	p.rrMu.Unlock()
	return avail[n%uint64(len(avail))]
}

// pickLeastInflight 从 avail 中选「实时负载最低」的账号（进行中请求数最少），
// 负载相同时按 provider|model 轮询分摊。调用方保证 avail 非空。
func (p *Pool) pickLeastInflight(providerID, modelName string, avail []*model.Account) *model.Account {
	best := avail[0]
	bestN := p.Inflight(providerID, best.UID)
	ties := []*model.Account{best}
	for _, a := range avail[1:] {
		n := p.Inflight(providerID, a.UID)
		if n < bestN {
			best, bestN = a, n
			ties = []*model.Account{a}
		} else if n == bestN {
			ties = append(ties, a)
		}
	}
	if len(ties) == 1 {
		return best
	}
	// 负载相同：在并列集合里轮询，保证不扎堆到第一个
	return p.roundRobin(providerID, modelName, ties)
}

// Acquire 标记一次请求占用账号（路由选中后调用，请求结束调用 Release）。
func (p *Pool) Acquire(a *model.Account) {
	key := a.Provider + "|" + a.UID
	p.inflightMu.Lock()
	p.inflight[key]++
	p.inflightMu.Unlock()
}

// Release 标记一次请求结束（与 Acquire 配对）。
func (p *Pool) Release(a *model.Account) {
	key := a.Provider + "|" + a.UID
	p.inflightMu.Lock()
	if n := p.inflight[key]; n > 1 {
		p.inflight[key] = n - 1
	} else {
		delete(p.inflight, key)
	}
	p.inflightMu.Unlock()
}

// Inflight 查询账号当前进行中的请求数（0 = 空闲）。
func (p *Pool) Inflight(providerID, uid string) int64 {
	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()
	return p.inflight[providerID+"|"+uid]
}

// InflightAll 全部账号实时负载快照（面板展示用）。
func (p *Pool) InflightAll() map[string]int64 {
	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()
	out := make(map[string]int64, len(p.inflight))
	for k, v := range p.inflight {
		out[k] = v
	}
	return out
}

// LiveLink 一条进行中的「客户端 → 厂商」链路（拓扑图实时并发可视化）。
type LiveLink struct {
	Client   string `json:"client"`
	Provider string `json:"provider"`
	Count    int64  `json:"count"`
}

// LiveUp / LiveDown 记录一条进行中的客户端→厂商链路（与 Acquire/Release 并行维护，
// 仅用于面板拓扑展示；key = client|provider）。
func (p *Pool) LiveUp(client, provider string) {
	if client == "" {
		client = "未知"
	}
	p.liveMu.Lock()
	p.live[client+"|"+provider]++
	p.liveMu.Unlock()
}

func (p *Pool) LiveDown(client, provider string) {
	if client == "" {
		client = "未知"
	}
	key := client + "|" + provider
	p.liveMu.Lock()
	if n := p.live[key]; n > 1 {
		p.live[key] = n - 1
	} else {
		delete(p.live, key)
	}
	p.liveMu.Unlock()
}

// Live 当前所有进行中链路的快照（按 count 降序）。
func (p *Pool) Live() []LiveLink {
	p.liveMu.Lock()
	defer p.liveMu.Unlock()
	out := make([]LiveLink, 0, len(p.live))
	for k, v := range p.live {
		if v <= 0 {
			continue
		}
		c, pv, ok := splitLiveKey(k)
		if !ok {
			continue
		}
		out = append(out, LiveLink{Client: c, Provider: pv, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Client+out[i].Provider < out[j].Client+out[j].Provider
	})
	return out
}

// splitLiveKey 拆 client|provider（provider 不含 '|'，从最后一个分隔符切）。
func splitLiveKey(k string) (client, provider string, ok bool) {
	i := strings.LastIndexByte(k, '|')
	if i < 0 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}

// EnabledAccounts 某 provider 全部启用中的账号（忽略冷却，但排除需重新登录的账号）。
func (p *Pool) EnabledAccounts(providerID string) []*model.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []*model.Account
	for _, a := range p.accts {
		if a.Provider == providerID && !a.Disabled && !a.NeedsRelogin {
			out = append(out, a)
		}
	}
	return out
}

// MarkSuccess 调用成功：清该模型的冷却 + 记录使用时间。
func (p *Pool) MarkSuccess(a *model.Account, modelName string) {
	p.mu.Lock()
	delete(p.cooldown, coolKey(a.Provider, a.UID, modelName))
	changed := time.Now().Unix()-a.LastUsedAt > 60
	if changed {
		a.LastUsedAt = time.Now().Unix()
	}
	p.mu.Unlock()
	if changed && p.OnAccountChanged != nil {
		p.OnAccountChanged(a)
	}
}

// MarkError 调用失败：按「账号 + 模型」粒度冷却（modelName 为空 = 账号级，
// 对该账号全部模型生效，用于 token 刷新失败这类账号本身的问题）。
// 凭证失效（ErrAuth）额外把账号标记为需重新登录：调度层直接跳过，
// 避免坏账号被无限复用；回调在锁外触发，防止持锁调用 SaveAccount。
func (p *Pool) MarkError(a *model.Account, modelName string, err error) {
	pe := provider.AsError(err)
	var d time.Duration
	switch pe.Kind {
	case provider.ErrRate:
		d = rateCooldown
	case provider.ErrQuota:
		d = quotaCooldown
	case provider.ErrUpstream:
		d = upstreamCool
	case provider.ErrAuth:
		d = authCooldown
	default:
		return
	}
	relogin := false
	p.mu.Lock()
	p.cooldown[coolKey(a.Provider, a.UID, modelName)] = time.Now().Add(d)
	if pe.Kind == provider.ErrAuth && !a.NeedsRelogin {
		a.NeedsRelogin = true
		relogin = true
	}
	// 顺带清理已解冻的冷却条目（直连 provider:model 的模型名不受配置约束，防 map 无限增长）
	if len(p.cooldown) > 128 {
		now := time.Now()
		for k, until := range p.cooldown {
			if !until.After(now) {
				delete(p.cooldown, k)
			}
		}
	}
	p.mu.Unlock()
	if relogin && p.OnAccountChanged != nil {
		p.OnAccountChanged(a)
	}
}

// ClearNeedsRelogin 清除需重新登录标记（重新导入凭证 / 手动恢复时调用）。
func (p *Pool) ClearNeedsRelogin(providerID, uid string) bool {
	p.mu.Lock()
	var target *model.Account
	for _, a := range p.accts {
		if a.Provider == providerID && a.UID == uid && a.NeedsRelogin {
			a.NeedsRelogin = false
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return false
	}
	if p.OnAccountChanged != nil {
		p.OnAccountChanged(target)
	}
	return true
}

// CooldownUntil 查询账号冷却截止时刻（含全部按模型冷却，取最晚者；未冷却返回零值）。
func (p *Pool) CooldownUntil(providerID, uid string) time.Time {
	var latest time.Time
	for _, until := range p.CooldownModels(providerID, uid) {
		if until.After(latest) {
			latest = until
		}
	}
	return latest
}

// CooldownModels 账号当前生效的冷却明细：模型名（""=账号级）→ 解冻时刻。
func (p *Pool) CooldownModels(providerID, uid string) map[string]time.Time {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := map[string]time.Time{}
	prefix := providerID + "|" + uid + "|"
	for k, until := range p.cooldown {
		if !strings.HasPrefix(k, prefix) || !until.After(now) {
			continue
		}
		out[strings.TrimPrefix(k, prefix)] = until
	}
	return out
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

// SetPublished 注入发布模型管理器（对外名 → 多渠道路由）。
func (p *Pool) SetPublished(m *pubmodel.Manager) {
	p.published = m
}

// Published 返回发布模型管理器（未启用返回 nil）。
func (p *Pool) Published() *pubmodel.Manager {
	return p.published
}

// ChannelTarget 一个候选上游渠道。
type ChannelTarget struct {
	PID   string // provider ID
	Model string // 上游模型裸名
}

// Route 一次请求的完整路由：对外名（记账/计价用）+ 有序渠道列表。
type Route struct {
	Public   string
	Channels []ChannelTarget
	// Published 是否命中发布模型（渠道计数只对发布模型记录）
	Published bool
	// Type 发布模型类别：chat（默认）/ image / video（空 = 未标注，按对话处理）；
	// 网关据此校验「图片模型不能走对话端点」等错配并给出提示
	Type string
}

// ResolveRoute 解析请求模型名为路由：
//   - 发布模型命中 → 渠道列表（priority 严格按配置顺序；round_robin 轮询起点轮转）
//   - 别名表命中 → 替换后继续
//   - "provider:model" → 单渠道
//   - 裸名 → 全平台模型表唯一命中
func (p *Pool) ResolveRoute(ctx context.Context, name string) (*Route, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("缺少 model 参数")
	}
	public := name
	if p.published != nil {
		if mo, ok := p.published.Lookup(name); ok {
			var chs []ChannelTarget
			for _, ch := range mo.Channels {
				if _, ok := p.provs[ch.Provider]; ok {
					chs = append(chs, ChannelTarget{PID: ch.Provider, Model: ch.Model})
				}
			}
			if len(chs) == 0 {
				return nil, fmt.Errorf("模型 %q 的渠道均不可用（平台未注册）", name)
			}
			if mo.Strategy == pubmodel.StrategyRoundRobin {
				// 轮询起点轮转：把渠道列表按游标旋转，实现渠道间负载均衡
				chs = p.rotateChannels(chs)
			}
			// priority 策略：严格按用户配置的优先级排序（如：国内优先，国际兜底）
			return &Route{Public: mo.ID, Channels: chs, Published: true, Type: mo.Type}, nil
		}
	}
	pid, mname, err := p.ResolveModel(ctx, name)
	if err != nil {
		// 未命中发布模型时给出更贴合的提示
		if p.published != nil && !p.published.Empty() {
			return nil, fmt.Errorf("模型 %q 未发布；可用模型见 GET /v1/models（或用 provider:model 直连）", name)
		}
		return nil, err
	}
	return &Route{Public: public, Channels: []ChannelTarget{{PID: pid, Model: mname}}}, nil
}

// rotateChannels 按发布模型级轮询游标旋转渠道列表。
func (p *Pool) rotateChannels(chs []ChannelTarget) []ChannelTarget {
	var sb strings.Builder
	for i, c := range chs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(c.PID + "/" + c.Model)
	}
	key := "chan:" + sb.String()
	p.rrMu.Lock()
	n := p.rr[key]
	p.rr[key] = n + 1
	p.rrMu.Unlock()
	rot := int(n % uint64(len(chs)))
	if rot == 0 {
		return chs
	}
	out := make([]ChannelTarget, 0, len(chs))
	out = append(out, chs[rot:]...)
	out = append(out, chs[:rot]...)
	return out
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
	// 单账号串行刷新：WorkBuddy 刷新有防重放（12153），并发刷新会被上游拒绝
	p.refreshMu.Lock()
	mu := p.refreshers[key(a)]
	if mu == nil {
		mu = &sync.Mutex{}
		p.refreshers[key(a)] = mu
	}
	p.refreshMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	changed, err := pr.RefreshToken(ctx, a)
	if err != nil {
		p.MarkError(a, "", err) // token 刷新失败是账号级问题，冷却整个账号
		return false, err
	}
	if changed {
		// 刷新成功说明凭证仍是有效的：清除之前的「需重新登录」标记
		p.ClearNeedsRelogin(a.Provider, a.UID)
		if p.OnAccountChanged != nil {
			p.OnAccountChanged(a)
		}
	}
	return changed, nil
}
