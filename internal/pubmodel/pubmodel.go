// Package pubmodel 发布模型配置：对外模型 → 多渠道（provider + 上游真实模型）映射 + 单价。
// 面板可视化编辑，落盘 data/models.json；网关据此路由并对外暴露模型列表。
package pubmodel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Channel 一个上游渠道：某平台的一个真实模型。
type Channel struct {
	Provider string `json:"provider"` // workbuddy / qwen / trae / qoder / qoder-intl
	Model    string `json:"model"`    // 上游模型裸名（不带前缀）
	Note     string `json:"note,omitempty"`
}

// Pricing 单价。对话模型用 Input/Output/Cache（$/1M tokens）；
// 图片/视频模型用 PerCall（$/次）。
type Pricing struct {
	Input   float64 `json:"input"`
	Output  float64 `json:"output"`
	Cache   float64 `json:"cache,omitempty"` // 缓存 token 单价（$/1M），0 = 缓存按输入价计
	PerCall float64 `json:"per_call,omitempty"` // 按次单价（$/张、$/条），0 = 无
}

// Model 一个对外发布的模型。
type Model struct {
	ID       string    `json:"id"`                // 对外模型名（客户端 model 参数写这个）
	Note     string    `json:"note,omitempty"`    // 备注/显示名
	Type     string    `json:"type,omitempty"`    // 类别：chat（默认）/ image / video
	Pricing  *Pricing  `json:"pricing,omitempty"` // 单价；空 = 不计价
	Channels []Channel `json:"channels"`          // 上游渠道（按序失败转移）
	// Strategy 渠道选路策略：priority（默认，严格按配置顺序，免费额度优先）/ round_robin（轮询起点轮转）
	Strategy string `json:"strategy,omitempty"`
}

// 渠道选路策略常量。
const (
	StrategyPriority   = "priority"
	StrategyRoundRobin = "round_robin"
)

// normalizeStrategy 归一化渠道策略（空/非法回落 priority）。
func normalizeStrategy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case StrategyRoundRobin:
		return StrategyRoundRobin
	default:
		return StrategyPriority
	}
}

// normalizeType 归一化类别（空 = chat）。
func normalizeType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "image":
		return "image"
	case "video":
		return "video"
	default:
		return ""
	}
}

// Config 发布模型配置文件。
type Config struct {
	Models []*Model `json:"models"`
	// USDCNY 美元兑人民币汇率（费用估算折算；<=0 用 usage 内置值）
	USDCNY float64 `json:"usd_cny,omitempty"`
}

// Manager 发布模型管理器（并发安全，pool/gateway/panel 共享同一实例）。
type Manager struct {
	mu   sync.RWMutex
	file string
	cfg  *Config

	// stats 渠道调用计数：key = lower(pubID)|provider|model。
	// 独立落盘 statsFile（Load 恢复 + 后台周期刷盘），跨重启保留。
	statsMu    sync.Mutex
	stats      map[string]*ChanStat
	statsFile  string
	statsDirty bool
}

// ChanStat 渠道成功/失败计数。
type ChanStat struct {
	OK   int64 `json:"ok"`
	Fail int64 `json:"fail"`
}

func chanStatKey(pubID, provider, model string) string {
	return strings.ToLower(strings.TrimSpace(pubID)) + "|" + provider + "|" + model
}

// BumpChannel 渠道调用计数 +1（标脏，由后台刷盘器持久化）。
func (m *Manager) BumpChannel(pubID, provider, model string, ok bool) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	if m.stats == nil {
		m.stats = map[string]*ChanStat{}
	}
	st := m.stats[chanStatKey(pubID, provider, model)]
	if st == nil {
		st = &ChanStat{}
		m.stats[chanStatKey(pubID, provider, model)] = st
	}
	if ok {
		st.OK++
	} else {
		st.Fail++
	}
	m.statsDirty = true
}

// ChannelStats 渠道计数快照。
func (m *Manager) ChannelStats() map[string]ChanStat {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	out := map[string]ChanStat{}
	for k, v := range m.stats {
		out[k] = *v
	}
	return out
}

