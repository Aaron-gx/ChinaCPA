//go:build windows

package main

import (
	"fmt"
	"time"
)

// Metrics 把总览压成浮动栏要显示的圆环列表。
//
// 顺序固定，与面板能力区一致；只保留有额度的平台。
// 同名平台（Qoder 国内/国际）合并成一个环。
func Metrics(ov *Overview) []Metric {
	if ov == nil {
		return nil
	}

	type agg struct {
		used, limit, remaining float64
		unit                   string
		count                  int
		daily                  bool
		rows                   []MetricRow
		resetsAt               string
		quotaExceeded          bool
		unhealthy              bool
	}

	byProvider := map[string]*agg{}
	multiAccount := len(ov.Accounts) > 1

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
		if q.Detail.ResetsAt != "" && g.resetsAt == "" {
			g.resetsAt = q.Detail.ResetsAt
		}
		if q.Detail.QuotaExceeded {
			g.quotaExceeded = true
		}
		if !q.Healthy && q.Limit <= 0 && q.Remaining <= 0 {
			// 服务商说不可用，且没有任何额度数字
			g.unhealthy = true
		}

		// 每个账号贡献一行
		label := "每日配额"
		if !q.Daily {
			label = "配额"
		}
		if multiAccount {
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
		} else if q.Remaining > 0 {
			detail = "剩余 " + shortNum(q.Remaining)
		} else {
			detail = "无额度数据"
		}
		row := MetricRow{Label: label, UsedPct: pct, Detail: detail}
		row.Reset = resetTextFor(q)
		g.rows = append(g.rows, row)
		g.count++
	}

	order := []struct{ id, label, full string }{
		{"qwen", "Qwen", "Qwen 通义千问"},
		{"workbuddy", "WorkBuddy", "WorkBuddy / CodeBuddy"},
		{"trae", "Trae", "Trae"},
		{"qoder", "Qoder", "Qoder"},
		{"qoder-intl", "Qoder", "Qoder 国际"},
		{"qwenwork", "QwenWork", "QwenWork 千问办公"},
		{"doubao", "Doubao", "豆包"},
	}

	type group struct {
		label, full, provider string
		used, limit, remaining float64
		unit                  string
		rows                  []MetricRow
		count                 int
		resetsAt              string
		quotaExceeded         bool
		unhealthy             bool
		daily                 bool
	}

	var groups []*group
	byLabel := map[string]*group{}

	for _, o := range order {
		g := byProvider[o.id]
		if g == nil {
			continue
		}
		gr := byLabel[o.label]
		if gr == nil {
			gr = &group{label: o.label, full: o.full, provider: o.id, unit: g.unit}
			byLabel[o.label] = gr
			groups = append(groups, gr)
		}
		gr.used += g.used
		gr.limit += g.limit
		gr.remaining += g.remaining
		gr.rows = append(gr.rows, g.rows...)
		gr.count += g.count
		if g.unit != "" && gr.unit == "" {
			gr.unit = g.unit
		}
		if g.resetsAt != "" && gr.resetsAt == "" {
			gr.resetsAt = g.resetsAt
		}
		gr.quotaExceeded = gr.quotaExceeded || g.quotaExceeded
		gr.unhealthy = gr.unhealthy || g.unhealthy
		gr.daily = gr.daily || g.daily
	}

	now := time.Now()
	if ov.Now > 0 {
		now = time.Unix(ov.Now, 0)
	}

	var out []Metric
	for _, gr := range groups {
		m := Metric{
			Label:        gr.label,
			Full:         gr.full,
			Provider:     gr.provider,
			Unit:         gr.unit,
			Rows:         gr.rows,
			AccountCount: gr.count,
			SecondUsed:   -1, // 默认不画第二环
		}

		if gr.limit > 0 {
			m.Used = clamp01(gr.used / gr.limit)
			m.Text = shortNum(gr.used) + "/" + shortNum(gr.limit)
		} else if gr.remaining > 0 {
			m.Used = 1
			m.Balance = true
			m.Text = shortCompact(gr.remaining)
		} else {
			m.Empty = true
			m.Text = "0"
		}

		// 耗尽：服务商明说超限，或比例已满
		m.Spent = gr.quotaExceeded || m.Used >= 1

		// 窗口时钟弧：从 resets_at 反推这一轮窗口过去多少。
		//
		// 面板只给重置时刻、没给窗口起点，所以按常见窗口长度猜：
		// 每日配额用 24h，其它按 30 天（Qoder 的月度额度）。
		// 算不出来就不画 —— 硬编一个假的进度比不画更糟。
		m.ClockFrac = clockFraction(gr.resetsAt, gr.daily, now)

		out = append(out, m)
	}
	return out
}

// clockFraction 由重置时刻与窗口长度算"已过去比例"。
//
// resetsAt 的格式各平台不同（"12-31 08:00"、"2026-11-01T00:00:00Z"），
// 这里只认能被 parseReset 解析的形式；解析失败返回 -1（不画）。
func clockFraction(resetsAt string, daily bool, now time.Time) float64 {
	t, ok := parseReset(resetsAt, now)
	if !ok {
		return -1
	}
	window := 30 * 24 * time.Hour
	if daily {
		window = 24 * time.Hour
	}
	// 距重置还剩多久 → 窗口已过去 (window - left) / window
	left := t.Sub(now)
	if left < 0 {
		left = 0
	}
	if left > window {
		// 重置时刻远在窗口之外：说明窗口长度猜错了，不画
		return -1
	}
	frac := 1 - left.Seconds()/window.Seconds()
	return clamp01(frac)
}

// parseReset 解析重置时刻。支持 "01-02 15:04"（无年份，按最近的未来算）
// 与 RFC3339 两种。
func parseReset(s string, now time.Time) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	// "12-31 08:00"
	if t, err := time.ParseInLocation("01-02 15:04", s, now.Location()); err == nil {
		t = time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
		if t.Before(now) {
			t = t.AddDate(1, 0, 0)
		}
		return t, true
	}
	return time.Time{}, false
}

// resetTextFor 生成一行里显示的重置文案。
func resetTextFor(q *Quota) string {
	if q.Detail.ResetsAt != "" {
		return q.Detail.ResetsAt + " 重置"
	}
	return resetText(q.Unit, q.Daily)
}
