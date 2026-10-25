//go:build windows

package main

import (
	"fmt"
	"math"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	gdiplus = windows.NewLazySystemDLL("gdiplus.dll")

	pGdiplusStartup       = gdiplus.NewProc("GdiplusStartup")
	pGdiplusShutdown      = gdiplus.NewProc("GdiplusShutdown")
	pGdipCreateFromHDC    = gdiplus.NewProc("GdipCreateFromHDC")
	pGdipDeleteGraphics   = gdiplus.NewProc("GdipDeleteGraphics")
	pGdipSetSmoothingMode = gdiplus.NewProc("GdipSetSmoothingMode")
	pGdipSetTextRenderingHint = gdiplus.NewProc("GdipSetTextRenderingHint")
	pGdipCreatePen1       = gdiplus.NewProc("GdipCreatePen1")
	pGdipDeletePen        = gdiplus.NewProc("GdipDeletePen")
	pGdipSetPenLineCap197819 = gdiplus.NewProc("GdipSetPenStartCap")
	pGdipSetPenEndCap     = gdiplus.NewProc("GdipSetPenEndCap")
	pGdipDrawEllipseI     = gdiplus.NewProc("GdipDrawEllipseI")
	pGdipDrawArcI         = gdiplus.NewProc("GdipDrawArcI")
	pGdipFillEllipseI     = gdiplus.NewProc("GdipFillEllipseI")
	pGdipCreateSolidFill  = gdiplus.NewProc("GdipCreateSolidFill")
	pGdipDeleteBrush      = gdiplus.NewProc("GdipDeleteBrush")
	pGdipCreatePath       = gdiplus.NewProc("GdipCreatePath")
	pGdipDeletePath       = gdiplus.NewProc("GdipDeletePath")
	pGdipAddPathArcI      = gdiplus.NewProc("GdipAddPathArcI")
	pGdipClosePathFigure  = gdiplus.NewProc("GdipClosePathFigure")
	pGdipFillPath         = gdiplus.NewProc("GdipFillPath")
	pGdipCreateFontFamilyFromName = gdiplus.NewProc("GdipCreateFontFamilyFromName")
	pGdipDeleteFontFamily = gdiplus.NewProc("GdipDeleteFontFamily")
	pGdipCreateFont = gdiplus.NewProc("GdipCreateFont")
	pGdipDeleteFont       = gdiplus.NewProc("GdipDeleteFont")
	pGdipDrawString       = gdiplus.NewProc("GdipDrawString")
	pGdipCreateStringFormat = gdiplus.NewProc("GdipCreateStringFormat")
	pGdipDeleteStringFormat = gdiplus.NewProc("GdipDeleteStringFormat")
	pGdipSetStringFormatAlign = gdiplus.NewProc("GdipSetStringFormatAlign")
	pGdipSetStringFormatLineAlign = gdiplus.NewProc("GdipSetStringFormatLineAlign")
	pGdipSetStringFormatFlags = gdiplus.NewProc("GdipSetStringFormatFlags")
	pGdipMeasureString    = gdiplus.NewProc("GdipMeasureString")
)

// GDI+ 常量
const (
	smoothingAntiAlias  = 4
	// 分层窗口上必须用灰度抗锯齿。ClearType 做的是子像素渲染，
	// 会把字拆成 RGB 三个通道的偏移，写到带 alpha 的位图上就成了
	// 彩色镶边（放大后能明显看到橙/蓝边）。
	textRenderingAntiAlias = 4 // TextRenderingHintAntiAliasGridFit
	penAlignmentCenter  = 0
	lineCapRound        = 2
	fillModeAlternate   = 0
	stringFormatNoWrap  = 0x00001000
	stringAlignmentCenter = 1
	stringAlignmentNear = 0
	stringAlignmentRight = 2
)

// sizeF GDI+ 尺寸。
type sizeF struct{ Width, Height float32 }

// rectF GDI+ 矩形。
type rectF struct{ X, Y, Width, Height float32 }

// argb GDI+ 颜色（0xAARRGGBB）。
type argb uint32

func rgb(r, g, b uint8) argb { return argb(0xFF000000 | uint32(r)<<16 | uint32(g)<<8 | uint32(b)) }
func rgba(r, g, b, a uint8) argb {
	return argb(uint32(a)<<24 | uint32(r)<<16 | uint32(g)<<8 | uint32(b))
}

