//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// 毛玻璃材质。
//
// Pulse 在 macOS 26 上用 Liquid Glass（系统级材质，自动模糊背后的窗口）。
// Windows 的对位措施是 acrylic/blur —— 用 SetWindowCompositionAttribute
// 给分层窗口挂一个模糊背景，rail 本体画成半透明，让背景的模糊透出来。
//
// 注意：这只在 Windows 10 1803+（build 17134）可用，且分层窗口需要配合
// 正确的 paint。拿不到时安全退回深灰实底，不影响功能。

var (
	pSetWindowCompositionAttribute = user32.NewProc("SetWindowCompositionAttribute")

	// 探测是否支持，缓存结果
	glassSupported    bool
	glassChecked      bool
)

const (
	wcaAccentPolicy = 19 // WCA_ACCENT_POLICY

	// ACCENT 状态
	accentDisabled           = 0
	accentEnableBlurBehind   = 1
	accentEnableAcrylicBlur  = 4

	// 绘制标志：默认
	drawLeftBorder   = 0x20
	drawTopBorder    = 0x40
	drawRightBorder  = 0x80
	drawBottomBorder = 0x100
)

// accentPolicy 系统毛玻璃策略。
type accentPolicy struct {
	AccentState  uint32
	AccentFlags  uint32
	GradientColor uint32 // ABGR
	AnimationID  uint32
}

type windowCompositionAttribData struct {
	Attrib uint32
	Data   unsafe.Pointer
	Size   uint32
}

// claimGlass 尝试给窗口挂 acrylic 毛玻璃。
//
// 返回 true 表示成功挂上（调用方应把 rail 底色改半透明让模糊透出来）。
func claimGlass(hwnd windows.Handle) bool {
	if !glassChecked {
		glassChecked = true
		glassSupported = pSetWindowCompositionAttribute.Find() == nil
	}
	if !glassSupported {
		return false
	}

	// acrylic：状态 4 + 一个深色渐变（0xAARRGGBB，给个近黑让字看得清）。
	// opacity 0xD0 ~ 82%，配 30% 的纯度。
	policy := accentPolicy{
		AccentState:  accentEnableAcrylicBlur,
		AccentFlags:  drawLeftBorder | drawTopBorder | drawRightBorder | drawBottomBorder,
		GradientColor: 0xCC1E1E1F, // 82% 不透明的近黑
	}
	data := windowCompositionAttribData{
		Attrib: wcaAccentPolicy,
		Data:   unsafe.Pointer(&policy),
		Size:   uint32(unsafe.Sizeof(policy)),
	}
	ret, _, _ := pSetWindowCompositionAttribute.Call(
		uintptr(hwnd), uintptr(unsafe.Pointer(&data)))
	return ret != 0
}