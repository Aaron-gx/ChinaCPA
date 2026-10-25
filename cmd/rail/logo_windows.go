//go:build windows

package main

import (
	"math"
	"unsafe"
)

// Logo 圆盘内绘制的平台标记。
//
// 参考图里每个圆点是模型自己的图标。这里不引入位图资源，而是用
// 极简矢量字形在圆盘中心绘制：每个平台一个易辨认的符号。
//
// 之所以用字形而不是位图：
//   - 不增加二进制体积，也不依赖外部资源文件；
//   - 任意 DPI 下都清晰（直接按几何绘制）；
//   - 颜色跟随圆盘，看起来是同一个视觉体系的一部分。
type logoKind int

const (
	logoGeneric logoKind = iota
	logoQwen            // 六角星（通义千问的简洁标记）
	logoWorkBuddy       // 圆角方框 + 缺口（CodeBuddy 的对话气泡感）
	logoTrae            // 三角（Trae 的 acute 意象）
	logoQoder           // 同心环（Qoder 的 Q）
)

// logoFor 按平台 id 选标记。
func logoFor(provider string) logoKind {
	switch provider {
	case "qwen":
		return logoQwen
	case "workbuddy":
		return logoWorkBuddy
	case "trae":
		return logoTrae
	case "qoder", "qoder-intl":
		return logoQoder
	default:
		return logoGeneric
	}
}

// drawLogo 在 (cx,cy) 为中心、半径 r 的范围内绘制平台标记。
//
// 优先用嵌入的真实图标（各平台官方 app icon）；图标缺失时退回矢量字形，
// 这样新加平台不会因为没配图而空一块。
func (p *painter) drawLogo(kind logoKind, provider string, cx, cy, r int32, col argb) {
	if icon := loadIcon(provider); icon != nil && p.target != nil {
		// 图标画成直径 2r 的方块；圆盘本身是实心的，图标直接叠上去
		p.drawIcon(icon, cx, cy, r*2)
		return
	}
	if r < 6 {
		p.fillCircle(cx, cy, 2, col)
		return
	}
	switch kind {
	case logoQwen:
		drawStar6(p, cx, cy, r, col)
	case logoWorkBuddy:
		drawChatBubble(p, cx, cy, r, col)
	case logoTrae:
		drawTriangle(p, cx, cy, r, col)
	case logoQoder:
		drawRingMark(p, cx, cy, r, col)
	default:
		p.fillCircle(cx, cy, r/3, col)
	}
}

// drawStar6 六角星：两条重叠的等边三角形，是通义的常见意象。
func drawStar6(p *painter, cx, cy, r int32, col argb) {
	rr := float64(r)
	for _, rot := range []float64{0, 90} {
		pts := make([]pointF, 3)
		for i := 0; i < 3; i++ {
			ang := (float64(i)*120 + rot - 90) * math.Pi / 180
			pts[i] = pointF{
				float64(cx) + rr*math.Cos(ang),
				float64(cy) + rr*math.Sin(ang),
			}
		}
		p.fillPolygon(pts, col)
	}
}

// drawChatBubble 圆角方框右下方带一个小尾巴，像对话气泡。
func drawChatBubble(p *painter, cx, cy, r int32, col argb) {
	bw := r - 1
	bh := r - 3
	x := cx - bw
	y := cy - bh - 1
	p.fillRound(x, y, bw*2, bh*2, bh/2, col)
	// 尾巴用一个小三角
	p.fillPolygon([]pointF{
		{float64(cx + bw - 4), float64(cy + bh - 3)},
		{float64(cx + bw + 2), float64(cy + bh + 4)},
		{float64(cx + bw - 6), float64(cy + bh + 1)},
	}, col)
}

// drawTriangle 向上的等边三角（Trae 的首字母形意）。
func drawTriangle(p *painter, cx, cy, r int32, col argb) {
	rr := float64(r) - 0.5
	pts := make([]pointF, 3)
	for i := 0; i < 3; i++ {
		ang := (float64(i)*120 - 90) * math.Pi / 180
		pts[i] = pointF{
			float64(cx) + rr*math.Cos(ang),
			float64(cy) + rr*math.Sin(ang)*0.92,
		}
	}
	p.fillPolygon(pts, col)
}

// drawRingMark 同心环：外圈 + 中心点，取 Q 的环形意象。
func drawRingMark(p *painter, cx, cy, r int32, col argb) {
	// 用粗描边画一圈，再点中心
	p.arc(cx, cy, r, float32(r)/2.2, 0, 359.9, col)
	p.fillCircle(cx, cy, r/4, col)
}

// pointF 浮点坐标。
type pointF struct{ X, Y float64 }

// fillPolygon 填充多边形（用 GDI+ path 的直线段构造）。
func (p *painter) fillPolygon(pts []pointF, col argb) {
	if len(pts) < 3 {
		return
	}
	var brush uintptr
	if ret, _, _ := pGdipCreateSolidFill.Call(uintptr(col), uintptr(unsafe.Pointer(&brush))); ret != 0 {
		return
	}
	defer pGdipDeleteBrush.Call(brush)

	var path uintptr
	if ret, _, _ := pGdipCreatePath.Call(fillModeAlternate, uintptr(unsafe.Pointer(&path))); ret != 0 {
		return
	}
	defer pGdipDeletePath.Call(path)

	// 转成 GDI+ 需要的整数点数组（POINT）
	buf := make([]struct{ X, Y int32 }, len(pts))
	for i, pt := range pts {
		buf[i].X = int32(pt.X)
		buf[i].Y = int32(pt.Y)
	}
	pGdipAddPathPolygon.Call(path, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	pGdipFillPath.Call(p.graphics, brush, path)
}

var pGdipAddPathPolygon = gdiplus.NewProc("GdipAddPathPolygon")
