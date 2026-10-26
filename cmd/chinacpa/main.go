// ChinaCPA —— 国产大模型反代网关：WorkBuddy(腾讯 CodeBuddy) / Qwen(通义) / Trae(字节)
// 单二进制：OAuth 凭证纳管 + 自动签到积分 + 额度自动刷新 + OpenAI 兼容网关 + Web 仪表板。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"chinacpa/internal/config"
	"chinacpa/internal/gateway"
	"chinacpa/internal/model"
	"chinacpa/internal/panel"
	"chinacpa/internal/pool"
	"chinacpa/internal/provider"
	"chinacpa/internal/provider/doubao"
	"chinacpa/internal/provider/qoder"
	"chinacpa/internal/provider/qwen"
	"chinacpa/internal/provider/qwenwork"
	"chinacpa/internal/provider/trae"
	"chinacpa/internal/provider/workbuddy"
	"chinacpa/internal/pubmodel"
	"chinacpa/internal/reqlog"
	"chinacpa/internal/scheduler"
	"chinacpa/internal/store"
	"chinacpa/internal/usage"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if _, statErr := os.Stat(*cfgPath); os.IsNotExist(statErr) {
		if err := config.SaveExample(*cfgPath); err == nil {
			log.Printf("已生成示例配置 %s（请按需修改 api_key / panel_password）", *cfgPath)
		}
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化数据目录失败: %v", err)
	}

	// providers
	wb := workbuddy.New("cn")
	wbi := workbuddy.New("global")
	qw := qwen.New(cfg.Qwen.DailyRequestLimit)
	tr := trae.New()
	qd := qoder.New("cn")
	qdi := qoder.New("intl")
	qww := qwenwork.New()
	db := doubao.New(cfg.Doubao.ImageDailyLimit, cfg.Doubao.VideoDailyLimit)

	// pool
	provs := []provider.Provider{wb, wbi, qw, tr, qd, qdi, qww, db}
	pl, err := pool.New(st, provs)
	if err != nil {
		log.Fatalf("加载账号失败: %v", err)
	}
	pl.OnAccountChanged = func(a *model.Account) {
		if err := pl.SaveAccount(a); err != nil {
			log.Printf("WARN: 账号凭证落盘失败 %s/%s: %v", a.Provider, a.UID, err)
		}
	}
	pl.SetModelAliases(cfg.ModelAliases)

	// usage
	u, err := usage.New(filepath.Join(cfg.DataDir, "usage"), cfg.PricingFileAbs())
	if err != nil {
		log.Fatalf("初始化用量目录失败: %v", err)
	}

	// 发布模型（对外模型 → 多渠道路由 + 单价）
	pub, err := pubmodel.Load(filepath.Join(cfg.DataDir, "models.json"))
	if err != nil {
		log.Fatalf("加载发布模型失败: %v", err)
	}
	pl.SetPublished(pub)
	pubPrices := map[string]usage.PubPrice{}
	for k, v := range pub.PricingMap() {
		pubPrices[k] = usage.PubPrice{Input: v.Input, Output: v.Output, Cache: v.Cache, PerCall: v.PerCall}
	}
	// 汇率优先级：发布模型配置 > config.json pricing.usd_cny > usage 内置缺省
	pubUSDCNY := pub.USDCNY()
	if pubUSDCNY <= 0 {
		pubUSDCNY = cfg.Pricing.USDCNY
	}
	u.SetPublishedPricing(pubPrices, pubUSDCNY)
	if n := len(pub.Get().Models); n > 0 {
		log.Printf("发布模型 %d 个（/v1/models 仅暴露已发布模型）", n)
	}

	// Qwen 今日计数回填（重启恢复）
	for _, a := range pl.Accounts() {
		if a.Provider == "qwen" {
			qw.RestoreCounters(a.UID, u.RequestsToday("qwen", a.UID))
		}
	}

	// 豆包每日计数跨重启持久化（store 落盘，键 provider:uid:day:kind）
	doubao.SetCounterPersistence(
		func(uid string, c doubao.CounterSnapshot) {
			day := time.Now().Format("2006-01-02")
			st.SetCounter("doubao:"+uid+":"+day+":chats", c.Chats)
			st.SetCounter("doubao:"+uid+":"+day+":images", c.Images)
			st.SetCounter("doubao:"+uid+":"+day+":videos", c.Videos)
		},
		func(uid string) (doubao.CounterSnapshot, bool) {
			day := time.Now().Format("2006-01-02")
			prefix := "doubao:" + uid + ":" + day + ":"
			vals := st.CountersPrefix(prefix)
			if len(vals) == 0 {
				return doubao.CounterSnapshot{}, false
			}
			return doubao.CounterSnapshot{
				Day:    day,
				Chats:  vals[prefix+"chats"],
				Images: vals[prefix+"images"],
				Videos: vals[prefix+"videos"],
			}, true
		},
	)
	for _, a := range pl.Accounts() {
		if a.Provider == "doubao" {
			doubao.RestoreAccountCounters(a.UID)
		}
	}

	// 请求日志（bbolt 本地数据库，自动迁移历史 JSONL）
	rl, err := reqlog.Open(filepath.Join(cfg.DataDir, "usage"))
	if err != nil {
		log.Fatalf("初始化请求日志失败: %v", err)
	}
	retentionDays := cfg.RequestLogDays
	if retentionDays <= 0 {
		retentionDays = 90
	}
	go func() {
		cutoff := func() int64 { return time.Now().AddDate(0, 0, -retentionDays).Unix() }
		if n, err := rl.DeleteBefore(cutoff()); err == nil && n > 0 {
			log.Printf("请求日志清理 %d 条（保留 %d 天）", n, retentionDays)
		}
		t := time.NewTicker(6 * time.Hour)
		for range t.C {
			if n, err := rl.DeleteBefore(cutoff()); err == nil && n > 0 {
				log.Printf("请求日志清理 %d 条", n)
			}
		}
	}()

	// gateway + scheduler + panel
	gw := gateway.New(pl, u)
	gw.SetQwenCounter(qw)
	gw.SetReqLog(rl)
	sched := scheduler.New(cfg, pl, st)
	sched.Start()
	pn := panel.New(cfg, pl, st, u, sched, pub, rl)

	mux := http.NewServeMux()
	pn.Register(mux, gw)

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSeconds) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSeconds) * time.Second,
	}
	host := "127.0.0.1" + cfg.Listen
	log.Printf("ChinaCPA 启动: 面板 http://%s/ 接口 http://%s/v1 账号 %d 个", host, host, len(pl.Accounts()))
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
}
