// Package config 加载 ChinaCPA 配置（config.json，缺省值内置）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config 全局配置。
type Config struct {
	Listen string `json:"listen"`
	// APIKey 网关 /v1/* 的 Bearer 密钥
	APIKey string `json:"api_key"`
	// PanelPassword 管理面板密码
	PanelPassword string `json:"panel_password"`
	// DataDir 数据目录（账号凭证/用量/状态）
	DataDir string `json:"data_dir"`

	Schedule Schedule `json:"schedule"`

	Qwen QwenConfig `json:"qwen"`

	// ModelAliases 模型别名：客户端写死的模型名 -> ChinaCPA 模型名。
	// 例：{"gpt-5.2-codex": "workbuddy:glm-5.3", "claude-sonnet-4-6": "trae:glm-5.2"}
	ModelAliases map[string]string `json:"model_aliases"`

	Pricing PricingConfig `json:"pricing"`

	// RequestLogDays 请求日志保留天数（本地数据库 data/requests.db，0 = 默认 90 天）
	RequestLogDays int `json:"request_log_days"`

	Server ServerConfig `json:"server"`
}

// Schedule 定时任务配置。
type Schedule struct {
	// CheckinHours 每日自动签到的小时列表（本地时间）
	CheckinHours []int `json:"checkin_hours"`
	// CheckinEnabled 自动签到总开关
	CheckinEnabled bool `json:"checkin_enabled"`
	// TasksEnabled WorkBuddy 任务中心自动接受+领奖
	TasksEnabled bool `json:"tasks_enabled"`
	// QuotaRefreshMinutes 额度自动刷新间隔（分钟）
	QuotaRefreshMinutes int `json:"quota_refresh_minutes"`
	// TokenRefreshHours token 保活刷新的小时列表
	TokenRefreshHours []int `json:"token_refresh_hours"`
}

// QwenConfig Qwen 平台配置。
type QwenConfig struct {
	// DailyRequestLimit OAuth 免费额度每日请求数上限（qwen3-coder 2000/天）
	DailyRequestLimit int64 `json:"daily_request_limit"`
}

// PricingConfig 计价配置。
type PricingConfig struct {
	// File 模型单价表路径（$/1M tokens）
	File string `json:"file"`
	// USDCNY 美元兑人民币汇率
	USDCNY float64 `json:"usd_cny"`
}

// ServerConfig HTTP 服务参数。
type ServerConfig struct {
	ReadTimeoutSeconds  int `json:"read_timeout_seconds"`
	WriteTimeoutSeconds int `json:"write_timeout_seconds"`
}

// Default 返回内置缺省配置。
func Default() *Config {
	return &Config{
		Listen:        ":8318",
		APIKey:        "sk-chinacpa-please-change-me",
		PanelPassword: "123456",
		DataDir:       "./data",
		Schedule: Schedule{
			CheckinHours:        []int{9, 21},
			CheckinEnabled:      true,
			TasksEnabled:        true,
			QuotaRefreshMinutes: 10,
			TokenRefreshHours:   []int{3},
		},
		Qwen: QwenConfig{DailyRequestLimit: 2000},
		Pricing: PricingConfig{
			File:   "",
			USDCNY: 7.1,
		},
		RequestLogDays: 90,
		Server:         ServerConfig{ReadTimeoutSeconds: 0, WriteTimeoutSeconds: 0},
	}
}

// Load 读取 path 的 JSON 配置并合并到缺省值；文件不存在返回缺省配置。
func Load(path string) (*Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.normalize()
	return cfg, nil
}

// normalize 修正非法取值。
func (c *Config) normalize() {
	if c.Listen == "" {
		c.Listen = ":8317"
	}
	if c.APIKey == "" {
		c.APIKey = "sk-chinacpa-please-change-me"
	}
	if c.PanelPassword == "" {
		c.PanelPassword = "123456"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if c.Schedule.QuotaRefreshMinutes <= 0 {
		c.Schedule.QuotaRefreshMinutes = 10
	}
	if c.Qwen.DailyRequestLimit <= 0 {
		c.Qwen.DailyRequestLimit = 2000
	}
	if c.Pricing.USDCNY <= 0 {
		c.Pricing.USDCNY = 7.1
	}
	if c.RequestLogDays <= 0 {
		c.RequestLogDays = 90
	}
}

// PricingFileAbs 计价表绝对路径（未配置时落在 DataDir/pricing.json）。
func (c *Config) PricingFileAbs() string {
	f := c.Pricing.File
	if f == "" {
		f = filepath.Join(c.DataDir, "pricing.json")
	}
	if filepath.IsAbs(f) {
		return f
	}
	abs, err := filepath.Abs(f)
	if err != nil {
		return f
	}
	return abs
}

// SaveExample 向 path 写出带注释说明的示例配置（JSON 不支持注释，字段说明见 README）。
func SaveExample(path string) error {
	cfg := Default()
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
