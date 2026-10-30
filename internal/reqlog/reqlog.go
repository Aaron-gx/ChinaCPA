// Package reqlog 请求日志：bbolt 单文件本地数据库（data/requests.db）。
// 按「时间键」存储，天然支持任意时间范围查询（今天/昨天/7天/更久），
// 服务重启不丢数据，另提供按保留天数清理。
package reqlog

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Entry 一次网关请求的日志。
type Entry struct {
	TS               int64    `json:"ts"` // Unix 秒
	ID               string   `json:"id"`
	Protocol         string   `json:"protocol"` // chat / responses / messages
	Model            string   `json:"model"`    // 对外请求模型名
	Provider         string   `json:"provider,omitempty"`
	Account          string   `json:"account,omitempty"`
	Stream           bool     `json:"stream"`
	PromptTokens     int64    `json:"prompt_tokens,omitempty"`
	CompletionTokens int64    `json:"completion_tokens,omitempty"`
	TotalTokens      int64    `json:"total_tokens,omitempty"`
	CacheTokens      int64    `json:"cache_tokens,omitempty"`
	CostCNY          float64  `json:"cost_cny,omitempty"`
	FirstMS          int64    `json:"first_ms,omitempty"` // 首字延迟（毫秒）
	TotalMS          int64    `json:"total_ms,omitempty"` // 总耗时（毫秒）
	OK               bool     `json:"ok"`
	Error            string   `json:"error,omitempty"`
	Attempts         []string `json:"attempts,omitempty"`       // 渠道尝试链路，如 qoder/dfmodel ✗
	ReportedModel    string   `json:"reported_model,omitempty"` // 上游自报模型名（与请求名比对）
	Finish           string   `json:"finish,omitempty"`         // 结束原因：stop / length（截断）/ tool_calls…
	Client           string   `json:"client,omitempty"`         // 请求来源客户端类型（zcode/claude-code/codex/openai-sdk/curl/其他）
}

// Stats 查询范围的聚合结果（含分布，供面板环形图直接使用）。
type Stats struct {
	Total    int64   `json:"total"`
	OK       int64   `json:"ok"`
	Fail     int64   `json:"fail"`
	Prompt   int64   `json:"prompt_tokens"`
	Complete int64   `json:"completion_tokens"`
	Tokens   int64   `json:"total_tokens"`
	Cache    int64   `json:"cache_tokens"`
	CostCNY  float64 `json:"cost_cny"`
	AvgMS    float64 `json:"avg_ms"`
	AvgFirst float64 `json:"avg_first_ms"`
	// 耗时分位数（仅统计成功请求，失败请求的超时/快速失败会扭曲体验指标）
	P50MS    float64 `json:"p50_ms"`
	P90MS    float64 `json:"p90_ms"`
	P99MS    float64 `json:"p99_ms"`
	MaxMS    int64   `json:"max_ms"`
	P50First float64 `json:"p50_first_ms"`
	P90First float64 `json:"p90_first_ms"`
	// LatencyByModel 按模型的成功请求耗时统计（面板耗时分布表）
	LatencyByModel []ModelLatency `json:"latency_by_model,omitempty"`
	// GapByClient 客户端请求间隔（工具/思考时间的近似），按样本量降序
	GapByClient []ClientGap `json:"gap_by_client,omitempty"`

	ByModel  []Dist `json:"by_model"`
	ByLine   []Dist `json:"by_line"`
	ByStatus []Dist `json:"by_status"`
	ByClient []Dist `json:"by_client,omitempty"` // 按请求来源客户端聚合
	// CostByModel 每模型的 token 用量（面板层用当前价格现算成本，见 CostModel）
	CostByModel []ModelCost `json:"cost_by_model,omitempty"`
	// UsageRows 按「模型×渠道×实例」三维聚合的用量明细（面板用量表跨日期查询用；
	// 当日内存聚合重启即失，历史范围从持久化日志现算）。
	UsageRows []UsageRow `json:"usage_rows,omitempty"`

	// 分位数样本（聚合期暂存，finalize 后释放，不参与序列化）
	dursAll    []int64
	firstsAll  []int64
	dursByMdl  map[string][]int64
	genByMdl   map[string][]int64 // 生成阶段耗时（总耗时-首字），仅流式成功
	firstByMdl map[string][]int64 // 首字延迟
	prevByCli  map[string][]int64 // 同客户端相邻请求间隔样本
	usageAgg   map[string]*UsageRow
}

