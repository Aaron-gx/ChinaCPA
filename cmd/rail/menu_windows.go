//go:build windows

package main

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	pGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	pGlobalLock       = kernel32.NewProc("GlobalLock")
	pGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	pOpenClipboard    = user32.NewProc("OpenClipboard")
	pCloseClipboard   = user32.NewProc("CloseClipboard")
	pEmptyClipboard   = user32.NewProc("EmptyClipboard")
	pSetClipboardData = user32.NewProc("SetClipboardData")

	pCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	pAppendMenuW         = user32.NewProc("AppendMenuW")
	pTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	pDestroyMenu         = user32.NewProc("DestroyMenu")
	pGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pPostMessageAll      = user32.NewProc("PostMessageW")

	shellExecuteW = shell32.NewProc("ShellExecuteW")
)

const (
	mfString  = 0x00000000
	mfSeparator = 0x00000800
	tpmLeftAlign  = 0x0000
	tpmReturnCmd  = 0x0100
	tpmNonotify   = 0x0080

	gmemMoveable  = 0x0002
	cfUnicodeText = 13
)

// 菜单项 id
const (
	cmdOpenPanel = 1
	cmdRefresh   = 2
	cmdCopyAPI   = 3
	cmdQuit      = 4
)

// showMenu 右键菜单：打开面板 / 立即刷新 / 复制地址 / 退出。
func (r *Rail) showMenu() {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)

	add := func(id uintptr, text string) {
		p, _ := windows.UTF16PtrFromString(text)
		pAppendMenuW.Call(menu, mfString, id, uintptr(unsafe.Pointer(p)))
	}
	add(cmdOpenPanel, "打开管理面板")
	add(cmdRefresh, "立即刷新")
	apiURL := strings.TrimRight(r.opts.BaseURL, "/") + "/v1"
	add(cmdCopyAPI, "复制接口地址 ("+apiURL+")")
	pAppendMenuW.Call(menu, mfSeparator, 0, 0)
	add(cmdQuit, "退出浮动栏")

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// 菜单需要窗口在前台，否则点外面不会消失
	hwndFg, _, _ := pGetForegroundWindow.Call()
	pSetForegroundWindow.Call(uintptr(r.hwnd))
	cmd, _, _ := pTrackPopupMenu.Call(menu,
		tpmLeftAlign|tpmReturnCmd|tpmNonotify,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(r.hwnd), 0)
	pSetForegroundWindow.Call(hwndFg)

	switch cmd {
	case cmdOpenPanel:
		openURL(r.opts.BaseURL)
	case cmdRefresh:
		if r.onRefresh != nil {
			r.onRefresh()
		}
	case cmdCopyAPI:
		setClipboardText(r.hwnd, apiURL)
	case cmdQuit:
		pDestroyWindow.Call(uintptr(r.hwnd))
	}
}

// setClipboardText 将文本置入系统剪贴板（纯 Win32 API 实现，零外部依赖）。
func setClipboardText(hwnd windows.Handle, text string) bool {
	u16, err := windows.UTF16FromString(text)
	if err != nil {
		return false
	}
	bytes := len(u16) * 2
	hMem, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(bytes))
	if hMem == 0 {
		return false
	}
	ptr, _, _ := pGlobalLock.Call(hMem)
	if ptr == 0 {
		return false
	}
	dest := (*[1 << 20]uint16)(unsafe.Pointer(ptr))[:len(u16):len(u16)]
	copy(dest, u16)
	pGlobalUnlock.Call(hMem)

	ret, _, _ := pOpenClipboard.Call(uintptr(hwnd))
	if ret == 0 {
		return false
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	res, _, _ := pSetClipboardData.Call(cfUnicodeText, hMem)
	return res != 0
}

// openURL 用默认浏览器打开地址。
func openURL(u string) {
	op, _ := windows.UTF16PtrFromString("open")
	target, _ := windows.UTF16PtrFromString(u)
	shellExecuteW.Call(0,
		uintptr(unsafe.Pointer(op)),
		uintptr(unsafe.Pointer(target)),
		0, 0, 1 /* SW_SHOWNORMAL */)
}

var _ = os.Exit