// FlushStats 脏则原子写盘（后台刷盘器与测试共用）。
func (m *Manager) FlushStats() error {
	m.statsMu.Lock()
	if !m.statsDirty || m.statsFile == "" {
		m.statsMu.Unlock()
		return nil
	}
	snap := map[string]ChanStat{}
	for k, v := range m.stats {
		snap[k] = *v
	}
	m.statsDirty = false
	m.statsMu.Unlock()
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.statsFile, raw)
}

// StartStatsFlusher 启动后台周期刷盘（interval 内至多一次写盘，进程无常驻退出钩子时兜底）。
func (m *Manager) StartStatsFlusher(interval time.Duration) {
	if m.statsFile == "" {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			_ = m.FlushStats()
		}
	}()
}

// Load 从文件加载；文件不存在返回空配置。statsFile 为渠道计数持久化路径（空 = 不持久化）。
func Load(file, statsFile string) (*Manager, error) {
	m := &Manager{file: file, cfg: &Config{}, statsFile: statsFile, stats: map[string]*ChanStat{}}
	if statsFile != "" {
		if raw, err := os.ReadFile(statsFile); err == nil {
			var snap map[string]ChanStat
			if json.Unmarshal(raw, &snap) == nil {
				for k, v := range snap {
					c := v
					m.stats[k] = &c
				}
			}
		}
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, fmt.Errorf("read published models: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse published models %s: %w", file, err)
	}
	m.cfg = &cfg
	return m, nil
}

// Get 配置深拷贝（面板展示/编辑用）。
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	raw, _ := json.Marshal(m.cfg)
	var out Config
	_ = json.Unmarshal(raw, &out)
	return &out
}

// Save 校验并保存配置（原子写盘 + 更新内存）。
func (m *Manager) Save(cfg *Config) error {
	if cfg == nil {
		cfg = &Config{}
	}
	seen := map[string]bool{}
	for _, mo := range cfg.Models {
		mo.ID = strings.TrimSpace(mo.ID)
		if mo.ID == "" {
			return fmt.Errorf("存在未填名称的模型")
		}
		key := strings.ToLower(mo.ID)
		if seen[key] {
			return fmt.Errorf("模型 %q 重复", mo.ID)
		}
		seen[key] = true
		mo.Type = normalizeType(mo.Type)
		mo.Strategy = normalizeStrategy(mo.Strategy)
		if len(mo.Channels) == 0 {
			return fmt.Errorf("模型 %q 至少需要一个渠道", mo.ID)
		}
		for i := range mo.Channels {
			ch := &mo.Channels[i]
			ch.Provider = strings.TrimSpace(ch.Provider)
			ch.Model = strings.TrimSpace(ch.Model)
			if ch.Provider == "" || ch.Model == "" {
				return fmt.Errorf("模型 %q 存在未选平台/模型的渠道", mo.ID)
			}
		}
	}
	sort.SliceStable(cfg.Models, func(i, j int) bool { return strings.ToLower(cfg.Models[i].ID) < strings.ToLower(cfg.Models[j].ID) })
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(m.file, raw); err != nil {
		return err
	}
	m.cfg = cfg
	return nil
}

// Lookup 按对外名称查渠道列表（大小写不敏感）。
func (m *Manager) Lookup(name string) (*Model, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mo := range m.cfg.Models {
		if strings.ToLower(mo.ID) == name {
			return mo, true
		}
	}
	return nil, false
}

// Empty 是否未配置任何发布模型（网关据此决定是否兜底暴露全部内部模型）。
func (m *Manager) Empty() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.cfg.Models) == 0
}

// PricingMap 对外模型名（小写）→ 单价。
func (m *Manager) PricingMap() map[string]Pricing {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]Pricing{}
	for _, mo := range m.cfg.Models {
		if mo.Pricing != nil && (mo.Pricing.Input > 0 || mo.Pricing.Output > 0 || mo.Pricing.PerCall > 0) {
			out[strings.ToLower(mo.ID)] = *mo.Pricing
		}
	}
	return out
}

// USDCNY 配置的汇率（未配置返回 0）。
func (m *Manager) USDCNY() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.USDCNY
}

func atomicWrite(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
