//go:build windows

package main

import (
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 托盘气泡通知。用 Shell_NotifyIcon 的 NIF_INFO 弹一条标准 Windows 通知。
//
// 这是最接近 macOS 通知中心的做法，且零 UI 依赖 —— 系统自己画气泡。
// Pulse 用 UserNotifications 框架；这里是它的对位实现。

const (
	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	// 图标：用应用的 exe 图标（IDI_APPLICATION 是个文档图标，能显示就行）
	// 这里用一个 ICO 资源里的图标 id；没有自定义图标就用系统的。
	iconApp = 32512

	// 托盘图标 id（随便一个非零值）
	notifyID = 1

	nimSetVersion = 0x00000004
	notifyVersion = 3

	// 气泡超时：短一点，避免遮挡
	notifyTimeout = 4000
)

// notifyIconData NOTIFYICONDATA（v6 结构）。
type notifyIconData struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	Flags            uint32
	CallbackMessage  uint32
	Icon             windows.Handle
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GuidItem         [16]byte
	BalloonIcon      windows.Handle
}

var (
	shellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	// 通知去重：同一个环在接近上限后只弹一次，直到它显著变化
	lastAlertKey string
	lastAlertFrac float64
)

// showTrayNotify 弹一条系统气泡。
func showTrayNotify(title, body string) {
	hwnd := notificationOwnerHWND()
	if hwnd == 0 {
		return
	}
	n := notifyIconData{}
	n.CbSize = uint32(unsafe.Sizeof(n))
	n.HWnd = hwnd
	n.UID = notifyID
	n.Flags = nifMessage | nifTip | nifInfo
	n.CallbackMessage = wmApp + 50
	n.Icon = windows.Handle(iconApp)
	copyU16(n.Tip[:], "ChinaCPA Rail")
	copyU16(n.InfoTitle[:], title)
	copyU16(n.Info[:], body)
	n.TimeoutOrVersion = notifyTimeout

	// 添加托盘图标（首次）
	shellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&n)))
	// 改气泡内容
	n.Flags = nifInfo
	shellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

// notificationOwnerHWND 找通知的宿主窗口。
//
// 用当前 rail 的窗口（如果还在），否则系统托盘会自己弹。
func notificationOwnerHWND() windows.Handle {
	if r := currentRail(); r != nil {
		return r.hwnd
	}
	return 0
}

// 全局指向当前实例，供通知模块找窗口
var globalRail *Rail

// currentRail 返回当前 rail 实例。
func currentRail() *Rail { return globalRail }

// copyU16 把 UTF-8 字符串写进 UTF-16 缓冲。
func copyU16(dst []uint16, s string) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	for i, c := range u {
		if i >= len(dst) {
			break
		}
		dst[i] = c
	}
}

// maybeAlert 检查各环是否接近上限，触发通知。
//
// 只在跨过 75% 阈值且和前一次通知相比有实质变化时弹，避免刷屏。
func (r *Rail) maybeAlert() {
	if len(r.notifiedAt) != len(r.metrics) {
		return
	}
	const warnAt = 0.75
	for i, m := range r.metrics {
		if m.Empty || m.Balance {
			continue
		}
		frac := targetOf(m)
		if frac < warnAt {
			continue
		}
		// 已通知过且没明显变化（±5%）就不再弹
		last := r.notifiedAt[i]
		if last >= warnAt && frac-last < 0.05 && (last-frac) < 0.05 {
			continue
		}
		title := m.Full + " 额度告警"
		body := m.Text + " 已用，" + fmtPct(frac*100) + " 接近上限"
		if m.NoBalance {
			// 零余额：不是"接近"，是已经没了
			body = "额度已耗尽，请充值或切换账号"
		}
		showTrayNotify(title, body)
		r.notifiedAt[i] = frac
	}
}

// ── 登录令牌临期/过期提醒 ─────────────────────────────
//
// 面板 TOKEN 列里剩 2.1 天 / 0.0 天这种状态，只有打开网页才看得到；
// 这里把它推到系统托盘，登录一过期就提醒去续。

// tokenAlertMemo 记录已弹过的提醒：状态 + 上次弹出时刻。
type tokenAlertMemo struct {
	state string
	at    time.Time
}

// tokenMemo 只在 UI 线程（drainUpdates → maybeTokenAlert）访问，无需锁。
var tokenMemo = map[string]tokenAlertMemo{}

// tokenRealertAfter 同一状态下的重复提醒间隔：弹一次没处理，
// 隔天再提醒一次；处理完（重新登录成功）状态归零，memo 自动清掉。
const tokenRealertAfter = 24 * time.Hour

// maybeTokenAlert 检查令牌提醒清单，合并成一条托盘通知。
//
// 多个账号同时临期只弹一条汇总，避免刷屏；状态升级
// （soon → expired）会立即再弹，不等 24 小时。
func (r *Rail) maybeTokenAlert() {
	if len(r.alerts) == 0 {
		return
	}
	var fresh []TokenAlert
	for _, a := range r.alerts {
		if a.State == "" {
			delete(tokenMemo, a.Key) // 恢复正常：清 memo，下次临期重新提醒
			continue
		}
		m, seen := tokenMemo[a.Key]
		if seen && m.state == a.State && time.Since(m.at) < tokenRealertAfter {
			continue
		}
		fresh = append(fresh, a)
	}
	if len(fresh) == 0 {
		return
	}
	title, body := tokenAlertText(fresh)
	showTrayNotify(title, body)
	now := time.Now()
	for _, a := range fresh {
		tokenMemo[a.Key] = tokenAlertMemo{state: a.State, at: now}
	}
}

// tokenAlertText 把本轮要提醒的账号拼成通知标题与正文。
func tokenAlertText(alerts []TokenAlert) (title, body string) {
	var dead, soon []TokenAlert
	for _, a := range alerts {
		if a.State == "soon" {
			soon = append(soon, a)
		} else {
			dead = append(dead, a) // expired / relogin
		}
	}
	switch {
	case len(dead) > 0 && len(soon) > 0:
		title = "登录令牌提醒"
		body = namesOf(dead) + " 已过期/失效；" + namesOf(soon) + " 即将过期"
	case len(dead) > 0:
		title = "登录令牌已过期"
		body = namesOf(dead) + " 已过期/失效，请重新登录续期"
	default:
		title = "登录令牌即将过期"
		var parts []string
		for _, a := range soon {
			parts = append(parts, a.Who+"（剩 "+fmtDay(a.Days)+"）")
		}
		body = strings.Join(parts, "、") + "，建议尽快重新登录"
	}
	return title, clipNotify(body, 250)
}

// namesOf 拼账号名，超过 3 个收拢成「等 N 个」，避免正文过长。
func namesOf(list []TokenAlert) string {
	names := make([]string, 0, 4)
	for i, a := range list {
		if i == 3 {
			break
		}
		names = append(names, a.Who)
	}
	joined := strings.Join(names, "、")
	if len(list) > 3 {
		joined += fmt.Sprintf(" 等 %d 个账号", len(list))
	}
	return joined
}

// fmtDay 剩余天数：不足一天显示小时。
func fmtDay(d float64) string {
	if d < 1 {
		return fmt.Sprintf("%.0f 小时", d*24)
	}
	return fmt.Sprintf("%.1f 天", d)
}

// clipNotify 截断正文，避免撑爆 NOTIFYICONDATA 的 UTF-16 缓冲。
func clipNotify(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}