// 主题色：与面板一致的暖色系
// 配色照搬 Pulse 的 UsageTint 与面板底色。
//
// 关键原则（来自 Pulse 的注释）：环上的颜色只表达"离上限多远"，
// 不表达是哪个平台 —— 平台由图标表达。
var (
	// rail 与卡片：近黑，Pulse 的面板底色
	colRail     = rgb(0x0A, 0x0A, 0x0B)
	colCardBg   = rgb(0x1A, 0x1A, 0x1C)
	colCardTitle = rgba(0xF5, 0xF4, 0xF1, 0xFF)
	colCardLabel = rgba(0xD8, 0xD6, 0xD0, 0xFF)
	colCardDim   = rgba(0x8E, 0x8C, 0x86, 0xFF)
	colCardTrack = rgba(0x3A, 0x3A, 0x3C, 0xFF)

	// 环的轨道：primary 18% 不透明度，Pulse 用 Color.primary.opacity(0.18)
	// Pulse: Color.primary.opacity(0.18)。在近黑底上 0x2E(18%) 仍偏亮，
	// 因为 GDI+ 的 alpha 混合和 SwiftUI 不完全等价，实测降到 0x22 才接近。
	colTrack = rgba(0xFF, 0xFF, 0xFF, 0x22)
	// 环内深色圆盘
	colDisc = rgb(0x08, 0x08, 0x09)

	// 用量配色（Pulse 的 dark 变体）
	colGood      = rgb(0x00, 0xE6, 0x8C) // < 50%
	colCaution   = rgb(0xFF, 0xC2, 0x26) // < 75%
	colWarning   = rgb(0xFF, 0x4F, 0x42) // >= 75%
	colExhausted = rgb(0xD9, 0x17, 0x21) // 耗尽
	colBalance   = rgb(0x4A, 0x9E, 0xE8) // 纯余额：中性蓝

	// 百分比文字
	colPercent     = rgba(0xF5, 0xF4, 0xF1, 0xFF)
	colPercentDim  = rgba(0x78, 0x76, 0x72, 0xFF)
	colPercentWarn = rgb(0xFF, 0x7A, 0x70)

	// 悬停环的细亮边
	colRingHalo = rgba(0xFF, 0xFF, 0xFF, 0x4D)

	// 遗留（旧配色，保留给图标兜底绘制）
	colDotEmpty   = rgba(0x45, 0x44, 0x42, 0xFF)
	colLogoDim    = rgba(0x9A, 0x97, 0x90, 0xFF)
	colLogo       = rgba(0xFF, 0xFF, 0xFF, 0xFF)
	colBg         = rgba(0xFF, 0xFF, 0xFF, 0xFF)
	colBorder     = rgba(0xD8, 0xD3, 0xC4, 0xFF)
	colText       = rgb(0x1F, 0x1E, 0x1D)
	colMuted      = rgb(0x8A, 0x86, 0x7B)
	colAccent     = rgb(0xC9, 0x64, 0x42)
	colGreen      = rgb(0x5C, 0x8A, 0x5E)
	colYellow     = rgb(0x9A, 0x7B, 0x2D)
	colRed        = rgb(0xB3, 0x56, 0x4E)
	colShadow     = rgba(0x1F, 0x1E, 0x1D, 0x18)
	colDotActive  = rgba(0xFF, 0xFF, 0xFF, 0xAA)
	colPill       = colRail
	colCard       = colCardBg
	colCardText   = colCardTitle
	colCardMuted  = colCardLabel
	colCardFaint  = colCardDim
	colCardLine   = colCardTrack
	colCardIcon   = colCardTrack
	colDotGreen   = colGood
	colDotYellow  = colCaution
	colDotOrange  = colWarning
	colDotRed     = colExhausted
	colDotBalance = colBalance
	colDotHighlight = rgba(0x00, 0x00, 0x00, 0x55)
	colPct        = colPercent
	colPctMuted   = colPercentDim
)

var gdiplusToken uintptr
var gdiplusReady bool

