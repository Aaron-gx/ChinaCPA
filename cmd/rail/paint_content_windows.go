//go:build windows

package main

import (
	"log"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 尺寸缩放。
//
// 基准是 Pulse 的 pt 值，落地到 Windows 要经过两层换算：
//   1. DPI：96dpi 下 1pt = 4/3 px；125% 缩放下 1pt = 4/3*1.25 px。
//   2. 屏幕高度：Pulse 的基准按笔记本屏（约 900pt 高）定。1080p 上
//      同样的像素占比更大，一条 640px 高的 rail 会吃掉 60% 屏幕。
var uiScale float32 = 4.0 / 3.0

func setUIScale(dpi int32, screenH int32) {
	base := float32(4.0) / 3.0
	if dpi > 0 {
		base = float32(dpi) / 72.0
	}
	var fit float32
	switch {
	case screenH >= 1600:
		fit = 1.12
	case screenH >= 1300:
		fit = 1.0
	case screenH >= 1100:
		fit = 0.90
	case screenH >= 1000:
		fit = 0.82
	default:
		fit = 0.74
	}
	uiScale = base * fit
}

func pd(v float64) int32 { return int32(float32(v)*uiScale + 0.5) }
func pdu(v float64) float32 { return float32(v) * uiScale }

// 浮动栏尺寸。全部由 pt 值经 pd() 换算，随 DPI 与屏幕高度缩放。
//
// 用访问器而不是全局 var：uiScale 在窗口创建时才确定（依赖 DPI 和
// 屏高），而包级 var 的初始化早于它，会永远拿到 96dpi 的默认值。
func railWidthPillF() int32 { return pd(64) }
func ringDiameterF() int32  { return pd(36) }
func ringLineF() int32 { return pd(4) }
func ringCentreGapF() int32 { return pd(4) }
func itemSpacingF() int32   { return pd(30) }
func percentFontF() int32 { return pd(13) }
func percentHeightF() int32 { return pd(16) }
func ringToTextF() int32    { return pd(6) }

func cardWidthF() int32      { return pd(250) }
func cardPadF() int32        { return pd(18) }
func cardRadiusF() int32     { return pd(20) }
func cardPointerWF() int32   { return pd(20) }
func cardPointerHF() int32   { return pd(40) }
func cardGapF() int32        { return pd(8) }
func cardContentSpacingF() int32 { return pd(14) }
func cardRowSpacingF() int32 { return pd(7) }
func cardTitleFontF() int32 { return pd(14) }
func cardRowFontF() int32 { return int32(11.5*float64(uiScale) + 0.5) }
func cardHeaderIconF() int32 { return pd(16) }
func cardBarHeightF() int32  { return pd(5) }
func cardRowLineF() int32    { return pd(14) }
func padTopF() int32         { return pd(20) }

// iconScale 图标占中心盘的比例。Pulse 用 0.8，但它的图标是 Lobe 的
// 线稿 SVG（本身留白多）；我们的素材是满边方形 app icon，裁圆后 0.8
// 仍显撑满，降到 0.68。
const iconScale = 0.68

// capRadiusF() rail 两端全圆
func capRadiusF() int32 { return railWidthPillF() / 2 }

// slotHeightF() 一个「环 + 百分比」槽的高度
func slotHeightF() int32 {
	return ringDiameterF() + ringToTextF() + percentHeightF() + itemSpacingF()
}

// cardReachF() 从 rail 左边缘到卡片左边缘的距离
func cardReachF() int32 {
	return railWidthPillF() + cardGapF() + cardPointerWF() + cardWidthF()
}

// centreDiameter 中心盘直径：环内边缘再内缩 centreGap。
func centreDiameter() int32 {
	return ringDiameterF() - (ringLineF()+ringCentreGapF())*2
}

// 布局：算出当前尺寸下 rail 与卡片的矩形。
type layout struct {
	pill  rect   // rail（右侧贴边）
	card  rect   // 详情卡本体（不含指针）
	ptr   rect   // 指向环的小三角外接矩形
	slots []int32 // 每个环的圆心 Y
}

// computeLayout 按窗口尺寸与状态算各块位置。
func (r *Rail) computeLayout(w, h int32) layout {
	var L layout
	n := len(r.metrics)
	if n > 8 {
		n = 8
	}

	// rail 贴窗口右边缘（窗口本身再贴屏幕边，见 applyPlacement）
	pillX := w - railWidthPillF()
	L.pill = rect{pillX, 0, w, h}

	if n > 0 {
		total := int32(n)*slotHeightF() - itemSpacingF()
		startY := (h - total) / 2
		if startY < padTopF() {
			startY = padTopF()
		}
		L.slots = make([]int32, n)
		for i := 0; i < n; i++ {
			// 圆心位于槽的上部
			L.slots[i] = startY + int32(i)*slotHeightF() + ringDiameterF()/2
		}
	}

	if r.expanded && n > 0 {
		ch := r.cardHeight()
		// 卡片对齐到当前悬停的环
		i := r.hoverIndex
		if i < 0 || i >= len(L.slots) {
			i = 0
		}
		anchor := L.slots[i]
		cy := anchor - ch/2
		if cy < 8 {
			cy = 8
		}
		if cy+ch > h-8 {
			cy = h - 8 - ch
		}
		cardRight := pillX - cardGapF() - cardPointerWF()
		cardLeft := cardRight - cardWidthF()
		L.card = rect{cardLeft, cy, cardRight, cy + ch}
		// 指针：贴在卡片右侧、垂直对准锚点环
		L.ptr = rect{cardRight, anchor - cardPointerHF()/2, pillX - cardGapF(), anchor + cardPointerHF()/2}
	}
	return L
}

// cardHeight 卡片高度：标题 + 分隔 + 每行。
func (r *Rail) cardHeight() int32 {
	m := r.hoveredMetric()
	if m == nil {
		return 0
	}
	rows := int32(len(m.Rows))
	if rows == 0 {
		rows = 1
	}
	if rows > 4 {
		rows = 4
	}
	// 标题行 + 内容间距 + 每行(标题+条+百分比)
	title := cardTitleFontF() + 8
	rowH := cardRowLineF()*2 + cardRowSpacingF()*2 + cardBarHeightF()
	h := cardPadF()*2 + title + cardContentSpacingF() + rows*rowH + (rows-1)*cardContentSpacingF()
	return h
}

// hoveredMetric 当前悬停（否则第一个）的环。
func (r *Rail) hoveredMetric() *Metric {
	if len(r.metrics) == 0 {
		return nil
	}
	i := r.hoverIndex
	if i < 0 || i >= len(r.metrics) {
		i = 0
	}
	return &r.metrics[i]
}

// paint 绘制整个浮动栏。
//
// 分层窗口用 per-pixel alpha：先画到 32 位 DIB（BGRA 预乘），
// 再用 UpdateLayeredWindow 提交。
func (r *Rail) paint() {
	var cr rect
	pGetClientRect.Call(uintptr(r.hwnd), uintptr(unsafe.Pointer(&cr)))
	w, h := int32(cr.Right-cr.Left), int32(cr.Bottom-cr.Top)
	if w <= 0 || h <= 0 {
		return
	}

	dib, err := newDIB(w, h)
	if err != nil {
		return
	}
	defer dib.Close()
	dib.clear()

	p, err := newPainter(windows.Handle(dib.dc))
	if err != nil {
		return
	}
	p.target, p.targetW, p.targetH = unsafe.Pointer(dib.mem), w, h

	L := r.computeLayout(w, h)
	if r.expanded && L.card.Right > L.card.Left {
		r.paintCard(p, L)
	}
	r.paintPill(p, L)

	p.Close()
	if err := dib.commit(r.hwnd); err != nil {
		if !r.commitErrLogged {
			log.Printf("提交位图失败: %v", err)
			r.commitErrLogged = true
		}
	}
}

// paintPill rail 本体：深色圆角条 + 一列圆环。
func (r *Rail) paintPill(p *painter, L layout) {
	pw := L.pill.Right - L.pill.Left
	ph := L.pill.Bottom - L.pill.Top
	p.fillRound(L.pill.Left, L.pill.Top, pw, ph, capRadiusF(), colRail)

	if len(r.metrics) == 0 {
		cx := L.pill.Left + pw/2
		cy := L.pill.Top + ph/2
		// 空状态：一个空心轨道 + 一个虚化的图标位置
		p.arc(cx, cy, ringDiameterF()/2, float32(ringLineF()), 0, 359.9, colTrack)
		p.fillCircle(cx, cy, centreDiameter()/2, colDisc)
		return
	}

	cx := L.pill.Left + pw/2
	for i := range r.metrics {
		if i >= len(L.slots) {
			break
		}
		m := r.metrics[i]
		cy := L.slots[i]
		r.paintRing(p, cx, cy, m, i == r.hoverIndex)

		// 百分比：居中在环下方。字号按 pt→px 换算（96dpi 下 1pt≈1.33px），
		// Pulse 的 13pt 在 Windows 上对应约 17px，视觉大小才一致。
		label := percentLabel(m)
		col := colPercent
		if m.Empty {
			col = colPercentDim
		}
		if !m.Balance && !m.Empty && m.Used >= warningThreshold {
			col = colPercentWarn
		}
		textW := int32(percentWidth())
		p.text(label, cx-textW/2, cy+ringDiameterF()/2+ringToTextF(), textW, percentHeightF(),
			float32(percentFontF()), true, stringAlignmentCenter, col)
	}
}

// percentLabel 环下方的文字：有上限时显示已用百分比，纯余额显示金额。
func percentLabel(m Metric) string {
	switch {
	case m.Empty:
		return "—"
	case m.Balance:
		return m.Text
	default:
		return fmtPct(m.Used * 100)
	}
}

func percentWidth() int {
	// "100%" 在 13pt 下约 38pt；纯余额文字（如 "3.8K"）更长，
	// 给到 rail 宽度靠居中排版，不要卡在 38pt 上被裁掉
	return int(railWidthPillF()) - 8
}

// paintRing 一个圆环：轨道 + 进度弧 + 中心盘 + 图标。
//
// 结构完全对齐 Pulse 的 UsageRingView：
//   - 外圈是描边（不是填充饼），从 12 点顺时针
//   - 颜色只表示"用了多少"，不表示平台
//   - 环内是深色圆盘，图标占圆盘 80%，单色渲染
func (r *Rail) paintRing(p *painter, cx, cy int32, m Metric, active bool) {
	rad := (ringDiameterF()/2)

	// 轨道：整圈细环，淡淡的白色（Pulse 用 primary 18%）
	p.arcTrack(cx, cy, rad, float32(ringLineF()), colTrack)

	if !m.Empty {
		frac := m.Used
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		col := ringColorFor(m)
		if frac > 0 {
			p.arcRound(cx, cy, rad, float32(ringLineF()), 0, frac*359.9, col)
		}
		// active：外发光
		if active {
			p.arcGlow(cx, cy, rad, float32(ringLineF())+3, 0, frac*359.9, col, 0x30)
		}
	}

	// 中心深色圆盘
	discR := centreDiameter() / 2
	p.fillCircle(cx, cy, discR, colDisc)

	// 图标：占中心盘 80%（Pulse 的 iconScale）；无读数时压到 35%
	iconSize := int32(float32(discR*2) * float32(iconScale))
	if icon := loadIcon(m.Provider); icon != nil {
		op := uint8(0xFF)
		if m.Empty {
			op = 0x59 // 35%
		}
		p.drawIconAlpha(icon, cx, cy, iconSize, op)
	}

	if active {
		// 指针所在环：细亮边
		p.arc(cx, cy, rad+2, 1.2, 0, 359.9, colRingHalo)
	}
}

// ringColorFor 环的颜色：只表达"离上限还有多远"。
//
// 阈值和配色照搬 Pulse 的 UsageTint：< 50% 绿、< 75% 黄、>= 75% 红、
// 耗尽用更深的红。
func ringColorFor(m Metric) argb {
	if m.Balance {
		// 纯余额没有上限比例：用中性蓝，不冒充用量
		return colBalance
	}
	switch {
	case m.Used >= 1:
		return colExhausted
	case m.Used >= 0.75:
		return colWarning
	case m.Used >= 0.5:
		return colCaution
	default:
		return colGood
	}
}

const warningThreshold = 0.75

// paintCard 详情卡：Pulse 的 UsageDetailCard 结构。
//
// 标题行（图标 + 平台名）+ 每个限制一行（标签/重置时间 → 进度条 → 已用/数值），
// 右侧带一个指向当前环的小三角。
func (r *Rail) paintCard(p *painter, L layout) {
	m := r.hoveredMetric()
	if m == nil {
		return
	}
	c := L.card
	cw := c.Right - c.Left
	ch := c.Bottom - c.Top

	// 卡片本体 + 指针（指针先画，才不会被卡片盖住接缝）
	p.fillRound(c.Left, c.Top, cw, ch, cardRadiusF(), colCardBg)
	p.fillPointer(L)

	// 标题行
	headY := c.Top + cardPadF()
	iconSz := int32(cardHeaderIconF())
	if icon := loadIcon(m.Provider); icon != nil {
		p.drawIconAlpha(icon, c.Left+cardPadF()+iconSz/2, headY+iconSz/2, iconSz, 0xFF)
	}
	titleX := c.Left + cardPadF() + iconSz + 8
	p.text(m.Full, titleX, headY-1, c.Right-cardPadF()-titleX, cardTitleFontF()+4,
		float32(cardTitleFontF()), true, stringAlignmentNear, colCardTitle)

	// 各行
	rowY := headY + cardTitleFontF() + 8 + cardContentSpacingF()
	rowW := cw - cardPadF()*2
	for i, row := range m.Rows {
		if i >= 4 {
			break
		}
		// 标签 + 重置时间
		p.text(row.Label, c.Left+cardPadF(), rowY, rowW*2/3, cardRowLineF(),
			float32(cardRowFontF()), true, stringAlignmentNear, colCardLabel)
		if row.Reset != "" {
			p.text(row.Reset, c.Left+cardPadF()+rowW/2, rowY, rowW/2, cardRowLineF(),
				float32(cardRowFontF())-0.5, false, stringAlignmentRight, colCardDim)
		}

		// 进度条
		barY := rowY + cardRowLineF() + cardRowSpacingF()
		p.fillRound(c.Left+cardPadF(), barY, rowW, cardBarHeightF(), cardBarHeightF()/2, colCardTrack)
		frac := row.UsedPct / 100
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		if frac > 0 {
			fillW := int32(float64(rowW) * frac)
			if fillW < cardBarHeightF() {
				fillW = cardBarHeightF()
			}
			p.fillRound(c.Left+cardPadF(), barY, fillW, cardBarHeightF(), cardBarHeightF()/2,
				barColorFor(frac))
		}

		// 已用百分比 + 明细
		infoY := barY + cardBarHeightF() + cardRowSpacingF()
		p.text(fmtPct(row.UsedPct), c.Left+cardPadF(), infoY, rowW/3, cardRowLineF(),
			float32(cardRowFontF()), true, stringAlignmentNear, colCardLabel)
		if row.Detail != "" {
			p.text(row.Detail, c.Left+cardPadF()+rowW/3, infoY, rowW*2/3, cardRowLineF(),
				float32(cardRowFontF())-0.5, false, stringAlignmentRight, colCardDim)
		}

		rowY += cardRowLineF()*2 + cardRowSpacingF()*2 + cardBarHeightF() + cardContentSpacingF()
	}
}

// barColorFor 卡片内进度条的颜色，和环同一套阈值。
func barColorFor(frac float64) argb {
	switch {
	case frac >= 1:
		return colExhausted
	case frac >= warningThreshold:
		return colWarning
	case frac >= 0.5:
		return colCaution
	default:
		return colGood
	}
}

// fillPointer 画卡片右侧指向环的小三角（Pulse 的卡片指针）。
//
// 直接按像素画：这块只有几十个像素，逐列算整数区间比 GDI+ 路径可靠 ——
// 而且它必须和图标贴图走同一条 CPU 写入路径，否则顺序会打架。
func (p *painter) fillPointer(L layout) {
	if p.target == nil || p.graphics == 0 {
		return
	}
	pGdipFlush.Call(p.graphics, 1)

	tipX := L.ptr.Right
	baseX := L.card.Right
	midY := (L.ptr.Top + L.ptr.Bottom) / 2
	half := cardPointerHF() / 2 * 45 / 100

	if tipX <= baseX || half <= 0 {
		return
	}
	col := uint32(colCardBg)
	buf := (*[1 << 26]uint32)(p.target)[: p.targetW*p.targetH : p.targetW*p.targetH]

	for x := baseX; x < tipX; x++ {
		if x < 0 || x >= p.targetW {
			continue
		}
		t := float32(x-baseX) / float32(tipX-baseX)
		h := int32(float32(half) * (1 - t))
		if h <= 0 {
			continue
		}
		for y := midY - h; y <= midY+h; y++ {
			if y < 0 || y >= p.targetH {
				continue
			}
			buf[y*p.targetW+x] = col
		}
	}
}

// fmtPct 百分比文案。
func fmtPct(p float64) string {
	if p <= 0 {
		return "0%"
	}
	if p < 1 {
		return strconvF(p, 1) + "%"
	}
	// Pulse 的百分比是整数
	return itoa(int64(math.Round(p))) + "%"
}

// strconvF 定点格式化。
func strconvF(v float64, prec int) string {
	pow := math.Pow(10, float64(prec))
	n := math.Round(v*pow) / pow
	intPart := int64(n)
	frac := n - float64(intPart)
	if prec == 0 {
		return itoa(intPart)
	}
	fracDigits := int64(math.Round(frac * pow))
	if fracDigits >= int64(pow) {
		intPart++
		fracDigits -= int64(pow)
	}
	return itoa(intPart) + "." + padLeft(itoa(fracDigits), prec)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func padLeft(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}

// fillRect 填充矩形。
func (p *painter) fillRect(x, y, w, h int32, c argb) {
	p.fillRound(x, y, w, h, 0, c)
}

// paintStruct Win32 PAINTSTRUCT。
type paintStruct struct {
	Hdc       windows.Handle
	Erase     int32
	Paint     rect
	Restore   int32
	IncUpdate int32
	Reserved  [32]byte
}