// UsageRow 模型×渠道×实例 的用量聚合行。
type UsageRow struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Requests int64  `json:"requests"`
	Prompt   int64  `json:"prompt_tokens"`
	Complete int64  `json:"completion_tokens"`
	Total    int64  `json:"total_tokens"`
	Cache    int64  `json:"cache_tokens"`
	Fail     int64  `json:"fail"`
}

// ModelLatency 单模型成功请求耗时统计。
type ModelLatency struct {
	Model string  `json:"model"`
	Count int64   `json:"count"`
	AvgMS float64 `json:"avg_ms"`
	P50MS float64 `json:"p50_ms"`
	P90MS float64 `json:"p90_ms"`
	P99MS float64 `json:"p99_ms"`
	// 阶段拆分（仅流式且有首字时间的请求）：FirstP50MS ≈ 思考+排队，GenP50/90MS = 流式生成
	FirstP50MS float64 `json:"first_p50_ms"`
	GenP50MS   float64 `json:"gen_p50_ms"`
	GenP90MS   float64 `json:"gen_p90_ms"`
}

// ClientGap 同客户端相邻请求的间隔统计（≈ 工具执行 + 用户思考的本地时间）。
type ClientGap struct {
	Client  string  `json:"client"`
	Samples int64   `json:"samples"`
	P50MS   float64 `json:"p50_ms"`
	P90MS   float64 `json:"p90_ms"`
}

// ModelCost 单模型 token 用量（供成本重算）。
type ModelCost struct {
	Model    string `json:"model"`
	Prompt   int64  `json:"prompt"`
	Complete int64  `json:"complete"`
	Cache    int64  `json:"cache"`
}

// Dist 分布条目。
type Dist struct {
	Label string `json:"label"`
	Value int64  `json:"value"`
}

// QueryOpts 查询条件。From/To 为 Unix 秒（含头含尾）；Status: ""=全部 "ok"=成功 "fail"=失败。
type QueryOpts struct {
	From   int64
	To     int64
	Model  string
	Status string
	Limit  int
	Offset int
}

// DB bbolt 请求日志库。
type DB struct {
	db      *bolt.DB
	bucket  []byte
	lastKey uint64 // 保证键严格递增
}

var bucketName = []byte("requests")

// Open 打开（不存在则创建）数据库，并迁移同目录下的历史 requests-*.jsonl。
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "requests.db")
	db, err := bolt.Open(path, 0o644, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open request db: %w", err)
	}
	d := &DB{db: db, bucket: bucketName}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	d.migrateJSONL(dir)
	return d, nil
}

// Close 关闭数据库。
func (d *DB) Close() error { return d.db.Close() }

// keyNano 生成严格递增的 8 字节时间键（UnixNano）。
func (d *DB) keyNano(tsSec int64) uint64 {
	n := uint64(time.Now().UnixNano())
	if tsSec > 0 {
		// 迁移历史数据：用事件时间，避免与当前时钟冲突（历史键必然小于当前键）
		n = uint64(tsSec) * 1e9
	}
	for {
		last := atomic.LoadUint64(&d.lastKey)
		if n > last {
			if atomic.CompareAndSwapUint64(&d.lastKey, last, n) {
				return n
			}
			continue
		}
		n = last + 1
	}
}

// Add 追加一条日志。
func (d *DB) Add(e Entry) {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	if e.ID == "" {
		e.ID = NewID()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	key := make([]byte, 8)
	// 键必须用纳秒时钟（传 0 = 走 time.Now().UnixNano）：
	// 若用 e.TS（秒级），同一秒内的多条请求键相同，bbolt Put 会相互覆盖丢日志
	binary.BigEndian.PutUint64(key, d.keyNano(0))
	_ = d.db.Batch(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Put(key, raw)
	})
}