// initGdiplus 启动 GDI+。进程内只需一次。
func initGdiplus() error {
	if gdiplusReady {
		return nil
	}
	// GdiplusStartupInput: GdiplusVersion(DWORD)=1, DebugEventCallback, SuppressBackgroundThread, SuppressExternalCodecs
	var input [4]uintptr
	input[0] = 1 // GdiplusVersion
	ret, _, _ := pGdiplusStartup.Call(
		uintptr(unsafe.Pointer(&gdiplusToken)),
		uintptr(unsafe.Pointer(&input[0])),
		0,
	)
	if ret != 0 {
		return fmt.Errorf("GdiplusStartup 失败: %d", ret)
	}
	gdiplusReady = true
	return nil
}

// painter 一次绘制的上下文。
type painter struct {
	graphics uintptr
	// 目标 DIB 表面（预乘 BGRA），供图标贴图直接按像素混合；
	// 只走 GDI+ 的绘制不设这两个字段。
	target  unsafe.Pointer
	targetW int32
	targetH int32
}

func newPainter(hdc windows.Handle) (*painter, error) {
	if err := initGdiplus(); err != nil {
		return nil, err
	}
	var g uintptr
	if ret, _, _ := pGdipCreateFromHDC.Call(uintptr(hdc), uintptr(unsafe.Pointer(&g))); ret != 0 {
		return nil, fmt.Errorf("GdipCreateFromHDC 失败: %d", ret)
	}
	pGdipSetSmoothingMode.Call(g, smoothingAntiAlias)
	pGdipSetTextRenderingHint.Call(g, textRenderingAntiAlias)
	// 画到带 alpha 的 DIB 上时，让 GDI+ 按 alpha 合成，圆角/圆环边缘才有过渡
	pGdipSetCompositingMode.Call(g, 0) // CompositingModeSourceOver
	pGdipSetCompositingQuality.Call(g, 2) // HighQuality
	return &painter{graphics: g}, nil
}

var (
	pGdipSetCompositingMode    = gdiplus.NewProc("GdipSetCompositingMode")
	pGdipSetCompositingQuality = gdiplus.NewProc("GdipSetCompositingQuality")
)

func (p *painter) Close() {
	if p.graphics != 0 {
		// GdipFlush 把 GDI+ 的绘制刷到位图内存。缺了它，紧接着的
		// UpdateLayeredWindow 可能读到还没写完的像素（表现为卡片透明）。
		pGdipFlush.Call(p.graphics, 1) // FlushIntentionSync
		pGdipDeleteGraphics.Call(p.graphics)
		p.graphics = 0
	}
}

var pGdipFlush = gdiplus.NewProc("GdipFlush")

// GDI+ 有两套矩形绘制函数：带 I 后缀的收整数，不带 I 的收浮点 REAL。
// 这里全部用整数版（I），传 int32 才不会把坐标读成无意义的小浮点。
var pGdipFillRectangleI = gdiplus.NewProc("GdipFillRectangleI")

// fillRound 填充圆角矩形。
//
// radius 为 0 时退化成直角矩形；radius 等于短边一半时是一个胶囊
// （两端各一个半圆 + 中间矩形）。
func (p *painter) fillRound(x, y, w, h, radius int32, c argb) {
	var brush uintptr
	if ret, _, _ := pGdipCreateSolidFill.Call(uintptr(c), uintptr(unsafe.Pointer(&brush))); ret != 0 {
		return
	}
	defer pGdipDeleteBrush.Call(brush)
	p.fillRoundWith(brush, x, y, w, h, radius)
}

