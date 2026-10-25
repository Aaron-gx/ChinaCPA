//go:build windows

package main

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/png"
	"math"
)

// 平台 logo，编译期打进二进制。
//
// 这些是各平台的应用图标（512~640px 方形 PNG，带透明通道）。
// 打进二进制而不是运行时读文件，是为了让 rail.exe 保持单文件可分发的特性。
//
//go:embed logos/*.png
var logoFS embed.FS

// iconImage 一张已解码的图标，预乘 alpha，可直接按像素写入 DIB。
type iconImage struct {
	w, h int32
	px   []uint32 // BGRA，预乘 alpha
}

var iconCache = map[string]*iconImage{}

// loadIcon 按平台 id 载入图标（带缓存）。
func loadIcon(provider string) *iconImage {
	if c, ok := iconCache[provider]; ok {
		return c
	}
	name := provider
	switch provider {
	case "qoder-intl":
		name = "qoder"
	}
	raw, err := logoFS.ReadFile("logos/" + name + ".png")
	if err != nil {
		iconCache[provider] = nil
		return nil
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		iconCache[provider] = nil
		return nil
	}
	icon := toIcon(img)
	iconCache[provider] = icon
	return icon
}

// toIcon 把 image.Image 转成预乘 alpha 的 BGRA。
//
// GDI+ 画到 DIB 时按预乘 alpha 解释像素，所以这里直接把颜色乘上 alpha，
// 避免半透明边缘发白。
//
// 同时把方形图标裁成圆形：这些 app icon 多为满边方块，直接贴在彩色
// 圆盘上会像糊了张方纸；裁成圆形后融进圆盘，视觉上是一个整体的"币"。
func toIcon(src image.Image) *iconImage {
	b := src.Bounds()
	w, h := int32(b.Dx()), int32(b.Dy())
	if w <= 0 || h <= 0 {
		return nil
	}
	out := &iconImage{w: w, h: h, px: make([]uint32, w*h)}
	cx := float64(w) / 2
	cy := float64(h) / 2
	// 内切圆半径，留 1px 做抗锯齿过渡
	radius := math.Min(cx, cy) - 0.5
	for y := int32(0); y < h; y++ {
		for x := int32(0); x < w; x++ {
			r, g, bl, a := src.At(b.Min.X+int(x), b.Min.Y+int(y)).RGBA()
			a8 := float64(a >> 8)

			// 圆形遮罩：边缘 1.2px 内平滑过渡，避免硬锯齿
			dx := float64(x) + 0.5 - cx
			dy := float64(y) + 0.5 - cy
			dist := math.Sqrt(dx*dx + dy*dy)
			var mask float64
			switch {
			case dist <= radius-1.2:
				mask = 1
			case dist >= radius+0.2:
				mask = 0
			default:
				mask = (radius + 0.2 - dist) / 1.4
			}
			if mask <= 0 {
				out.px[y*w+x] = 0
				continue
			}

			a8 *= mask
			if a8 <= 0.5 {
				out.px[y*w+x] = 0
				continue
			}
			af := a8 / 255
			r8 := uint32(float64(r>>8) * af)
			g8 := uint32(float64(g>>8) * af)
			b8 := uint32(float64(bl>>8) * af)
			out.px[y*w+x] = uint32(a8)<<24 | r8<<16 | g8<<8 | b8
		}
	}
	return out
}

// drawIcon 把图标按目标边长缩放到 (cx,cy) 为中心的位置。
//
// 用双线性重采样，因为图标要缩到 ~20px：最近邻会明显锯齿，
// 而更高质量的重采样在这个尺寸上不值得。
func (p *painter) drawIcon(icon *iconImage, cx, cy, size int32) {
	p.drawIconAlpha(icon, cx, cy, size, 0xFF)
}

// drawIconAlpha 同 drawIcon，但整体乘以 alpha（0=全透明，255=原样）。
//
// 用于"这个平台还没有读数"的状态：Pulse 把图标压到 35% 不透明度，
// 让人一眼看出哪些平台真的拿到了数据。
func (p *painter) drawIconAlpha(icon *iconImage, cx, cy, size int32, alpha uint8) {
	if icon == nil || size <= 0 || alpha == 0 {
		return
	}
	scaled := resampleIcon(icon, size)
	if scaled == nil {
		return
	}
	if alpha < 0xFF {
		f := uint32(alpha)
		for i, v := range scaled.px {
			a := v >> 24 & 0xFF
			if a == 0 {
				continue
			}
			na := a * f / 255
			r := (v >> 16 & 0xFF) * f / 255
			g := (v >> 8 & 0xFF) * f / 255
			b := (v & 0xFF) * f / 255
			scaled.px[i] = na<<24 | r<<16 | g<<8 | b
		}
	}
	p.blitBGRA(scaled, cx-size/2, cy-size/2)
}

