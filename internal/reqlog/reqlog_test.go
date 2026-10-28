package reqlog

import (
	"math"
	"testing"
)

func TestPercentile(t *testing.T) {
	sample := []int64{10, 20, 30, 40, 50} // 已升序
	cases := []struct {
		p    float64
		want float64
	}{
		{50, 30},          // 中位数
		{0, 10},           // 最小
		{100, 50},         // 最大
		{90, 46},          // 4*0.9=3.6 → 40*(0.4)+50*(0.6)=46
		{25, 20},          // 4*0.25=1 → 20
	}
	for _, c := range cases {
		got := percentile(sample, c.p)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("percentile(p=%.0f)=%v 期望 %v", c.p, got, c.want)
		}
	}
	if percentile(nil, 50) != 0 {
		t.Error("空样本应返回 0")
	}
	if percentile([]int64{7}, 99) != 7 {
		t.Error("单样本应返回自身")
	}
}

func TestAggregateLatency(t *testing.T) {
	s := &Stats{}
	// 3 条成功（耗时 100/200/300ms，首字 10/20/30），1 条失败（耗时 9999 不应计入分位数）
	aggregate(s, &Entry{Model: "m1", OK: true, TotalMS: 100, FirstMS: 10})
	aggregate(s, &Entry{Model: "m1", OK: true, TotalMS: 200, FirstMS: 20})
	aggregate(s, &Entry{Model: "m2", OK: true, TotalMS: 300, FirstMS: 30})
	aggregate(s, &Entry{Model: "m1", OK: false, TotalMS: 9999})
	finalize(s)
	if s.P50MS != 200 {
		t.Errorf("P50=%v 期望 200", s.P50MS)
	}
	if s.MaxMS != 300 {
		t.Errorf("Max 应只统计成功请求，得 %v", s.MaxMS)
	}
	if s.P50First != 20 {
		t.Errorf("首字 P50=%v 期望 20", s.P50First)
	}
	// 按模型：m1 有 2 条成功
	var m1 *ModelLatency
	for i := range s.LatencyByModel {
		if s.LatencyByModel[i].Model == "m1" {
			m1 = &s.LatencyByModel[i]
		}
	}
	if m1 == nil || m1.Count != 2 || m1.P50MS != 150 {
		t.Errorf("m1 耗时统计异常: %+v", m1)
	}
	// finalize 后样本应释放
	if s.dursAll != nil || s.dursByMdl != nil {
		t.Error("finalize 后样本内存未释放")
	}
}

// TestAggregateClientGap 请求间隔=完成(N+1)-完成(N)-耗时(N+1)：
// 倒序喂入两条 zcode 请求：N+1 完成于 ts=1000（耗时 30s），N 完成于 ts=940
// → 间隔 = (1000-940)*1000 - 30000 = 30000ms（工具跑了 30 秒）
func TestAggregateClientGap(t *testing.T) {
	s := &Stats{}
	// 需要 ≥5 样本才出统计，构造 6 对
	for i := 0; i < 6; i++ {
		done := int64(1000 - i*60) // 每条间隔 60s，其中模型占 30s
		aggregate(s, &Entry{Client: "zcode", OK: true, TS: done, TotalMS: 30000, FirstMS: 5000, Stream: true, Model: "m"})
	}
	finalize(s)
	var g *ClientGap
	for i := range s.GapByClient {
		if s.GapByClient[i].Client == "zcode" {
			g = &s.GapByClient[i]
		}
	}
	if g == nil {
		t.Fatal("应有 zcode 间隔统计")
	}
	if g.Samples != 5 {
		t.Errorf("样本数=%v 期望 5", g.Samples)
	}
	if g.P50MS != 30000 {
		t.Errorf("P50=%v 期望 30000（60s 间隔扣除 30s 模型耗时）", g.P50MS)
	}
}

// TestAggregatePhaseSplit 流式请求的阶段拆分：首字与生成样本
func TestAggregatePhaseSplit(t *testing.T) {
	s := &Stats{}
	for i := 0; i < 6; i++ {
		aggregate(s, &Entry{Model: "m", OK: true, Stream: true, FirstMS: 20000, TotalMS: 50000, TS: int64(1000 - i)})
	}
	finalize(s)
	if len(s.LatencyByModel) != 1 {
		t.Fatalf("应 1 个模型，得 %d", len(s.LatencyByModel))
	}
	ml := s.LatencyByModel[0]
	if ml.FirstP50MS != 20000 {
		t.Errorf("首字 P50=%v 期望 20000", ml.FirstP50MS)
	}
	if ml.GenP50MS != 30000 {
		t.Errorf("生成 P50=%v 期望 30000", ml.GenP50MS)
	}
}
