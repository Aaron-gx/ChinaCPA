// Package usage 用量统计：JSONL 日志（按天分文件）+ 内存聚合（今日/累计/按模型/按账号）
// + 计价（$/1M → ¥ 自动折算）。
package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record 单次成功调用的用量记录。
type Record struct {
	TS               int64  `json:"ts"` // Unix 秒
	Provider         string `json:"provider"`
	Account          string `json:"account"`
	Model            string `json:"model"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
}

// Tracker 用量记录器与聚合器。
type Tracker struct {
	dir string
	mu  sync.Mutex
	// 今日内存聚合
	today     string
	todayRecs map[string]*agg // key: provider|account|model
	// pricing
	pricingMu sync.Mutex
	pricing   *Pricing
	pricingAt string
}

type agg struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Prompt   int64  `json:"prompt_tokens"`
	Complete int64  `json:"completion_tokens"`
	Total    int64  `json:"total_tokens"`
}

// Pricing 模型单价表（$/1M tokens）。
type Pricing struct {
	USDCNY float64 `json:"usd_cny"`
	Models map[string]struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"models"`
}

// New 打开用量目录并回填今日数据（从今日日志文件）。
func New(dir string, pricingFile string) (*Tracker, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	t := &Tracker{
		dir:       dir,
		today:     time.Now().Format("2006-01-02"),
		todayRecs: map[string]*agg{},
	}
	t.loadPricing(pricingFile)
	t.restoreToday()
	return t, nil
}

func (t *Tracker) file(day string) string {
	return filepath.Join(t.dir, "usage-"+strings.ReplaceAll(day, "-", "")+".jsonl")
}

// Record 记录一次调用用量。
func (t *Tracker) Record(r Record) {
	if r.TS == 0 {
		r.TS = time.Now().Unix()
	}
	if r.TotalTokens == 0 {
		r.TotalTokens = r.PromptTokens + r.CompletionTokens
	}
	day := time.Unix(r.TS, 0).Format("2006-01-02")
	t.mu.Lock()
	defer t.mu.Unlock()
	// 跨日：重置内存聚合
	if day != t.today {
		t.today = day
		t.todayRecs = map[string]*agg{}
	}
	key := r.Provider + "|" + r.Account + "|" + r.Model
	a, ok := t.todayRecs[key]
	if !ok {
		a = &agg{Provider: r.Provider, Account: r.Account, Model: r.Model}
		t.todayRecs[key] = a
	}
	a.Requests++
	a.Prompt += r.PromptTokens
	a.Complete += r.CompletionTokens
	a.Total += r.TotalTokens
	// 追加落盘
	f, err := os.OpenFile(t.file(day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	raw, _ := json.Marshal(r)
	_, _ = f.Write(append(raw, '\n'))
}

// restoreToday 重启回填今日聚合。
func (t *Tracker) restoreToday() {
	f, err := os.Open(t.file(t.today))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		key := r.Provider + "|" + r.Account + "|" + r.Model
		a, ok := t.todayRecs[key]
		if !ok {
			a = &agg{Provider: r.Provider, Account: r.Account, Model: r.Model}
			t.todayRecs[key] = a
		}
		a.Requests++
		a.Prompt += r.PromptTokens
		a.Complete += r.CompletionTokens
		a.Total += r.TotalTokens
	}
}

// TodayAgg 今日聚合副本。
func (t *Tracker) TodayAgg() []agg {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]agg, 0, len(t.todayRecs))
	for _, a := range t.todayRecs {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out
}

// RequestsToday 今日某账号请求数（qwen 额度计数回填用）。
func (t *Tracker) RequestsToday(provider, uid string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var n int64
	for _, a := range t.todayRecs {
		if a.Provider == provider && a.Account == uid {
			n += a.Requests
		}
	}
	return n
}

// HistoryDays 最近 n 天（含今日）每日总量。
func (t *Tracker) HistoryDays(n int) []DayTotal {
	t.mu.Lock()
	today := t.today
	t.mu.Unlock()
	out := make([]DayTotal, 0, n)
	for i := n - 1; i >= 0; i-- {
		day := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		dt := DayTotal{Day: day}
		if day == today {
			for _, a := range t.TodayAgg() {
				dt.Requests += a.Requests
				dt.Prompt += a.Prompt
				dt.Complete += a.Complete
				dt.Total += a.Total
			}
		} else {
			dt = t.scanDay(day)
		}
		out = append(out, dt)
	}
	return out
}

// DayTotal 单日总量。
type DayTotal struct {
	Day      string `json:"day"`
	Requests int64  `json:"requests"`
	Prompt   int64  `json:"prompt_tokens"`
	Complete int64  `json:"completion_tokens"`
	Total    int64  `json:"total_tokens"`
}