// resampleIcon 双线性缩放图标到 size×size。
func resampleIcon(src *iconImage, size int32) *iconImage {
	if src == nil || size <= 0 {
		return nil
	}
	dst := &iconImage{w: size, h: size, px: make([]uint32, size*size)}
	scaleX := float64(src.w) / float64(size)
	scaleY := float64(src.h) / float64(size)
	for y := int32(0); y < size; y++ {
		for x := int32(0); x < size; x++ {
			// 采样中心
			sx := (float64(x)+0.5)*scaleX - 0.5
			sy := (float64(y)+0.5)*scaleY - 0.5
			dst.px[y*size+x] = bilinear(src, sx, sy)
		}
	}
	return dst
}

// bilinear 在源图上做双线性采样，返回预乘 BGRA。
func bilinear(src *iconImage, fx, fy float64) uint32 {
	x0 := int32(fx)
	y0 := int32(fy)
	if fx < 0 {
		x0 = -1
	}
	if fy < 0 {
		y0 = -1
	}
	x1, y1 := x0+1, y0+1
	dx := float32(fx - float64(x0))
	dy := float32(fy - float64(y0))

	get := func(x, y int32) (a, r, g, b float32) {
		if x < 0 || y < 0 || x >= src.w || y >= src.h {
			return 0, 0, 0, 0
		}
		v := src.px[y*src.w+x]
		return float32(v>>24&0xFF) / 255,
			float32(v>>16&0xFF) / 255,
			float32(v>>8&0xFF) / 255,
			float32(v&0xFF) / 255
	}
	a00, r00, g00, b00 := get(x0, y0)
	a10, r10, g10, b10 := get(x1, y0)
	a01, r01, g01, b01 := get(x0, y1)
	a11, r11, g11, b11 := get(x1, y1)

	mix := func(v00, v10, v01, v11 float32) float32 {
		top := v00 + (v10-v00)*dx
		bot := v01 + (v11-v01)*dx
		return top + (bot-top)*dy
	}
	a := mix(a00, a10, a01, a11)
	r := mix(r00, r10, r01, r11)
	g := mix(g00, g10, g01, g11)
	b := mix(b00, b10, b01, b11)
	to8 := func(v float32) uint32 {
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		return uint32(v*255 + 0.5)
	}
	return to8(a)<<24 | to8(r)<<16 | to8(g)<<8 | to8(b)
}

// blitBGRA 把预乘 BGRA 图块贴到当前绘制的 DIB 表面。
//
// painter 需要知道目标表面地址：由 paint() 在创建 painter 后设置。
//
// 贴图前先 GdipFlush：图标是 CPU 直接写 DIB 内存，而之前的圆盘/文字走
// GDI+，两者混用时必须把 GDI+ 的绘制先落到内存，否则图标会被随后的
// flush 覆盖掉。
func (p *painter) blitBGRA(img *iconImage, dx, dy int32) {
	if p.target == nil || img == nil {
		return
	}
	if p.graphics != 0 {
		pGdipFlush.Call(p.graphics, 1) // FlushIntentionSync
	}
	tw, th := p.targetW, p.targetH
	buf := (*[1 << 26]uint32)(p.target)[: tw*th : tw*th]
	for y := int32(0); y < img.h; y++ {
		ty := dy + y
		if ty < 0 || ty >= th {
			continue
		}
		for x := int32(0); x < img.w; x++ {
			tx := dx + x
			if tx < 0 || tx >= tw {
				continue
			}
			s := img.px[y*img.w+x]
			sa := s >> 24
			if sa == 0 {
				continue
			}
			if sa == 255 {
				buf[ty*tw+tx] = s
				continue
			}
			// 源覆盖：预乘 alpha 的 over 运算
			d := buf[ty*tw+tx]
			inv := 255 - sa
			nr := (s>>16&0xFF) + (d>>16&0xFF)*inv/255
			ng := (s>>8&0xFF) + (d>>8&0xFF)*inv/255
			nb := (s&0xFF) + (d&0xFF)*inv/255
			na := sa + (d>>24)*inv/255
			buf[ty*tw+tx] = na<<24 | nr<<16 | ng<<8 | nb
		}
	}
}

// iconDebug 用于确认嵌入是否成功。
func iconDebug() string {
	names := []string{"qwen", "workbuddy", "trae", "qoder"}
	var out string
	for _, n := range names {
		if ic := loadIcon(n); ic != nil {
			out += fmt.Sprintf("%s:%dx%d ", n, ic.w, ic.h)
		} else {
			out += n + ":MISSING "
		}
	}
	return out
}