// Query 按条件查询：返回一页日志（新→旧）+ 全范围聚合统计。
func (d *DB) Query(q QueryOpts) ([]Entry, *Stats, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	if q.To <= 0 {
		q.To = time.Now().Unix()
	}
	stats := &Stats{}
	var page []Entry
	var skipped int
	model := strings.ToLower(strings.TrimSpace(q.Model))

	err := d.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketName).Cursor()
		// 从 To 的下一纳秒位置向前遍历；若该位置之后无数据则从最新开始
		seek := make([]byte, 8)
		binary.BigEndian.PutUint64(seek, uint64(q.To)*1e9+1)
		k, v := c.Seek(seek)
		if k == nil {
			k, v = c.Last()
		}
		for ; k != nil; k, v = c.Prev() {
			ts := int64(binary.BigEndian.Uint64(k) / 1e9)
			if ts > q.To {
				continue // Seek 落点可能越过上界（查询范围整体早于最新数据）
			}
			if ts < q.From {
				break
			}
			var e Entry
			if json.Unmarshal(v, &e) != nil {
				continue
			}
			if model != "" && !strings.Contains(strings.ToLower(e.Model), model) {
				continue
			}
			if q.Status == "ok" && !e.OK {
				continue
			}
			if q.Status == "fail" && e.OK {
				continue
			}
			aggregate(stats, &e)
			if len(page) < q.Limit && skipped >= q.Offset {
				page = append(page, e)
			} else if skipped < q.Offset {
				skipped++
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	finalize(stats)
	return page, stats, nil
}

// incCostModel 按模型累积 token（供面板层用当前价格重算成本）。
func incCostModel(arr *[]ModelCost, model string, prompt, complete, cache int64) {
	for i := range *arr {
		if (*arr)[i].Model == model {
			(*arr)[i].Prompt += prompt
			(*arr)[i].Complete += complete
			(*arr)[i].Cache += cache
			return
		}
	}
	*arr = append(*arr, ModelCost{Model: model, Prompt: prompt, Complete: complete, Cache: cache})
}

// aggregate 累加一条到统计（finalize 前用临时和）。
func aggregate(s *Stats, e *Entry) {
	s.Total++
	if e.OK {
		s.OK++
	} else {
		s.Fail++
	}
	s.Prompt += e.PromptTokens
	s.Complete += e.CompletionTokens
	s.Tokens += e.TotalTokens
	s.Cache += e.CacheTokens
	// 成本不在写入时固化：改价后历史统计需用当前价格重算（见 agg.costByModel 面板层现算）
	incCostModel(&s.CostByModel, e.Model, e.PromptTokens, e.CompletionTokens, e.CacheTokens)
	// 模型×渠道×实例 三维聚合（与内存当日聚合同构，供历史范围明细表）
	uk := e.Model + "\x00" + e.Provider + "\x00" + e.Account
	if s.usageAgg == nil {
		s.usageAgg = map[string]*UsageRow{}
	}
	ur := s.usageAgg[uk]
	if ur == nil {
		ur = &UsageRow{Model: e.Model, Provider: e.Provider, Account: e.Account}
		s.usageAgg[uk] = ur
	}
	ur.Requests++
	ur.Prompt += e.PromptTokens
	ur.Complete += e.CompletionTokens
	ur.Total += e.TotalTokens
	ur.Cache += e.CacheTokens
	if !e.OK {
		ur.Fail++
	}
	if e.TotalMS > 0 {
		s.AvgMS += float64(e.TotalMS)
	}
	if e.OK && e.FirstMS > 0 {
		s.AvgFirst += float64(e.FirstMS)
	}
	// 分位数样本：仅成功请求的完整耗时与首字耗时
	if e.OK && e.TotalMS > 0 {
		s.dursAll = append(s.dursAll, e.TotalMS)
		if e.TotalMS > s.MaxMS {
			s.MaxMS = e.TotalMS
		}
		if s.dursByMdl == nil {
			s.dursByMdl = map[string][]int64{}
		}
		mk := e.Model
		if mk == "" {
			mk = "未知"
		}
		s.dursByMdl[mk] = append(s.dursByMdl[mk], e.TotalMS)
		// 阶段拆分：流式且有首字时间 → 首字（思考+排队）与生成（流式输出）两个样本
		if e.Stream && e.FirstMS > 0 && e.TotalMS > e.FirstMS {
			if s.genByMdl == nil {
				s.genByMdl = map[string][]int64{}
				s.firstByMdl = map[string][]int64{}
			}
			s.genByMdl[mk] = append(s.genByMdl[mk], e.TotalMS-e.FirstMS)
			s.firstByMdl[mk] = append(s.firstByMdl[mk], e.FirstMS)
		}
	}
	if e.OK && e.FirstMS > 0 {
		s.firstsAll = append(s.firstsAll, e.FirstMS)
	}
	// 客户端请求间隔（≈ 工具执行 + 用户思考）：Query 按时间倒序遍历，
	// prevByCli 存上一条（更新的）的 [完成ts, 耗时ms, 样本...]。
	// ts 是请求完成时刻，故 工具时间 = 完成(N+1) - 完成(N) - 耗时(N+1)。
	// 限幅 1s~30min：更小多为并发批量，更大是人离开电脑
	{
		ck := e.Client
		if ck == "" {
			ck = "未知"
		}
		if s.prevByCli == nil {
			s.prevByCli = map[string][]int64{}
		}
		arr := s.prevByCli[ck]
		if len(arr) == 0 {
			arr = make([]int64, 2, 64) // [0]=上一条完成ts [1]=上一条耗时ms
		}
		if arr[0] > e.TS {
			gap := (arr[0]-e.TS)*1000 - arr[1]
			if gap >= 1000 && gap <= 30*60*1000 {
				arr = append(arr, gap)
			}
		}
		if len(arr) > 402 {
			arr = arr[:402] // 每客户端样本封顶，控制内存
		}
		arr[0], arr[1] = e.TS, e.TotalMS
		s.prevByCli[ck] = arr
	}
	inc := func(d *[]Dist, label string) {
		for i := range *d {
			if (*d)[i].Label == label {
				(*d)[i].Value++
				return
			}
		}
		*d = append(*d, Dist{Label: label, Value: 1})
	}
	inc(&s.ByModel, e.Model)
	if e.Provider != "" {
		inc(&s.ByLine, e.Provider)
	} else {
		inc(&s.ByLine, "失败")
	}
	if e.OK {
		inc(&s.ByStatus, "成功")
	} else {
		inc(&s.ByStatus, "失败")
	}
	client := e.Client
	if client == "" {
		client = "未知"
	}
	inc(&s.ByClient, client)
}

// percentile 对升序样本取分位（线性插值，样本空返回 0）。
func percentile(sorted []int64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return float64(sorted[0])
	}
	idx := p / 100 * float64(n-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= n {
		return float64(sorted[n-1])
	}
	w := idx - float64(lo)
	return float64(sorted[lo])*(1-w) + float64(sorted[hi])*w
}

// finalize 计算均值/分位数并按量排序分布。
func finalize(s *Stats) {
	if s.Total > 0 {
		s.AvgMS /= float64(s.Total)
	}
	if s.OK > 0 {
		s.AvgFirst /= float64(s.OK)
	}
	// 耗时分位数
	sort.Slice(s.dursAll, func(i, j int) bool { return s.dursAll[i] < s.dursAll[j] })
	s.P50MS = percentile(s.dursAll, 50)
	s.P90MS = percentile(s.dursAll, 90)
	s.P99MS = percentile(s.dursAll, 99)
	sort.Slice(s.firstsAll, func(i, j int) bool { return s.firstsAll[i] < s.firstsAll[j] })
	s.P50First = percentile(s.firstsAll, 50)
	s.P90First = percentile(s.firstsAll, 90)
	// 按模型耗时：请求量降序，最多 12 个
	type mlat struct {
		m    string
		durs []int64
	}
	var lats []mlat
	for m, durs := range s.dursByMdl {
		lats = append(lats, mlat{m, durs})
	}
	sort.Slice(lats, func(i, j int) bool { return len(lats[i].durs) > len(lats[j].durs) })
	if len(lats) > 12 {
		lats = lats[:12]
	}
	for _, x := range lats {
		sort.Slice(x.durs, func(i, j int) bool { return x.durs[i] < x.durs[j] })
		sum := int64(0)
		for _, d := range x.durs {
			sum += d
		}
		ml := ModelLatency{
			Model: x.m, Count: int64(len(x.durs)),
			AvgMS: float64(sum) / float64(len(x.durs)),
			P50MS: percentile(x.durs, 50), P90MS: percentile(x.durs, 90), P99MS: percentile(x.durs, 99),
		}
		if g := s.genByMdl[x.m]; len(g) >= 5 { // 样本太少不出阶段值
			sort.Slice(g, func(i, j int) bool { return g[i] < g[j] })
			ml.GenP50MS = percentile(g, 50)
			ml.GenP90MS = percentile(g, 90)
		}
		if f := s.firstByMdl[x.m]; len(f) >= 5 {
			sort.Slice(f, func(i, j int) bool { return f[i] < f[j] })
			ml.FirstP50MS = percentile(f, 50)
		}
		s.LatencyByModel = append(s.LatencyByModel, ml)
	}
	// 客户端请求间隔（工具/思考时间近似）
	for ck, arr := range s.prevByCli {
		gaps := arr[2:] // [0]=完成ts [1]=耗时ms，之后才是间隔样本
		if len(gaps) < 5 {
			continue
		}
		sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
		s.GapByClient = append(s.GapByClient, ClientGap{
			Client: ck, Samples: int64(len(gaps)),
			P50MS: percentile(gaps, 50), P90MS: percentile(gaps, 90),
		})
	}
	sort.Slice(s.GapByClient, func(i, j int) bool { return s.GapByClient[i].Samples > s.GapByClient[j].Samples })
	// 用量明细行：按请求数降序，最多 60 行（面板表容量）
	for _, ur := range s.usageAgg {
		s.UsageRows = append(s.UsageRows, *ur)
	}
	sort.Slice(s.UsageRows, func(i, j int) bool { return s.UsageRows[i].Requests > s.UsageRows[j].Requests })
	if len(s.UsageRows) > 60 {
		s.UsageRows = s.UsageRows[:60]
	}
	// 释放样本内存
	s.dursAll, s.firstsAll, s.dursByMdl = nil, nil, nil
	s.genByMdl, s.firstByMdl, s.prevByCli, s.usageAgg = nil, nil, nil, nil
	sortDist := func(d []Dist) {
		sort.Slice(d, func(i, j int) bool { return d[i].Value > d[j].Value })
	}
	sortDist(s.ByModel)
	sortDist(s.ByLine)
	sortDist(s.ByStatus)
	sortDist(s.ByClient)
}

// DeleteBefore 删除指定时刻之前的日志，返回删除条数。
func (d *DB) DeleteBefore(cutoffSec int64) (int, error) {
	removed := 0
	err := d.db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketName).Cursor()
		for k, _ := c.First(); k != nil && int64(binary.BigEndian.Uint64(k)/1e9) < cutoffSec; k, _ = c.First() {
			if err := c.Delete(); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}

// migrateJSONL 一次性导入历史 requests-*.jsonl（仅当库为空时）。
func (d *DB) migrateJSONL(dir string) {
	var total int
	_ = d.db.View(func(tx *bolt.Tx) error {
		total = tx.Bucket(bucketName).Stats().KeyN
		return nil
	})
	if total > 0 {
		return
	}
	files, _ := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	if len(files) == 0 {
		return
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range bytes.Split(raw, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var e Entry
			if json.Unmarshal(line, &e) != nil || e.ID == "" {
				continue
			}
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, d.keyNano(e.TS))
			kk := append([]byte(nil), key...)
			ee := e
			_ = d.db.Batch(func(tx *bolt.Tx) error {
				return tx.Bucket(bucketName).Put(kk, mustJSON(ee))
			})
		}
	}
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// NewID 生成请求 ID：秒级时间戳-随机 hex。
func NewID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d-%s", time.Now().Unix(), hex.EncodeToString(b))
}

// Attempt 归一化渠道尝试描述。
func Attempt(provider, model string, ok bool, note string) string {
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	s := provider + "/" + model + " " + mark
	if note != "" {
		s += " " + strings.TrimSpace(note)
	}
	return s
}
