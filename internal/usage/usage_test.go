package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecordAndAggregate 记录 → 今日聚合 → 计费估算 → 跨日重置。
func TestRecordAndAggregate(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir, "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	tr.Record(Record{Provider: "qwen", Account: "u1", Model: "qwen3-coder-plus", PromptTokens: 1000000, CompletionTokens: 500000})
	tr.Record(Record{Provider: "qwen", Account: "u1", Model: "qwen3-coder-plus", PromptTokens: 1000000, CompletionTokens: 500000})
	items := tr.TodayWithCost()
	if len(items) != 1 || items[0].Requests != 2 {
		t.Fatalf("聚合错误: %+v", items)
	}
	if items[0].Total != 3000000 {
		t.Fatalf("token 合计错误: %d", items[0].Total)
	}
	// 计价：2M in * 0.002 + 1M out * 0.006 = 0.01 USD → ¥0.071
	if items[0].CostCNY < 0.070 || items[0].CostCNY > 0.072 {
		t.Fatalf("费用估算错误: %f", items[0].CostCNY)
	}
	// 请求数查询（qwen 额度回填）
	if n := tr.RequestsToday("qwen", "u1"); n != 2 {
		t.Fatalf("RequestsToday 错误: %d", n)
	}
}

// TestRestoreToday 重启回填：新建 Tracker 从日志恢复今日计数。
func TestRestoreToday(t *testing.T) {
	dir := t.TempDir()
	tr, _ := New(dir, "")
	tr.Record(Record{Provider: "trae", Account: "x", Model: "glm-5.2", PromptTokens: 10, CompletionTokens: 20})
	// 模拟重启
	tr2, err := New(dir, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n := tr2.RequestsToday("trae", "x"); n != 1 {
		t.Fatalf("重启回填失败: %d", n)
	}
	items := tr2.TodayWithCost()
	if len(items) != 1 || items[0].Total != 30 {
		t.Fatalf("重启聚合失败: %+v", items)
	}
}

// TestHistoryDays 近 N 日。
func TestHistoryDays(t *testing.T) {
	dir := t.TempDir()
	tr, _ := New(dir, "")
	tr.Record(Record{Provider: "qwen", Account: "u", Model: "m", PromptTokens: 5, CompletionTokens: 5})
	h := tr.HistoryDays(7)
	if len(h) != 7 {
		t.Fatalf("天数错误: %d", len(h))
	}
	today := time.Now().Format("2006-01-02")
	var found bool
	for _, d := range h {
		if d.Day == today && d.Total == 10 {
			found = true
		}
	}
	if !found {
		t.Fatalf("今日数据缺失: %+v", h)
	}
}

// TestPricingFile 自定义单价表加载。
func TestPricingFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.json")
	os.WriteFile(f, []byte(`{"usd_cny":7.0,"models":{"my-model":{"input":1,"output":2}}}`), 0o644)
	tr, _ := New(t.TempDir(), f)
	c := tr.Cost("my-model", 1000000, 1000000)
	if c < 20.99 || c > 21.01 { // 1*1 + 1*2 = 3 USD * 7 = 21
		t.Fatalf("自定义计价错误: %f", c)
	}
}

// TestCallsPerUnit 按次计费（图片/视频）：Calls/Unit 记账、聚合、重启回放与计价。
func TestCallsPerUnit(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir, "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// 发布模型按次单价 $0.02/张
	tr.SetPublishedPricing(map[string]PubPrice{
		"seedream": {PerCall: 0.02},
	}, 7.0)
	tr.Record(Record{Provider: "doubao", Account: "u1", Model: "seedream", Calls: 3, Unit: "images"})
	items := tr.TodayWithCost()
	if len(items) != 1 || items[0].Calls != 3 {
		t.Fatalf("聚合错误: %+v", items)
	}
	// 3 张 * $0.02 * 7 = ¥0.42
	if items[0].CostCNY < 0.41 || items[0].CostCNY > 0.43 {
		t.Fatalf("按次计价错误: %f", items[0].CostCNY)
	}
	if c := tr.CostCalls("seedream", 3); c < 0.41 || c > 0.43 {
		t.Fatalf("CostCalls 错误: %f", c)
	}
	// 重启回放保留 Calls/Unit
	tr2, err := New(dir, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	items2 := tr2.TodayWithCost()
	if len(items2) != 1 || items2[0].Calls != 3 || items2[0].Unit != "images" {
		t.Fatalf("重启回放失败: %+v", items2)
	}
	// HistoryDays 带 Calls
	h := tr2.HistoryDays(1)
	if h[0].Calls != 3 {
		t.Fatalf("HistoryDays 丢失 Calls: %+v", h)
	}
}
