//go:build windows

package main

import (
	"golang.org/x/sys/windows"
)

// 单实例锁。
//
// 用命名互斥体而不是查进程列表：互斥体在进程退出时由内核释放，
// 崩溃也不会留下僵尸锁。名字带 Global\ 前缀会在整个会话生效。
//
// 多实例的后果不只是"多一条 rail"：每个实例各自维护悬停状态，
// 一个收起时另一个还展开着，看起来就像卡片一直挂在屏幕上。
var instanceMutex windows.Handle

// acquireSingleInstance 拿锁，已有实例在跑时返回 false。
func acquireSingleInstance() bool {
	name, err := windows.UTF16PtrFromString(`Local\ChinaCPA.Rail.Instance`)
	if err != nil {
		return true // 拿不到名字就别挡着用户
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		// ERROR_ALREADY_EXISTS 时 CreateMutex 仍然返回一个有效句柄，
		// 只是 GetLastError 会报告已存在；x/sys 把它变成 err。
		if err == windows.ERROR_ALREADY_EXISTS {
			return false
		}
		return true
	}
	// 检查是否真的新建成功：已存在的互斥体句柄有效但所有权不属于我们
	if err == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(h)
		return false
	}
	instanceMutex = h
	return true
}