// fillRoundWith 用已有画刷填圆角矩形。
//
// 分两种情况，都不依赖 path 的弧拼接（半径等于短边一半时 path 会退化）：
//   - 普通圆角：三个矩形 + 四个角圆
//   - 胶囊（radius >= 短边一半）：中间矩形 + 上下两个半圆
func (p *painter) fillRoundWith(brush uintptr, x, y, w, h, radius int32) {
	if w <= 0 || h <= 0 {
		return
	}
	short := w
	if h < short {
		short = h
	}
	if radius <= 0 {
		pGdipFillRectangleI.Call(p.graphics, brush,
			uintptr(x), uintptr(y), uintptr(w), uintptr(h))
		return
	}
	if radius*2 >= short {
		// 胶囊：上下各一个整圆 + 中间矩形
		r := short / 2
		pGdipFillRectangleI.Call(p.graphics, brush,
			uintptr(x), uintptr(y+r), uintptr(w), uintptr(h-r*2))
		d := r * 2
		pGdipFillEllipseI.Call(p.graphics, brush,
			uintptr(x), uintptr(y), uintptr(d), uintptr(d))
		pGdipFillEllipseI.Call(p.graphics, brush,
			uintptr(x), uintptr(y+h-d), uintptr(d), uintptr(d))
		if w > d {
			pGdipFillRectangleI.Call(p.graphics, brush,
				uintptr(x+r), uintptr(y), uintptr(w-d), uintptr(h))
		}
		return
	}

	// 普通圆角：中段通栏 + 两侧窄条 + 四角圆
	pGdipFillRectangleI.Call(p.graphics, brush,
		uintptr(x+radius), uintptr(y), uintptr(w-radius*2), uintptr(h))
	pGdipFillRectangleI.Call(p.graphics, brush,
		uintptr(x), uintptr(y+radius), uintptr(w), uintptr(h-radius*2))
	d := radius * 2
	pGdipFillEllipseI.Call(p.graphics, brush, uintptr(x), uintptr(y), uintptr(d), uintptr(d))
	pGdipFillEllipseI.Call(p.graphics, brush, uintptr(x+w-d), uintptr(y), uintptr(d), uintptr(d))
	pGdipFillEllipseI.Call(p.graphics, brush, uintptr(x), uintptr(y+h-d), uintptr(d), uintptr(d))
	pGdipFillEllipseI.Call(p.graphics, brush, uintptr(x+w-d), uintptr(y+h-d), uintptr(d), uintptr(d))
}

var pGdipDrawRectangleI = gdiplus.NewProc("GdipDrawRectangleI")

// strokeRound 圆角矩形描边。
func (p *painter) strokeRound(x, y, w, h, radius int32, c argb, width float32) {
	var pen uintptr
	if ret, _, _ := pGdipCreatePen1.Call(uintptr(c), uintptr(math.Float32bits(width)), 2, uintptr(unsafe.Pointer(&pen))); ret != 0 {
		return
	}
	defer pGdipDeletePen.Call(pen)

	if radius <= 0 {
		pGdipDrawRectangleI.Call(p.graphics, pen,
			uintptr(x), uintptr(y), uintptr(w), uintptr(h))
		return
	}
	half := w
	if h < half {
		half = h
	}
	if radius > half/2 {
		radius = half / 2
	}
	if radius <= 0 {
		pGdipDrawRectangleI.Call(p.graphics, pen,
			uintptr(x), uintptr(y), uintptr(w), uintptr(h))
		return
	}
	// 四条边 + 四个角弧（用椭圆描边近似角）
	pGdipDrawRectangleI.Call(p.graphics, pen,
		uintptr(x+radius), uintptr(y), uintptr(w-radius*2), uintptr(h))
	pGdipDrawRectangleI.Call(p.graphics, pen,
		uintptr(x), uintptr(y+radius), uintptr(w), uintptr(h-radius*2))
	d := radius * 2
	pGdipDrawEllipseI.Call(p.graphics, pen, uintptr(x), uintptr(y), uintptr(d), uintptr(d))
	pGdipDrawEllipseI.Call(p.graphics, pen, uintptr(x+w-d), uintptr(y), uintptr(d), uintptr(d))
	pGdipDrawEllipseI.Call(p.graphics, pen, uintptr(x), uintptr(y+h-d), uintptr(d), uintptr(d))
	pGdipDrawEllipseI.Call(p.graphics, pen, uintptr(x+w-d), uintptr(y+h-d), uintptr(d), uintptr(d))
}

var pGdipDrawPath = gdiplus.NewProc("GdipDrawPath")
var pGdipAddPathPieI = gdiplus.NewProc("GdipAddPathPie")

