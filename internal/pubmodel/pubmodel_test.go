package pubmodel

import (
	"path/filepath"
	"testing"
)

// TestChanStatPersistence 计数 Flush 后重新 Load 应保留（跨重启）。
func TestChanStatPersistence(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "models.json")
	statsFile := filepath.Join(dir, "channel_stats.json")

	m, err := Load(cfgFile, statsFile)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m.BumpChannel("deepseek-flash", "workbuddy", "deepseek-v4.1-flash", true)
	m.BumpChannel("deepseek-flash", "workbuddy", "deepseek-v4.1-flash", true)
	m.BumpChannel("deepseek-flash", "workbuddy", "deepseek-v4.1-flash", false)
	if err := m.FlushStats(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	m2, err := Load(cfgFile, statsFile)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := m2.ChannelStats()["deepseek-flash|workbuddy|deepseek-v4.1-flash"]
	if got.OK != 2 || got.Fail != 1 {
		t.Fatalf("期望 OK=2 Fail=1，得到 %+v", got)
	}
}

// TestChanStatKeyNormalization 键归一化：大小写与空白不影响同一渠道计数。
func TestChanStatKeyNormalization(t *testing.T) {
	dir := t.TempDir()
	m, err := Load(filepath.Join(dir, "models.json"), "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m.BumpChannel("DeepSeek-Flash", "workbuddy", "m", true)
	if got := m.ChannelStats()["deepseek-flash|workbuddy|m"]; got.OK != 1 {
		t.Fatalf("键应归一化，得到 %+v", got)
	}
}
