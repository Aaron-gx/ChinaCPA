//go:build windows

package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
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
)

// 菜单项 id
const (
	cmdOpenPanel = 1
	cmdRefresh   = 2
	cmdQuit      = 3
)

// showMenu 右键菜单：打开面板 / 立即刷新 / 退出。
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
	case cmdQuit:
		pDestroyWindow.Call(uintptr(r.hwnd))
	}
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
