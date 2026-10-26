//go:build windows

package main

import "math"

// 动画引擎。
//
// Pulse 用 SwiftUI 的隐式动画和 Core Animation；这里没有那些东西，
// 所以自己维护一个逐帧推进的状态：
//
//   - 每个数值有一个"当前值"和一个"目标值"，每帧朝目标靠近。
//   - 两种插值：弹簧（进度弧，对应 Pulse 的 spring(response:0.5, damping:0.85)）
//     和线性（旋转扫描时间）。
//   - 由 UI 线程的 60fps 定时器驱动，只在有动画进行时才重绘。
//
// 一切都在 UI 线程内访问，不需要锁。

// springValue 一个用弹簧逼近目标的标量。
//
// 用半隐式欧拉积分：v' += (target-cur)*k*dt - v*damp*dt，
// cur += v*dt。参数按 Pulse 的 response=0.5 / damping=0.85 折算。
type springValue struct {
	cur    float64
	target float64
	vel    float64
}

const (
	// 弹簧刚度与阻尼。response 0.5s 对应约 12 的刚度（ω≈2π/0.5），
	// damping 0.85 表示欠阻尼，会轻微过冲 —— 这是 Pulse 的手感。
	springK    = 148.0
	springDamp = 16.8
	// 停止阈值：每秒位移小于 0.0005 且速度极小时吸附到目标
	springEps = 0.0005
)

func newSpring(v float64) *springValue {
	return &springValue{cur: v, target: v}
}

// set 设定新目标。
func (s *springValue) set(v float64) {
	s.target = v
}

// step 推进一帧，返回是否还在移动。
func (s *springValue) step(dt float64) bool {
	if dt <= 0 || dt > 0.1 {
		// 时间跳变（首帧、窗口被挂起）时直接吸附，避免爆掉
		s.cur, s.vel = s.target, 0
		return false
	}
	diff := s.target - s.cur
	s.vel += (diff*springK - s.vel*springDamp) * dt
	s.cur += s.vel * dt

	if math.Abs(s.target-s.cur) < springEps && math.Abs(s.vel) < springEps*10 {
		s.cur, s.vel = s.target, 0
		return false
	}
	return true
}

// spin 一个匀速旋转的相位，用于扫描弧。
type spin struct {
	phase    float64 // 0~1
	period   float64 // 转一圈的秒数
	active   bool
	holdAt   float64 // 停止后停留的角度（便于淡出）
	startedAt float64
}

func (s *spin) start(elapsed float64) {
	if s.active {
		return
	}
	s.active = true
	s.phase = 0
	s.startedAt = elapsed
}

func (s *spin) stop() {
	s.active = false
	s.phase = 0
}

// step 推进相位，返回是否还在转。
func (s *spin) step(dt float64) bool {
	if !s.active || s.period <= 0 {
		return false
	}
	s.phase += dt / s.period
	if s.phase >= 1 {
		s.phase -= math.Floor(s.phase)
	}
	return true
}

// fade 一个 0~1 的淡入淡出值，用于扫描弧的出现/消失。
type fade struct {
	cur    float64
	target float64
	speed  float64 // 每秒变化量
}

func newFade(v, speed float64) *fade {
	return &fade{cur: v, target: v, speed: speed}
}

func (f *fade) set(v float64) { f.target = v }

// step 推进一帧，返回是否还在变化。
func (f *fade) step(dt float64) bool {
	if f.cur == f.target {
		return false
	}
	d := f.target - f.cur
	step := f.speed * dt
	if math.Abs(d) <= step {
		f.cur = f.target
		return false
	}
	if d > 0 {
		f.cur += step
	} else {
		f.cur -= step
	}
	return true
}

// ringAnim 一个环的全部动画状态。
type ringAnim struct {
	// arc 进度弧的当前比例（弹性）
	arc *springValue
	// refresh 刷新扫描（拉数据时一圈彩色短弧）
	refresh *spin
	refreshFade *fade
	// busy CLI 活动标记（白色短弧在环内侧）
	busy *spin
	busyFade *fade
	// clock 窗口时钟弧的比例（弹性）
	clock *springValue
	// halo 悬停发光强度（淡入淡出）
	halo *fade
}

func newRingAnim(arc float64) *ringAnim {
	return &ringAnim{
		arc:         newSpring(arc),
		refresh:     &spin{period: 0.85},
		refreshFade: newFade(0, 6),
		busy:        &spin{period: 1.0},
		busyFade:    newFade(0, 6),
		clock:       newSpring(0),
		halo:        newFade(0, 5),
	}
}

// busy 判断这个环是否有动画在进行。
func (a *ringAnim) busyNow() bool {
	if a == nil {
		return false
	}
	return a.refresh.active || a.refreshFade.cur != a.refreshFade.target ||
		a.busy.active || a.busyFade.cur != a.busyFade.target ||
		a.halo.cur != a.halo.target ||
		!springAt(a.arc) ||
		!springAt(a.clock)
}

func springAt(s *springValue) bool {
	return math.Abs(s.target-s.cur) < 1e-9 && math.Abs(s.vel) < 1e-9
}