// fillSector 填充实心扇形（像饼图的一片），用于圆盘按用量填色。
//
// startDeg/sweepDeg 以 12 点方向为 0、顺时针为正；GDI+ 的 pie 以 3 点为 0，
// 所以这里减 90。整圆（sweep >= 359.9）退化成实心圆，避免 arc 的接缝。
func (p *painter) fillSector(cx, cy, radius int32, sweepDeg float64, c argb) {
	if sweepDeg >= 359.9 {
		p.fillCircle(cx, cy, radius, c)
		return
	}
	var brush uintptr
	if ret, _, _ := pGdipCreateSolidFill.Call(uintptr(c), uintptr(unsafe.Pointer(&brush))); ret != 0 {
		return
	}
	defer pGdipDeleteBrush.Call(brush)

	var path uintptr
	if ret, _, _ := pGdipCreatePath.Call(fillModeAlternate, uintptr(unsafe.Pointer(&path))); ret != 0 {
		return
	}
	defer pGdipDeletePath.Call(path)

	x := cx - radius
	y := cy - radius
	d := radius * 2
	pGdipAddPathPieI.Call(path,
		uintptr(x), uintptr(y), uintptr(d), uintptr(d),
		uintptr(math.Float32bits(float32(-90))),
		uintptr(math.Float32bits(float32(sweepDeg))))
	pGdipFillPath.Call(p.graphics, brush, path)
}

// arcGlow 画一段带透明度的粗弧，用作环的发光/外圈。
func (p *painter) arcGlow(cx, cy, radius int32, thickness float32, startDeg, sweepDeg float64, c argb, alpha uint8) {
	if sweepDeg <= 0 {
		return
	}
	a := uint32(c) >> 24
	if a == 0 {
		a = 255
	}
	// 用传入的 alpha 替换原色的 alpha
	col := argb(uint32(alpha)<<24 | uint32(c)&0x00FFFFFF)
	var pen uintptr
	if ret, _, _ := pGdipCreatePen1.Call(uintptr(col), uintptr(math.Float32bits(thickness)), 2, uintptr(unsafe.Pointer(&pen))); ret != 0 {
		return
	}
	defer pGdipDeletePen.Call(pen)
	pGdipSetPenLineCap197819.Call(pen, 0) // flat cap：发光线不要圆头
	pGdipSetPenEndCap.Call(pen, 0)
	r := radius - int32(thickness/2)
	x := cx - r
	y := cy - r
	d := r * 2
	pGdipDrawArcI.Call(p.graphics, pen,
		uintptr(x), uintptr(y), uintptr(d), uintptr(d),
		uintptr(math.Float32bits(float32(startDeg-90))),
		uintptr(math.Float32bits(float32(sweepDeg))))
}
// arcTrack 画整圈轨道。
//
// 用平头（flat cap）而不是圆头：整圈的起点与终点重合，圆头会在 12 点
// 位置叠出一个鼓包，看起来比别处粗。
func (p *painter) arcTrack(cx, cy, radius int32, thickness float32, c argb) {
	var pen uintptr
	if ret, _, _ := pGdipCreatePen1.Call(uintptr(c), uintptr(math.Float32bits(thickness)), 2, uintptr(unsafe.Pointer(&pen))); ret != 0 {
		return
	}
	defer pGdipDeletePen.Call(pen)
	pGdipSetPenLineCap197819.Call(pen, 0)
	pGdipSetPenEndCap.Call(pen, 0)
	r := radius - int32(thickness/2)
	pGdipDrawArcI.Call(p.graphics, pen,
		uintptr(cx-r), uintptr(cy-r), uintptr(r*2), uintptr(r*2),
		uintptr(math.Float32bits(-90)), uintptr(math.Float32bits(359.99)))
}

// arcRound 画进度弧，两端圆头（Pulse 的 StrokeStyle lineCap: .round）。
func (p *painter) arcRound(cx, cy, radius int32, thickness float32, startDeg, sweepDeg float64, c argb) {
	if sweepDeg <= 0 {
		return
	}
	var pen uintptr
	if ret, _, _ := pGdipCreatePen1.Call(uintptr(c), uintptr(math.Float32bits(thickness)), 2, uintptr(unsafe.Pointer(&pen))); ret != 0 {
		return
	}
	defer pGdipDeletePen.Call(pen)
	pGdipSetPenLineCap197819.Call(pen, lineCapRound)
	pGdipSetPenEndCap.Call(pen, lineCapRound)
	r := radius - int32(thickness/2)
	pGdipDrawArcI.Call(p.graphics, pen,
		uintptr(cx-r), uintptr(cy-r), uintptr(r*2), uintptr(r*2),
		uintptr(math.Float32bits(float32(startDeg-90))),
		uintptr(math.Float32bits(float32(sweepDeg))))
}

