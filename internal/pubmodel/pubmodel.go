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
)

// Channel 一个上游渠道：某平台的一个真实模型。
type Channel struct {
	Provider string `json:"provider"` // workbuddy / qwen / trae / qoder / qoder-intl
	Model    string `json:"model"`    // 上游模型裸名（不带前缀）
	Note     string `json:"note,omitempty"`
}

// Pricing 单价（$/1M tokens）。
type Pricing struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Cache  float64 `json:"cache,omitempty"` // 缓存 token 单价（$/1M），0 = 缓存按输入价计
}

// Model 一个对外发布的模型。
type Model struct {
	ID       string    `json:"id"`                // 对外模型名（客户端 model 参数写这个）
	Note     string    `json:"note,omitempty"`    // 备注/显示名
	Pricing  *Pricing  `json:"pricing,omitempty"` // 单价；空 = 不计价
	Channels []Channel `json:"channels"`          // 上游渠道（按序失败转移，轮询起点轮转）
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

	// stats 渠道调用计数（运行期，重启清零）：key = lower(pubID)|provider|model
	statsMu sync.Mutex
	stats   map[string]*ChanStat
}

// ChanStat 渠道成功/失败计数。
type ChanStat struct {
	OK   int64 `json:"ok"`
	Fail int64 `json:"fail"`
}

func chanStatKey(pubID, provider, model string) string {
	return strings.ToLower(strings.TrimSpace(pubID)) + "|" + provider + "|" + model
}

// BumpChannel 渠道调用计数 +1。
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

// Load 从文件加载；文件不存在返回空配置。
func Load(file string) (*Manager, error) {
	m := &Manager{file: file, cfg: &Config{}}
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
		if mo.Pricing != nil && (mo.Pricing.Input > 0 || mo.Pricing.Output > 0) {
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
