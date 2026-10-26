//go:build windows

package main

import (
	"math"
)

// 动画相关的绘制。
//
// 这一层把 anim_windows.go 里的状态画出来，对应 Pulse 的
// UsageRingView 里四个附加视觉元素：
//
//   - 外层窗口时钟弧（windowClockFraction）—— 环外一圈细线
//   - 内层第二限制环（secondFraction）—— 环内更细的小环
//   - 刷新扫描弧（isRefreshing）—— 拉数据时一圈彩色短弧
//   - CLI 活动标记（isBusy）—— 白色短弧在环内侧游走
//
// 另外 progress 弧本身用弹簧插值（Pulse 的 spring(response:0.5, damping:0.85)）。

const (
	// 窗口时钟弧：环外 3pt 间隙、2pt 线宽（Pulse 的 clockGap / clockLineWidth）
	clockGap      = 3.0
	clockLine     = 2.0
	// 第二限制环：26pt 直径、2.5pt 线宽（DockLayout.secondRingDiameter）
	secondRingDia = 26.0
	secondRingLine = 2.5
	// 刷新扫描弧占整圈的比例与活动标记的比例
	refreshSweep = 0.16
	busySweep    = 0.22
	// 悬停光晕半径（Pulse 的 haloRadius = 10）
	haloRadius = 10.0
)

// paintRingAnimated 画一个带全部动效的环。
//
// 绘制顺序照着 Pulse 的 ZStack：轨道 → 进度弧（带光晕）→ 刷新弧 →
// 中心盘 + 图标 → 第二环 → 活动标记。后者盖在前者上，和 Pulse 一致。
func (r *Rail) paintRingAnimated(p *painter, cx, cy int32, m Metric, a *ringAnim, active bool) {
	rad := ringDiameterF() / 2
	lineW := float32(ringLineF())

	// 1) 轨道
	p.arcTrack(cx, cy, rad, lineW, colTrack)

	// 2) 进度弧（弹簧插值后的比例）
	arcFrac := a.arc.cur
	if arcFrac < 0 {
		arcFrac = 0
	}
	if arcFrac > 1 {
		arcFrac = 1
	}
	col := ringColorFor(m)

	// 悬停光晕：Pulse 是在弧上加 42% 的 shadow，内圈挖空避免糊住图标。
	// GDI+ 没有 shadow，这里用几圈逐渐变淡的粗弧近似。
	if a.halo.cur > 0.01 && arcFrac > 0 {
		r.paintHalo(p, cx, cy, rad, lineW, arcFrac, col, a.halo.cur)
	}

	// 耗尽状态：整圈填满（Pulse: isSpent 时 arcFraction 返回 1）
	drawFrac := arcFrac
	if m.Spent {
		drawFrac = 1
	}
	if drawFrac > 0 && !m.Empty {
		p.arcRound(cx, cy, rad, lineW, 0, drawFrac*359.9, col)
	}

	// 3) 刷新扫描弧：一圈彩色短弧
	if a.refreshFade.cur > 0.01 {
		start := a.refresh.phase * 360
		p.arcRound(cx, cy, rad, lineW, start, refreshSweep*360,
			withAlpha(col, uint8(a.refreshFade.cur*255)))
	}

	// 4) 中心盘 + 图标
	discR := centreDiameter() / 2
	// 有第二环时中心盘每边让 2pt（Pulse 的 secondRingSqueeze）
	if m.SecondUsed >= 0 {
		discR -= 2
	}
	p.fillCircle(cx, cy, discR, colDisc)

	iconSize := int32(float32(discR*2) * float32(iconScale))
	op := uint8(0xFF)
	if m.Empty {
		op = 0x59
	}
	if icon := loadIcon(m.Provider); icon != nil {
		p.drawIconAlpha(icon, cx, cy, iconSize, op)
	} else {
		drawInitial(p, m.Label, cx, cy, float32(iconSize)*0.72, op)
	}

	// 5) 第二限制内环
	if m.SecondUsed >= 0 {
		r.paintSecondRing(p, cx, cy, m, a)
	}

	// 6) 活动标记：白色短弧（只在有请求在跑且没有机器人标记时画）
	if a.busyFade.cur > 0.01 {
		start := a.busy.phase * 360
		busyR := rad - int32(lineW*1.5) - 2
		if busyR < 4 {
			busyR = 4
		}
		col := withAlpha(colTextPrimary, uint8(a.busyFade.cur*255))
		p.arcRound(cx, cy, busyR, maxFloat(lineW*0.5, 1.5), start, busySweep*360, col)
	}

	// 7) 窗口时钟弧：环外侧的细线
	if m.ClockFrac >= 0 {
		r.paintClockArc(p, cx, cy, rad, lineW, a.clock.cur)
	}
}