// arc 画圆环弧段。startDeg 从 12 点方向顺时针计算，sweepDeg 为扫过角度。
func (p *painter) arc(cx, cy, radius int32, thickness float32, startDeg, sweepDeg float64, c argb) {
	var pen uintptr
	// GDI+ Pen 的 Unit 参数：UnitPixel = 2
	if ret, _, _ := pGdipCreatePen1.Call(uintptr(c), uintptr(math.Float32bits(thickness)), 2, uintptr(unsafe.Pointer(&pen))); ret != 0 {
		return
	}
	defer pGdipDeletePen.Call(pen)
	pGdipSetPenLineCap197819.Call(pen, lineCapRound)
	pGdipSetPenEndCap.Call(pen, lineCapRound)
	r := radius - int32(thickness/2)
	// GDI+ 角度以 3 点方向为 0，顺时针为正；我们的 startDeg 以 12 点为 0
	gdiStart := startDeg - 90
	x := cx - r
	y := cy - r
	d := r * 2
	pGdipDrawArcI.Call(p.graphics, pen,
		uintptr(x), uintptr(y), uintptr(d), uintptr(d),
		uintptr(math.Float32bits(float32(gdiStart))),
		uintptr(math.Float32bits(float32(sweepDeg))))
}

// fillCircle 实心圆。
func (p *painter) fillCircle(cx, cy, radius int32, c argb) {
	var brush uintptr
	if ret, _, _ := pGdipCreateSolidFill.Call(uintptr(c), uintptr(unsafe.Pointer(&brush))); ret != 0 {
		return
	}
	defer pGdipDeleteBrush.Call(brush)
	x := cx - radius
	y := cy - radius
	d := radius * 2
	pGdipFillEllipseI.Call(p.graphics, brush, uintptr(x), uintptr(y), uintptr(d), uintptr(d))
}

// text 在指定矩形内绘制文字。align: 0=左 1=中 2=右。
func (p *painter) text(s string, x, y, w, h int32, size float32, bold bool, align int, c argb) {
	u16, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	familyName, _ := syscall.UTF16PtrFromString("Microsoft YaHei UI")
	var family uintptr
	if ret, _, _ := pGdipCreateFontFamilyFromName.Call(
		uintptr(unsafe.Pointer(familyName)), 0, uintptr(unsafe.Pointer(&family))); ret != 0 || family == 0 {
		familyName2, _ := syscall.UTF16PtrFromString("Segoe UI")
		pGdipCreateFontFamilyFromName.Call(uintptr(unsafe.Pointer(familyName2)), 0, uintptr(unsafe.Pointer(&family)))
		if family == 0 {
			return
		}
	}
	defer pGdipDeleteFontFamily.Call(family)

	style := 0
	if bold {
		style = 1 // FontStyleBold
	}
	// 字号以像素为单位：UnitPixel = 2
	var font uintptr
	if ret, _, _ := pGdipCreateFont.Call(family,
		uintptr(math.Float32bits(size)), uintptr(style), 2, uintptr(unsafe.Pointer(&font))); ret != 0 || font == 0 {
		return
	}
	defer pGdipDeleteFont.Call(font)

	var brush uintptr
	if ret, _, _ := pGdipCreateSolidFill.Call(uintptr(c), uintptr(unsafe.Pointer(&brush))); ret != 0 {
		return
	}
	defer pGdipDeleteBrush.Call(brush)

	var format uintptr
	pGdipCreateStringFormat.Call(0, 0, uintptr(unsafe.Pointer(&format)))
	if format != 0 {
		defer pGdipDeleteStringFormat.Call(format)
		pGdipSetStringFormatAlign.Call(format, uintptr(align))
		pGdipSetStringFormatLineAlign.Call(format, stringAlignmentNear)
		pGdipSetStringFormatFlags.Call(format, stringFormatNoWrap)
	}
	layout := rectF{float32(x), float32(y), float32(w), float32(h)}
	length := len(u16) - 1 // 去掉结尾 NUL
	pGdipDrawString.Call(p.graphics,
		uintptr(unsafe.Pointer(&u16[0])), uintptr(length), font,
		uintptr(unsafe.Pointer(&layout)), format, brush)
}
