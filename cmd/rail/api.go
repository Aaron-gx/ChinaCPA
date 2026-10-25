// Command rail 屏幕边缘常驻的额度浮动栏。
//
// 它不内嵌进 chinacpa 服务，而是一个独立的桌面进程：只读面板 API，
// 不写任何数据。这样主服务可以无 UI 运行（服务器/容器），浮动栏只在
// 装了桌面的机器上按需启动。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Options 浮动栏的运行参数。
type Options struct {
	BaseURL  string  // 面板地址，如 http://127.0.0.1:8313
	Password string  // 面板密码
	Interval int     // 轮询秒数
	Tint     string  // 颜色主题 accent / green / teal
	AlwaysOnTop bool // 是否置顶
}

// Quota 单个账号的额度快照（对应 panel 的 quota 字段）。
type Quota struct {
	Used      float64 `json:"used"`
	Limit     float64 `json:"limit"`
	Remaining float64 `json:"remaining"`
	Unit      string  `json:"unit"`
	Daily     bool    `json:"daily"`
	Credits   int64   `json:"credits,omitempty"`
}

// Account 账号视图（只取浮动栏需要的字段）。
type Account struct {
	Provider     string `json:"provider"`
	ProviderName string `json:"provider_name"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	Status       string `json:"status"`
	Quota        *Quota `json:"quota"`
}

// Overview 面板总览的精简投影。
type Overview struct {
	Accounts       []Account `json:"accounts"`
	AccountTotal   int       `json:"account_total"`
	ProviderCount  map[string]int `json:"provider_count"`
	TotalCredits   float64   `json:"total_credits"`
	TotalIdeCredits float64  `json:"total_ide_credits"`
	QwenRemaining  float64   `json:"qwen_remaining"`
	QoderRemaining float64   `json:"qoder_remaining"`
	Today          struct {
		Requests   int64   `json:"requests"`
		TotalTokens int64  `json:"total_tokens"`
		CostCNY    float64 `json:"cost_cny"`
	} `json:"today"`
}

// Client 面板 API 客户端。复用 cookie 会话，避免每轮都重新登录。
type Client struct {
	base  string
	pass  string
	http  *http.Client
	ready bool
}

// NewClient 构造客户端。
func NewClient(base, password string, timeout time.Duration) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	base = strings.TrimSuffix(base, "/")
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("面板地址无效: %w", err)
	}
	return &Client{
		base: base,
		pass: password,
		http: &http.Client{Jar: jar, Timeout: timeout},
	}, nil
}

// login 建立面板会话。面板用无状态 cookie，重启服务后旧 cookie 仍有效。
func (c *Client) login() error {
	body := strings.NewReader(fmt.Sprintf(`{"password":%q}`, c.pass))
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/auth", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("连接面板失败: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("面板密码错误")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("登录返回 %d", resp.StatusCode)
	}
	c.ready = true
	return nil
}

// Fetch 拉取总览。会话失效时自动重登一次。
func (c *Client) Fetch() (*Overview, error) {
	if !c.ready {
		if err := c.login(); err != nil {
			return nil, err
		}
	}
	ov, err := c.fetchOnce()
	if err == nil {
		return ov, nil
	}
	// cookie 可能因改密码等原因失效，重登一次再试
	c.ready = false
	if lerr := c.login(); lerr != nil {
		return nil, lerr
	}
	return c.fetchOnce()
}

func (c *Client) fetchOnce() (*Overview, error) {
	resp, err := c.http.Get(c.base + "/api/overview")
	if err != nil {
		return nil, fmt.Errorf("请求总览失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("会话失效")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("总览返回 %d", resp.StatusCode)
	}
	var ov Overview
	if err := json.NewDecoder(resp.Body).Decode(&ov); err != nil {
		return nil, fmt.Errorf("解析总览失败: %w", err)
	}
	return &ov, nil
}

// Metric 浮动栏上的一个圆环。
type Metric struct {
	Label    string  // 详情卡标题用的平台名
	Full     string  // 平台全名
	Provider string  // 平台 id，用于选 logo
	Used     float64 // 已用比例 0~1（决定圆盘占满程度与颜色）
	Text     string  // 已用/上限，如 "7.70K/9.03K"
	Unit     string  // 单位
	Empty    bool    // 无数据
	// Balance 是纯余额（无固定上限）：不参与用量告警配色。
	Balance bool
	// 详情卡里的行：每条用量窗口
	Rows []MetricRow
	// 账号数（同平台可能有多个账号）
	AccountCount int
}

// MetricRow 详情卡里的一行用量。
type MetricRow struct {
	Label   string  // 如「每日配额」
	UsedPct float64 // 已用百分比 0~100
	Reset   string  // 重置时间文案，如「16:00 重置」，空则不显示
	Detail  string  // 补充说明（已用/上限）
}

// Metrics 把总览压成浮动栏要显示的圆环列表。
//
// 顺序固定为 Qwen / WorkBuddy / Trae / Qoder，与面板能力区一致；
// 只保留有额度的平台，一个都没有时返回空（界面显示未连接）。
func Metrics(ov *Overview) []Metric {
	if ov == nil {
		return nil
	}
	// 按平台聚合成一个环
	type agg struct {
		used, limit, remaining float64
		unit                   string
		count                  int
		daily                  bool
		rows                   []MetricRow
	}
	byProvider := map[string]*agg{}
	for _, a := range ov.Accounts {
		if a.Quota == nil || a.Status == "disabled" {
			continue
		}
		g := byProvider[a.Provider]
		if g == nil {
			g = &agg{unit: a.Quota.Unit}
			byProvider[a.Provider] = g
		}
		q := a.Quota
		g.used += q.Used
		g.limit += q.Limit
		g.remaining += q.Remaining
		g.daily = g.daily || q.Daily
		if q.Unit != "" {
			g.unit = q.Unit
		}
		// 每个账号贡献一行（多账号时标注昵称）
		label := "每日配额"
		if !q.Daily {
			label = "配额"
		}
		if len(ov.Accounts) > 1 {
			name := a.Nickname
			if name == "" {
				name = a.UID
				if len(name) > 8 {
					name = name[:8]
				}
			}
			label = name
		}
		var pct float64
		detail := ""
		if q.Limit > 0 {
			pct = q.Used / q.Limit * 100
			detail = fmt.Sprintf("已用 %s / %s", shortNum(q.Used), shortNum(q.Limit))
		} else {
			pct = 0
			detail = "剩余 " + shortNum(q.Remaining)
		}
		row := MetricRow{Label: label, UsedPct: pct, Detail: detail}
		row.Reset = resetText(q.Unit, q.Daily)
		g.rows = append(g.rows, row)
		g.count++
	}

	order := []struct{ id, label, full string }{
		{"qwen", "Qwen", "Qwen 通义千问"},
		{"workbuddy", "WorkBuddy", "WorkBuddy / CodeBuddy"},
		{"trae", "Trae", "Trae"},
		{"qoder", "Qoder", "Qoder"},
		{"qoder-intl", "Qoder", "Qoder 国际"},
	}
	var out []Metric
	seen := map[string]bool{}
	for _, o := range order {
		g := byProvider[o.id]
		if g == nil {
			continue
		}
		short := o.label
		if seen[short] {
			continue
		}
		seen[short] = true

		m := Metric{
			Label:        short,
			Full:         o.full,
			Provider:     o.id,
			Unit:         g.unit,
			Rows:         g.rows,
			AccountCount: g.count,
		}
		if g.limit > 0 {
			m.Used = clamp01(g.used / g.limit)
			m.Text = shortNum(g.used) + "/" + shortNum(g.limit)
		} else if g.remaining > 0 {
			m.Used = 1
			m.Balance = true
			// 圆盘下方只有 46px 宽，用最紧凑写法
			m.Text = shortCompact(g.remaining)
		} else {
			m.Empty = true
			m.Text = "0"
		}
		out = append(out, m)
	}
	return out
}

// resetText 生成重置时间文案。
//
// 面板目前不提供精确重置时刻，只能按单位给出可预期的时间点：
// 每日配额 → 次日 0 点；积分类 → 不显示（上游按账单周期重置，未知即不说）。
func resetText(unit string, daily bool) string {
	if !daily && unit != "requests/day" {
		return ""
	}
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	return next.Format("01/02 15:04") + " 重置"
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// shortNum 人读数字：1.2K / 3.8M 风格。
func shortNum(v float64) string {
	a := v
	if a < 0 {
		a = -a
	}
	switch {
	case a >= 1e9:
		return trimZero(fmt.Sprintf("%.1fB", v/1e9))
	case a >= 1e6:
		return trimZero(fmt.Sprintf("%.1fM", v/1e6))
	case a >= 1e4:
		return trimZero(fmt.Sprintf("%.1fK", v/1e3))
	case a >= 1e3:
		return trimZero(fmt.Sprintf("%.2fK", v/1e3))
	default:
		return trimZero(fmt.Sprintf("%.0f", v))
	}
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

// shortCompact 极简数字：圆盘下方窄条里用，控制在 5 字符内。
func shortCompact(v float64) string {
	a := v
	if a < 0 {
		a = -a
	}
	switch {
	case a >= 1e6:
		return trimZero(fmt.Sprintf("%.1fM", v/1e6))
	case a >= 1e4:
		return trimZero(fmt.Sprintf("%.0fK", v/1e3))
	case a >= 1e3:
		return trimZero(fmt.Sprintf("%.1fK", v/1e3))
	default:
		return trimZero(fmt.Sprintf("%.0f", v))
	}
}

// sortedProviders 调试用：列出出现过的平台名。
func sortedProviders(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
