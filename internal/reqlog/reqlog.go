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

	ByModel  []Dist `json:"by_model"`
	ByLine   []Dist `json:"by_line"`
	ByStatus []Dist `json:"by_status"`
	ByClient []Dist `json:"by_client,omitempty"` // 按请求来源客户端聚合
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
	binary.BigEndian.PutUint64(key, d.keyNano(e.TS))
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
	s.CostCNY += e.CostCNY
	if e.TotalMS > 0 {
		s.AvgMS += float64(e.TotalMS)
	}
	if e.OK && e.FirstMS > 0 {
		s.AvgFirst += float64(e.FirstMS)
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

// finalize 计算均值并按量排序分布。
func finalize(s *Stats) {
	if s.Total > 0 {
		s.AvgMS /= float64(s.Total)
	}
	if s.OK > 0 {
		s.AvgFirst /= float64(s.OK)
	}
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