// paintClockArc 环外侧的窗口时钟：一圈极淡的轨道 + 一段中性色进度。
//
// Pulse 明确说它用中性色而不是第二种彩色 —— 环上的颜色只表示用量，
// 再引入一个色相就成了第二套语言。线宽也更细（2pt），靠半径区分。
func (r *Rail) paintClockArc(p *painter, cx, cy, rad int32, ringLineW float32, frac float64) {
	if frac <= 0 {
		return
	}
	if frac > 1 {
		frac = 1
	}
	scale := uiScale
	gap := int32(clockGap * float64(scale) * 1.0)
	lineW := float32(clockLine * float64(scale))
	outerR := rad + int32(ringLineW)/2 + gap + int32(lineW)/2
	if outerR <= rad {
		return
	}
	// 轨道：16% 不透明度
	p.arcTrack(cx, cy, outerR, lineW, colClockTrack)
	// 进度：70% 不透明度
	p.arcRound(cx, cy, outerR, lineW, 0, frac*359.9, colClockArc)
}

// paintSecondRing 第二限制内环：更细、更小，同样的颜色语言。
func (r *Rail) paintSecondRing(p *painter, cx, cy int32, m Metric, a *ringAnim) {
	scale := uiScale
	dia := int32(secondRingDia * float64(scale))
	rad := dia / 2
	lineW := float32(secondRingLine * float64(scale))

	// 轨道 18%
	p.arcTrack(cx, cy, rad, lineW, colSecondTrack)

	frac := m.SecondUsed
	if frac < 0 {
		return
	}
	if frac > 1 {
		frac = 1
	}
	if frac <= 0 {
		return
	}
	// 颜色和主环同一套（Pulse 明确要求：两个环计量同一件事，必须读法一致）
	col := colorForFraction(frac, m.Spent)
	// 第二环不做弹簧：它变化慢，跟着主环的节奏反而怪
	p.arcRound(cx, cy, rad, lineW, 0, frac*359.9, col)
}

// paintHalo 悬停光晕：在弧的外侧叠几圈逐渐变淡的粗弧。
//
// Pulse 用 SwiftUI 的 shadow + 内圈 mask。GDI+ 没有阴影，所以用
// 三层同心粗弧模拟：最内层最亮、向外逐层变淡，半径从环边缘开始，
// 不往圆心方向画 —— 这就是 Pulse 那个 mask 在做的事（避免糊住图标）。
func (r *Rail) paintHalo(p *painter, cx, cy, rad int32, lineW float32, frac float64, col argb, strength float64) {
	if frac <= 0 {
		return
	}
	sweep := frac * 359.9
	base := uint8(0.42 * strength * 255) // Pulse 的 arcColour.opacity(0.42)
	layers := []struct {
		expand int32
		width  float32
		alpha  uint8
	}{
		{0, lineW + 2, base},
		{2, lineW + 4, uint8(float32(base) * 0.55)},
		{4, lineW + 6, uint8(float32(base) * 0.28)},
	}
	for _, l := range layers {
		if l.alpha == 0 {
			continue
		}
		p.arcGlow(cx, cy, rad+l.expand, l.width, 0, sweep, col, l.alpha)
	}
}

// colorForFraction 按比例给颜色（与 ringColorFor 同一套阈值）。
func colorForFraction(frac float64, spent bool) argb {
	if spent || frac >= 1 {
		return colExhausted
	}
	switch {
	case frac >= 0.75:
		return colWarning
	case frac >= 0.5:
		return colCaution
	default:
		return colGood
	}
}

// withAlpha 替换颜色的 alpha 通道。
func withAlpha(c argb, a uint8) argb {
	return argb(uint32(a)<<24 | uint32(c)&0x00FFFFFF)
}

func maxFloat(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

// 供 anim 代码判断用
var _ = math.Abs
