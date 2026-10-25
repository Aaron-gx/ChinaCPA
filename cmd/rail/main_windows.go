//go:build windows

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func main() {
	var (
		base     = flag.String("url", "http://127.0.0.1:8313", "面板地址")
		pass     = flag.String("password", "", "面板密码（留空读取 config.json）")
		config   = flag.String("config", "config.json", "chinacpa 配置文件，用于读密码与端口")
		interval = flag.Int("interval", 60, "轮询间隔（秒）")
		top      = flag.Bool("top", true, "窗口是否置顶")
		demo     = flag.Bool("demo", false, "用示例数据预览外观（不连服务）")
		demoExpanded = flag.Bool("demo-expanded", false, "配合 -demo：直接展开详情卡")
	)
	flag.Parse()

	// 单实例：多开会叠出几条 rail，
	// 而且它们的悬停状态互相独立，看起来就像卡片一直在。
	if !acquireSingleInstance() {
		log.Printf("浮动栏已在运行，不重复启动")
		return
	}

	opts := Options{
		BaseURL:     *base,
		Interval:    *interval,
		AlwaysOnTop: *top,
	}

	// Win32 UI 必须固定在同一个 OS 线程上
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := initGdiplus(); err != nil {
		log.Fatalf("GDI+ 初始化失败: %v", err)
	}

	rail, err := NewRail(opts)
	if err != nil {
		log.Fatalf("创建浮动栏失败: %v", err)
	}

	if *demo {
		rail.Seed(demoMetrics())
		// -demo-expanded 直接把详情卡展开，便于核对卡片布局
		if *demoExpanded {
			rail.hovered = true
			rail.expanded = true
			rail.requestPaint()
		}
		log.Printf("浮动栏已启动（演示数据）")
		rail.Run()
		return
	}

	pw := *pass
	if pw == "" {
		pw = passwordFromConfig(*config)
	}
	if pw == "" {
		pw = "123456"
	}
	opts.Password = pw

	cli, err := NewClient(*base, pw, 8*time.Second)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	// 后台轮询，把结果投给 UI 线程
	trigger := make(chan struct{}, 1)
	go func() {
		first := true
		tick := func() {
			ov, err := cli.Fetch()
			if err != nil {
				log.Printf("取数失败: %v", err)
				if first {
					rail.Seed(nil)
					first = false
				} else {
					rail.Push(nil)
				}
				return
			}
			if first {
				rail.Seed(Metrics(ov))
				first = false
			} else {
				rail.Push(Metrics(ov))
			}
		}
		tick()
		t := time.NewTicker(time.Duration(*interval) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				tick()
			case <-trigger:
				// 右键「立即刷新」
				tick()
			}
		}
	}()
	rail.onRefresh = func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}

	log.Printf("浮动栏已启动 · 面板 %s · 每 %d 秒刷新", *base, *interval)
	rail.Run()
}

// demoMetrics 预览外观用的示例数据。
func demoMetrics() []Metric {
	return []Metric{
		{
			Label: "Qwen", Full: "Qwen 通义千问", Provider: "qwen", Used: 0.17,
			Text: "340/2K", Unit: "次",
			Rows: []MetricRow{
				{Label: "每日配额", UsedPct: 17, Reset: "明日 00:00 重置", Detail: "已用 340 / 2K"},
			},
			AccountCount: 1,
		},
		{
			Label: "WorkBuddy", Full: "WorkBuddy / CodeBuddy", Provider: "workbuddy",
			Balance: true, Used: 1, Text: "3.8K", Unit: "credits",
			Rows: []MetricRow{
				{Label: "积分余额", UsedPct: 0, Detail: "剩余 3.8K credits"},
			},
			AccountCount: 1,
		},
		{
			Label: "Trae", Full: "Trae", Provider: "trae",
			Balance: true, Used: 1, Text: "842", Unit: "credits",
			Rows: []MetricRow{
				{Label: "ide_credits", UsedPct: 0, Detail: "剩余 842"},
			},
			AccountCount: 1,
		},
		{
			Label: "Qoder", Full: "Qoder", Provider: "qoder", Used: 0.92,
			Text: "2.4K/2.6K", Unit: "credits",
			Rows: []MetricRow{
				{Label: "月度配额", UsedPct: 92, Reset: "11/01 00:00 重置", Detail: "已用 2.4K / 2.6K"},
			},
			AccountCount: 1,
		},
	}
}

// passwordFromConfig 从 chinacpa 的 config.json 读面板密码。
// 只在同目录或指定路径存在时读取；失败返回空串，由调用方兜底。
func passwordFromConfig(path string) string {
	// 允许从任意目录启动：先试试当前目录，再试试可执行文件旁边
	candidates := []string{path}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), path))
	}
	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			PanelPassword string `json:"panel_password"`
			Listen        string `json:"listen"`
		}
		if err := jsonUnmarshal(raw, &cfg); err != nil {
			continue
		}
		if cfg.PanelPassword != "" {
			return cfg.PanelPassword
		}
	}
	return ""
}

func jsonUnmarshal(data []byte, v any) error {
	return jsonUnmarshalImpl(data, v)
}

var _ = fmt.Sprint
