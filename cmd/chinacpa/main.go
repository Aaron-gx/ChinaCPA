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
	"chinacpa/internal/provider/qwen"
	"chinacpa/internal/provider/trae"
	"chinacpa/internal/provider/workbuddy"
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
	wb := workbuddy.New()
	qw := qwen.New(cfg.Qwen.DailyRequestLimit)
	tr := trae.New()

	// pool
	provs := []provider.Provider{wb, qw, tr}
	pl, err := pool.New(st, provs)
	if err != nil {
		log.Fatalf("加载账号失败: %v", err)
	}
	pl.OnAccountChanged = func(a *model.Account) {
		if err := pl.SaveAccount(a); err != nil {
			log.Printf("WARN: 账号凭证落盘失败 %s/%s: %v", a.Provider, a.UID, err)
		}
	}

	// usage
	u, err := usage.New(filepath.Join(cfg.DataDir, "usage"), cfg.PricingFileAbs())
	if err != nil {
		log.Fatalf("初始化用量目录失败: %v", err)
	}

	// Qwen 今日计数回填（重启恢复）
	for _, a := range pl.Accounts() {
		if a.Provider == "qwen" {
			qw.RestoreCounters(a.UID, u.RequestsToday("qwen", a.UID))
		}
	}

	// gateway + scheduler + panel
	gw := gateway.New(pl, u)
	gw.SetQwenCounter(qw)
	sched := scheduler.New(cfg, pl, st)
	sched.Start()
	pn := panel.New(cfg, pl, st, u, sched)

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
