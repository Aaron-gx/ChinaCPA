// quota.go 豆包生成额度：按账号 × 类别（图片/视频/对话）本地日计数。
// 上游网页端无公开额度查询接口，采用与 Qwen 相同的本地计数方案：
//   - 计数点：提交成功（图片按出图张数、视频按提交条数、对话按次）
//   - 达到每日上限 → 返回 ErrQuota，账号池自动冷却该账号并切换下一个
//   - Quota() 上报明细（含按模型分布），面板「生成额度」视图展示
//
// 计数为进程内存态：跨日自动清零，服务重启清零。
package doubao

import (
	"fmt"
	"sync"
	"time"

	"chinacpa/internal/provider"
)

// dbDayCounter 单账号当日计数。
type dbDayCounter struct {
	day     string
	chats   int64
	images  int64            // 张
	videos  int64            // 条
	byModel map[string]int64 // 模型 -> 次数（图片按张/视频按条统一记）
}

// dbUsageSnapshot 额度快照（保留字段定义供后续持久化/恢复使用）。
type dbUsageSnapshot struct {
	Chats       int64            `json:"chats_today"`
	ImagesUsed  int64            `json:"images_used_today"`
	ImagesLimit int64            `json:"images_limit"`
	VideosUsed  int64            `json:"videos_used_today"`
	VideosLimit int64            `json:"videos_limit"`
	ByModel     map[string]int64 `json:"by_model"`
}

// imagesLeft / videosLeft 剩余额度。
func (s *dbUsageSnapshot) imagesLeft() int64 { return s.ImagesLimit - s.ImagesUsed }
func (s *dbUsageSnapshot) videosLeft() int64 { return s.VideosLimit - s.VideosUsed }

// 额度计数器（进程级）。
var (
	qmu       sync.Mutex
	qcounters = map[string]*dbDayCounter{}

	// persistSave / persistRestore 跨重启持久化钩子（main 接线 store）。
	// save 在计数变更后持锁内调用（实现只读快照）；restore 用于启动时恢复今日计数。
	persistSave    func(uid string, c CounterSnapshot)
	persistRestore func(uid string) (CounterSnapshot, bool)
)

// CounterSnapshot 跨重启持久化的计数器快照（可安全跨包传递）。
type CounterSnapshot struct {
	Day     string
	Chats   int64
	Images  int64
	Videos  int64
	ByModel map[string]int64
}

// SetCounterPersistence 注入持久化钩子（nil = 不持久化）。
func SetCounterPersistence(save func(uid string, c CounterSnapshot), restore func(uid string) (CounterSnapshot, bool)) {
	qmu.Lock()
	defer qmu.Unlock()
	persistSave = save
	persistRestore = restore
}

// persistLocked 把今日计数推给持久化钩子（调用方持 qmu）。
func persistLocked() {
	if persistSave == nil {
		return
	}
	t := today()
	for uid, c := range qcounters {
		if c.day != t {
			continue
		}
		snap := CounterSnapshot{Day: c.day, Chats: c.chats, Images: c.images, Videos: c.videos}
		if len(c.byModel) > 0 {
			snap.ByModel = make(map[string]int64, len(c.byModel))
			for k, v := range c.byModel {
				snap.ByModel[k] = v
			}
		}
		persistSave(uid, snap)
	}
}

// RestoreAccountCounters 启动时按账号恢复今日计数（main 对每个账号调用）。
func RestoreAccountCounters(uid string) {
	qmu.Lock()
	defer qmu.Unlock()
	if persistRestore == nil {
		return
	}
	snap, ok := persistRestore(uid)
	if !ok || snap.Day != today() {
		return
	}
	c := &dbDayCounter{day: snap.Day, chats: snap.Chats, images: snap.Images, videos: snap.Videos, byModel: map[string]int64{}}
	if snap.ByModel != nil {
		for k, v := range snap.ByModel {
			c.byModel[k] = v
		}
	}
	qcounters[uid] = c
}

func today() string { return time.Now().Format("2006-01-02") }

// counterOf 取（或新建）账号当日计数。
func counterOf(uid string) *dbDayCounter {
	qmu.Lock()
	defer qmu.Unlock()
	c, ok := qcounters[uid]
	if !ok || c.day != today() {
		c = &dbDayCounter{day: today(), byModel: map[string]int64{}}
		qcounters[uid] = c
	}
	return c
}

// countChat 对话 +1。
func countChat(uid string) {
	qmu.Lock()
	defer qmu.Unlock()
	c, ok := qcounters[uid]
	if !ok || c.day != today() {
		c = &dbDayCounter{day: today(), byModel: map[string]int64{}}
		qcounters[uid] = c
	}
	c.chats++
	persistLocked()
}

// countImages 图片 +n 张（按模型记明细）。
func countImages(uid, modelName string, n int64) {
	if n <= 0 {
		return
	}
	qmu.Lock()
	defer qmu.Unlock()
	c, ok := qcounters[uid]
	if !ok || c.day != today() {
		c = &dbDayCounter{day: today(), byModel: map[string]int64{}}
		qcounters[uid] = c
	}
	c.images += n
	c.byModel[modelName] += n
	persistLocked()
}

// countVideos 视频 +1 条。
func countVideos(uid, modelName string) {
	qmu.Lock()
	defer qmu.Unlock()
	c, ok := qcounters[uid]
	if !ok || c.day != today() {
		c = &dbDayCounter{day: today(), byModel: map[string]int64{}}
		qcounters[uid] = c
	}
	c.videos++
	c.byModel[modelName]++
	persistLocked()
}

// usageToday 当日快照。
func usageToday(uid string) *dbDayCounter {
	qmu.Lock()
	defer qmu.Unlock()
	if c, ok := qcounters[uid]; ok && c.day == today() {
		clone := *c
		clone.byModel = make(map[string]int64, len(c.byModel))
		for k, v := range c.byModel {
			clone.byModel[k] = v
		}
		return &clone
	}
	return &dbDayCounter{day: today(), byModel: map[string]int64{}}
}

// checkQuota 生成前额度检查：达到上限返回 ErrQuota（账号池冷却切号）。
func (p *Provider) checkQuota(uid, kind string) error {
	switch kind {
	case "image":
		if p.ImageDailyLimit > 0 {
			if used := usageToday(uid).images; used >= p.ImageDailyLimit {
				return &provider.Error{Kind: provider.ErrQuota,
					Msg: fmt.Sprintf("今日图片生成额度已用完（本地计数 %d/%d，明日重置；可加豆包账号分摊）", used, p.ImageDailyLimit)}
			}
		}
	case "video":
		if p.VideoDailyLimit > 0 {
			if used := usageToday(uid).videos; used >= p.VideoDailyLimit {
				return &provider.Error{Kind: provider.ErrQuota,
					Msg: fmt.Sprintf("今日视频生成额度已用完（本地计数 %d/%d，明日重置；可加豆包账号分摊）", used, p.VideoDailyLimit)}
			}
		}
	}
	return nil
}

// buildQuotaDetail 组装额度明细（面板「生成额度」视图数据源）。
func (p *Provider) buildQuotaDetail(uid string) map[string]any {
	c := usageToday(uid)
	detail := map[string]any{
		"channel":           "doubao-web",
		"chats_today":       c.chats,
		"images_used_today": c.images,
		"images_limit":      p.ImageDailyLimit,
		"videos_used_today": c.videos,
		"videos_limit":      p.VideoDailyLimit,
	}
	if len(c.byModel) > 0 {
		byModel := map[string]int64{}
		for k, v := range c.byModel {
			byModel[k] = v
		}
		detail["by_model"] = byModel
	}
	return detail
}