func (t *Tracker) scanDay(day string) DayTotal {
	dt := DayTotal{Day: day}
	f, err := os.Open(t.file(day))
	if err != nil {
		return dt
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		dt.Requests++
		dt.Prompt += r.PromptTokens
		dt.Complete += r.CompletionTokens
		dt.Total += r.TotalTokens
	}
	return dt
}

// ── 计价 ─────────────────────────────────────────────────

// loadPricing 读取单价表（文件不存在时用内置常用模型缺省价）。
func (t *Tracker) loadPricing(path string) {
	t.pricingMu.Lock()
	defer t.pricingMu.Unlock()
	t.picingAtSet(path)
	t.pricing = defaultPricing()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var p Pricing
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	if p.USDCNY <= 0 {
		p.USDCNY = 7.1
	}
	if len(p.Models) > 0 {
		t.pricing = &p
	}
}

// picingAtSet 记录单价表路径（热更新监控用）。
func (t *Tracker) picingAtSet(path string) { t.pricingAt = path }

// PricingFile 单价表路径。
func (t *Tracker) PricingFile() string {
	t.pricingMu.Lock()
	defer t.pricingMu.Unlock()
	return t.pricingAt
}

// ReloadPricing 热加载单价表。
func (t *Tracker) ReloadPricing() {
	t.loadPricing(t.PricingFile())
}

// defaultPricing 内置常用国产模型参考价（$/1M tokens，社区公开牌价量级）。
// 仅用于仪表板费用估算，非计费依据；可在 data/pricing.json 覆盖。
func defaultPricing() *Pricing {
	type m = struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	}
	return &Pricing{
		USDCNY: 7.1,
		Models: map[string]struct {
			Input  float64 `json:"input"`
			Output float64 `json:"output"`
		}{
			"qwen3-coder-plus":  {Input: 0.002, Output: 0.006},
			"qwen3-coder-flash": {Input: 0.0005, Output: 0.002},
			"qwen3-max":         {Input: 0.0018, Output: 0.0072},
			"qwen3-plus":        {Input: 0.0004, Output: 0.0012},
			"glm-5.2":           {Input: 0.002, Output: 0.008},
			"glm-5.3":           {Input: 0.002, Output: 0.008},
			"deepseek-v4.1":     {Input: 0.002, Output: 0.008},
			"kimi-k3":           {Input: 0.002, Output: 0.008},
		},
	}
}

// Cost 估算一段用量的费用（人民币元）。
func (t *Tracker) Cost(model string, prompt, completion int64) float64 {
	t.pricingMu.Lock()
	p := t.pricing
	t.pricingMu.Unlock()
	if p == nil {
		return 0
	}
	mp, ok := p.Models[normalizeModel(model)]
	if !ok {
		return 0
	}
	usd := float64(prompt)/1e6*mp.Input + float64(completion)/1e6*mp.Output
	return usd * p.USDCNY
}

// USDCNY 当前汇率。
func (t *Tracker) USDCNY() float64 {
	t.pricingMu.Lock()
	defer t.pricingMu.Unlock()
	if t.pricing == nil {
		return 7.1
	}
	return t.pricing.USDCNY
}

// normalizeModel 去掉可能的 provider 前缀与别名后缀，匹配单价表键。
func normalizeModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.Index(m, ":"); i > 0 {
		m = m[i+1:]
	}
	return m
}

// AggWithCost 聚合条目 + 估算费用。
type AggWithCost struct {
	agg
	CostCNY float64 `json:"cost_cny"`
}

// TodayWithCost 今日聚合（含费用）。
func (t *Tracker) TodayWithCost() []AggWithCost {
	base := t.TodayAgg()
	out := make([]AggWithCost, 0, len(base))
	for _, a := range base {
		out = append(out, AggWithCost{agg: a, CostCNY: t.Cost(a.Model, a.Prompt, a.Complete)})
	}
	return out
}

// Totals 汇总。
type Totals struct {
	Requests int64   `json:"requests"`
	Prompt   int64   `json:"prompt_tokens"`
	Complete int64   `json:"completion_tokens"`
	Total    int64   `json:"total_tokens"`
	CostCNY  float64 `json:"cost_cny"`
}

// Sum 汇总聚合条目。
func Sum(items []AggWithCost) Totals {
	var tt Totals
	for _, it := range items {
		tt.Requests += it.Requests
		tt.Prompt += it.Prompt
		tt.Complete += it.Complete
		tt.Total += it.Total
		tt.CostCNY += it.CostCNY
	}
	return tt
}
