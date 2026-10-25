//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pCreateDIBSection  = gdi32.NewProc("CreateDIBSection")
	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pSelectObject2     = gdi32.NewProc("SelectObject")
	pDeleteObject2     = gdi32.NewProc("DeleteObject")
	pDeleteDC2         = gdi32.NewProc("DeleteDC")
	pGdiFlush          = gdi32.NewProc("GdiFlush")

	pUpdateLayeredWindow = user32.NewProc("UpdateLayeredWindow")
	pGetDCEx             = user32.NewProc("GetDC")
	pReleaseDCEx         = user32.NewProc("ReleaseDC")
	pGetWindowDC         = user32.NewProc("GetWindowDC")
)

// bitmapInfoHeader BITMAPINFOHEADER。
type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// bitmapInfo BITMAPINFO（只有 1 个调色板项）。
type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

const (
	dibRGBColors   = 0
	biRGB          = 0
	ulwAlpha       = 0x02
	acSrcOver      = 0x00
	acSrcAlpha     = 0x01
)

// dib 一块 32 位 top-down DIB section，用作分层窗口的绘制表面。
type dib struct {
	mem  uintptr
	dc   windows.Handle
	bmp  windows.Handle
	old  windows.Handle
	w, h int32
}

// newDIB 创建宽 w 高 h 的 32 位位图表面。
func newDIB(w, h int32) (*dib, error) {
	dc, _, _ := pCreateCompatibleDC.Call(0)
	if dc == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC 失败")
	}
	bi := bitmapInfo{}
	bi.Header.Size = uint32(unsafe.Sizeof(bitmapInfoHeader{}))
	bi.Header.Width = w
	bi.Header.Height = -h // 负数 = top-down，行序与绘制坐标一致
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = biRGB

	var mem uintptr
	bmp, _, _ := pCreateDIBSection.Call(dc,
		uintptr(unsafe.Pointer(&bi)), dibRGBColors,
		uintptr(unsafe.Pointer(&mem)), 0, 0)
	if bmp == 0 || mem == 0 {
		pDeleteDC2.Call(dc)
		return nil, fmt.Errorf("CreateDIBSection 失败")
	}
	old, _, _ := pSelectObject2.Call(dc, bmp)
	return &dib{
		mem: mem,
		dc:  windows.Handle(dc),
		bmp: windows.Handle(bmp),
		old: windows.Handle(old),
		w:   w, h: h,
	}, nil
}

// clear 把整块表面清成全透明。
//
// 预乘 alpha 下"透明"就是全 0；GDI+ 画上来的像素会自己带 alpha，
// 所以圆角外保持 0 就能让桌面透出来。
func (d *dib) clear() {
	n := int(d.w) * int(d.h)
	p := (*[1 << 26]uint32)(unsafe.Pointer(d.mem))[:n:n]
	for i := range p {
		p[i] = 0
	}
}

// commit 用 UpdateLayeredWindow 把整块位图提交到窗口。
func (d *dib) commit(hwnd windows.Handle) error {
	// 确保 GDI+ 的绘制已经落到位图内存里
	pGdiFlush.Call()

	// dst 必须是屏幕坐标；用窗口当前矩形左上角
	var wr rect
	getWindowRect(hwnd, &wr)

	src := point{0, 0}
	size := sizeL{int32(d.w), int32(d.h)}
	dst := point{wr.Left, wr.Top}
	blend := blendFunction{
		BlendOp:             acSrcOver,
		BlendFlags:          0,
		SourceConstantAlpha: 255, // 全程不透明；透明度由每个像素的 alpha 决定
		AlphaFormat:         acSrcAlpha,
	}
	// 屏幕 DC：UpdateLayeredWindow 要求一个参照 DC
	screenDC, _, _ := pGetDCEx.Call(0)
	if screenDC == 0 {
		return fmt.Errorf("GetDC 失败")
	}
	defer pReleaseDCEx.Call(0, screenDC)

	ret, _, callErr := pUpdateLayeredWindow.Call(
		uintptr(hwnd),
		screenDC,
		uintptr(unsafe.Pointer(&dst)),
		uintptr(unsafe.Pointer(&size)),
		uintptr(d.dc),
		uintptr(unsafe.Pointer(&src)),
		0,
		uintptr(unsafe.Pointer(&blend)),
		ulwAlpha,
	)
	if ret == 0 {
		return fmt.Errorf("UpdateLayeredWindow 失败: %v", callErr)
	}
	return nil
}

// Close 释放位图与 DC。
func (d *dib) Close() {
	if d.dc != 0 {
		pSelectObject2.Call(uintptr(d.dc), uintptr(d.old))
		pDeleteDC2.Call(uintptr(d.dc))
		d.dc = 0
	}
	if d.bmp != 0 {
		pDeleteObject2.Call(uintptr(d.bmp))
		d.bmp = 0
	}
}

// sizeL SIZE 结构（用 int32 以免和 windows.SIZE 冲突）。
type sizeL struct{ Cx, Cy int32 }

// blendFunction BLENDFUNCTION。
type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}
