//go:build windows

package main

import (
	"log"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// debugWnd 由环境变量 CCPA_RAIL_DEBUG 打开，用于排查窗口消息。
var debugWnd = os.Getenv("CCPA_RAIL_DEBUG") != ""

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pShowWindow       = user32.NewProc("ShowWindow")
	pUpdateWindow     = user32.NewProc("UpdateWindow")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	pBeginPaint       = user32.NewProc("BeginPaint")
	pEndPaint         = user32.NewProc("EndPaint")
	pGetClientRect    = user32.NewProc("GetClientRect")
	pFillRect         = user32.NewProc("FillRect")
	pInvalidateRect   = user32.NewProc("InvalidateRect")
	pSetTimer         = user32.NewProc("SetTimer")
	pKillTimer        = user32.NewProc("KillTimer")
	pGetCursorPos     = user32.NewProc("GetCursorPos")
	pLoadCursorW      = user32.NewProc("LoadCursorW")
	pSetWindowPos     = user32.NewProc("SetWindowPos")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	pMonitorFromPoint = user32.NewProc("MonitorFromPoint")
	pGetMonitorInfoW  = user32.NewProc("GetMonitorInfoW")
	pScreenToClient   = user32.NewProc("ScreenToClient")
	pSetCapture       = user32.NewProc("SetCapture")
	pReleaseCapture   = user32.NewProc("ReleaseCapture")
	pTrackMouseEvent  = user32.NewProc("TrackMouseEvent")
	pGetStockObject   = gdi32.NewProc("GetStockObject")
	pCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	pDeleteObject     = gdi32.NewProc("DeleteObject")
	pSelectObject     = gdi32.NewProc("SelectObject")
	pRoundRect        = gdi32.NewProc("RoundRect")
	pEllipse          = gdi32.NewProc("Ellipse")
	pCreatePen        = gdi32.NewProc("CreatePen")
	pSetBkMode        = gdi32.NewProc("SetBkMode")
	pSetTextColor     = gdi32.NewProc("SetTextColor")
	pCreateFontW      = gdi32.NewProc("CreateFontW")
	pTextOutW         = gdi32.NewProc("TextOutW")
	pGetTextExtentPoint32W = gdi32.NewProc("GetTextExtentPoint32W")
	pSetLayeredWindowAttributes2 = user32.NewProc("SetLayeredWindowAttributes")
	pGetDC            = user32.NewProc("GetDC")
	pReleaseDC        = user32.NewProc("ReleaseDC")
)

const (
	wsPopup       = 0x80000000
	wsExLayered   = 0x00080000
	wsExTopmost   = 0x00000008
	wsExToolwindow = 0x00000080
	wsExTransparent = 0x00000020 // 点击穿透
	wsExNoActivate = 0x08000000

	swShowNoActivate = 4
	lwaAlpha        = 0x00000002
	swpNoActivate   = 0x0010
	swpNoZorder     = 0x0004
	swpNoSize       = 0x0001

	wmDestroy  = 0x0002
	wmPaint    = 0x000F
	wmTimer    = 0x0113
	wmMouseMove = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp = 0x0205
	wmMouseLeave = 0x02A3
	wmNCHitTest   = 0x0084
	htTransparent = 0xFFFFFFFE // -1 as DWORD
	htClient      = 1
	wmMouseHover = 0x02A1
	wmApp      = 0x8000

	smCxScreen = 0
	smCyScreen = 1

	monitorDefaultToNearest = 2
	transparentBk = 1
	psSolid       = 0
	bsSolid       = 0
	dtCenter      = 0x0001
	dtVCenter     = 0x0004
	dtSingleLine  = 0x0020

	// 自定义消息：后台取到新数据后通知 UI 线程重绘
	wmUpdateData = wmApp + 1
	// 重绘请求（WM_PAINT 是保留消息，PostMessage 投它会被丢弃）
	wmRepaint    = wmApp + 3
	// 收起定时器 id
	timerCollapse = 1
	timerWatchdog = 2
	timerAnim     = 3
	timerSliver    = 4
)

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

type msg struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

type trackMouseEvent struct {
	Size      uint32
	Flags     uint32
	TrackHWnd windows.Handle
	HoverTime uint32
}

// Rail 浮动栏窗口。
type Rail struct {
	hwnd     windows.Handle
	opts     Options
	metrics  []Metric
	dib      *dib // 缓存的内存位图表面，尺寸未变时跨帧复用，避免频繁创建/销毁 GDI 对象
	edge     string // 吸附边：left/right/top/bottom
	isVertical bool // 竖排（贴左/右）还是横排（贴上/下）
	expanded bool
	sliver   bool // 闲置时收成细条
	sliverArmed bool // 已启动7f29细条计时（防止每次重设定时间器）
	hovered  bool
	width    int32
	height   int32
	// 通信：后台 goroutine 把抓到的数据投进 channel，UI 线程消费
	incoming chan []Metric
	// 回调
	onMenu func()
	onRefresh func()
	// 拖动
	dragging  bool
	dragFrom  point
	dragStart point
	origin    point
	userPlaced bool // 用户拖过，之后不再自动吸边
	commitErrLogged bool
	hoverIndex int // 悬停/展开的圆环索引
	leavePending bool // 已收到 MOUSELEAVE，等定时器确认

	// ── 动画状态 ──
	// 每个环一个动画对象；有动画在跑时由 60fps 定时器逐帧推进。
	// 都在 UI 线程访问，不需要锁。
	anims []*ringAnim
	// 上次数据快照，用于检测变化并触发动画
	lastUsed []float64
	lastClock []float64
	lastBusy []bool
	lastRefreshAt int64 // 最近一次刷新开始时刻
	animTickerActive bool
	animElapsed float64 // 动画时钟（秒，单调）
	// 通知去重：记录上次通知时各环的已用比例，避免每帧都弹
	notifiedAt []float64
}

// NewRail 创建浮动栏窗口。必须在 UI 线程调用（LockOSThread 之后）。
func NewRail(opts Options) (*Rail, error) {
	globalRail = new(Rail)
	r := globalRail
	if opts.Edge == "" {
		opts.Edge = "right"
	}
	*r = Rail{
		opts:       opts,
		edge:       opts.Edge,
		isVertical: opts.Edge == "left" || opts.Edge == "right",
		expanded:   false, // 收起态起步：只显示胶囊，悬停才弹出详情卡
		hoverIndex: 0,
		incoming:   make(chan []Metric, 4),
	}
	if err := r.registerClass(); err != nil {
		return nil, err
	}
	if err := r.createWindow(); err != nil {
		return nil, err
	}
	r.applyPlacement()
	return r, nil
}

func (r *Rail) registerClass() error {
	cls, err := windows.UTF16PtrFromString("ChinaCPARail")
	if err != nil {
		return err
	}
	cursor, _, _ := pLoadCursorW.Call(0, 32512) // IDC_ARROW
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		Style:     0x0008, // CS_DBLCLKS 支持双击直接打开面板
		WndProc:   syscall.NewCallback(railWndProc),
		ClassName: cls,
		Cursor:    windows.Handle(cursor),
	}
	atom, _, callErr := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		return callErr
	}
	return nil
}

func (r *Rail) createWindow() error {
	// 先按当前屏幕的 DPI 与高度定好缩放，再建窗口 ——
	// 否则窗口会用默认尺寸创建，首帧就画错大小。
	dpi := systemDPI()
	screenH := primaryScreenHeight()
	setUIScale(dpi, screenH)
	if debugWnd {
		log.Printf("DPI=%d screenH=%d uiScale=%.3f", dpi, screenH, uiScale)
	}

	cls, _ := windows.UTF16PtrFromString("ChinaCPARail")
	title, _ := windows.UTF16PtrFromString("ChinaCPA Rail")
	exStyle := uint32(wsExLayered | wsExToolwindow)
	if r.opts.AlwaysOnTop {
		exStyle |= wsExTopmost
	}
	r.width, r.height = railWidthPillF(), pd(100)
	hwnd, _, callErr := pCreateWindowExW.Call(
		uintptr(exStyle), uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)),
		uintptr(wsPopup),
		0, 0, uintptr(r.width), uintptr(r.height),
		0, 0, 0, 0,
	)
	if hwnd == 0 {
		return callErr
	}
	r.hwnd = windows.Handle(hwnd)
	// 注意：这里不能调 SetLayeredWindowAttributes。分层窗口只有两种模式，
	// 一旦用了 LWA_ALPHA（整窗透明度），UpdateLayeredWindow 的 per-pixel
	// alpha 就失效，位图会显示不出来。
	setRailInstance(r.hwnd, r)
	// 常驻轮询：每 200ms 看一次光标是否还在窗口上。
	// 不依赖 WM_MOUSELEAVE：那个消息可靠性差（窗口被其他窗口遮住、
	// 切换桌面、屏幕锁屏时都不一定来），而这个窗口
	// 必须在指针离开时收起来。
	pSetTimer.Call(uintptr(r.hwnd), timerWatchdog, 200, 0)
	pShowWindow.Call(hwnd, swShowNoActivate)
	pUpdateWindow.Call(hwnd)
	return nil
}

// 用全局 map 把 hwnd 映射回 Rail，避免依赖 GWLP_USERDATA 的时序
var railInstances = map[windows.Handle]*Rail{}

func setRailInstance(h windows.Handle, r *Rail) { railInstances[h] = r }
func railFor(h windows.Handle) *Rail           { return railInstances[h] }

// railWndProc 窗口过程。只做分发，逻辑在方法里。
func railWndProc(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	r := railFor(hwnd)
	switch msg {
	case wmPaint:
		// 分层窗口下 WM_PAINT 不承载绘制（见 requestPaint），但仍需 BeginPaint/EndPaint
		// 清掉无效区域，否则系统会不断重投。
		var ps paintStruct
		hdc, _, _ := pBeginPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		if hdc != 0 {
			pEndPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		}
		if r != nil {
			r.paint()
		}
		return 0
	case wmRepaint:
		if r != nil {
			r.paint()
		}
		return 0
	case wmTimer:
		if r != nil {
			switch wParam {
			case timerCollapse:
				pKillTimer.Call(uintptr(hwnd), timerCollapse)
				r.collapseNow()
			case timerWatchdog:
				r.watchdog()
			case timerAnim:
				r.tickAnim()
			case timerSliver:
				pKillTimer.Call(uintptr(hwnd), timerSliver)
				r.collapseToSliver()
			default:
				r.drainUpdates()
			}
		}
		return 0
	case wmMouseMove:
		if r != nil {
			// lParam 低 16 位是 x，高 16 位是 y（客户区坐标）
			r.onMouseMove(int32(lParam&0xFFFF), int32((lParam>>16)&0xFFFF))
		} else if debugWnd {
			log.Printf("WM_MOUSEMOVE: railFor(%v)=nil", hwnd)
		}
		return 0
	case wmMouseLeave:
		if r != nil {
			if debugWnd {
				log.Printf("WM_MOUSELEAVE")
			}
			r.onMouseLeave()
		}
		return 0
	case wmLButtonDown:
		if r != nil {
			r.onLDown()
		}
		return 0
	case wmLButtonUp:
		if r != nil {
			r.onLUp()
		}
		return 0
	case wmLButtonDblClk:
		if r != nil {
			openURL(r.opts.BaseURL)
		}
		return 0
	case wmRButtonUp:
		if r != nil {
			r.showMenu()
		}
		return 0
	case wmUpdateData:
		if r != nil {
			r.drainUpdates()
		}
		return 0
	case wmNCHitTest:
		// 透明区域点击穿透：窗口很宽（含卡片位置），
		// 若不判断就会把桌面上一大块透明区挡住。
		if r != nil {
			// lParam 是屏幕坐标，转成客户区
			pt := point{int32(lParam & 0xFFFF), int32((lParam >> 16) & 0xFFFF)}
			pScreenToClient.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pt)))
			if r.hitOpaque(pt.X, pt.Y) {
				ret, _, _ := pDefWindowProcW.Call(uintptr(hwnd), wmNCHitTest, wParam, lParam)
				return ret
			}
			return uintptr(htTransparent)
		}
		return uintptr(htTransparent)
	case wmDestroy:
		if r != nil && r.dib != nil {
			r.dib.Close()
			r.dib = nil
		}
		delete(railInstances, hwnd)
		pPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := pDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return ret
}

// Run 消息循环，阻塞直到窗口关闭。调用方需 LockOSThread。
func (r *Rail) Run() {
	var m msg
	for {
		ret, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// Push 由后台 goroutine 调用，投递最新指标。
func (r *Rail) Push(m []Metric) {
	select {
	case r.incoming <- m:
	default:
		// 上一批还没画完，丢掉这次，避免堆积
	}
	r.requestPaint()
}

// Seed 立即设置首帧数据（构造后、Run 前调用，避免首帧画出"未连接"）。
func (r *Rail) Seed(m []Metric) {
	r.metrics = m
	r.fitHeight()
	r.applyPlacement()
}

// requestPaint 让 UI 线程在下一轮消息循环里重绘。
//
// 用分层窗口（UpdateLayeredWindow）后 WM_PAINT 不再自动到来，而且
// PostMessage(WM_PAINT) 会被系统丢弃（它是保留消息）。所以投一个自定义
// 消息，由窗口过程显式调用 paint。
func (r *Rail) requestPaint() {
	pPostMessageW.Call(uintptr(r.hwnd), wmRepaint, 0, 0)
}

var pPostMessageW = user32.NewProc("PostMessageW")

// fitHeight 计算窗口尺寸。
//
// 窗口固定为「rail + 卡片」的最大尺寸，展开只改绘制内容：
// 收起时左侧画成全透明。这样悬停/切换环都不会改变窗口几何，
// 指针始终在窗口内，不会因为窗口收缩触发 WM_MOUSELEAVE（早前卡片
// 一闪就没的根因）。
func (r *Rail) fitHeight() bool {
	n := len(r.metrics)
	if n > 8 {
		n = 8
	}

	// 细条：6px 宽、96px 高（Pulse 的 collapsedWidth/Height）
	if r.sliver {
		wantW := int32(pd(6)) + 8
		wantH := int32(pd(96))
		if wantW == r.width && wantH == r.height {
			return false
		}
		r.width, r.height = wantW, wantH
		return true
	}

	var pillH int32
	if n == 0 {
		pillH = railWidthPillF() + padTopF()*2
	} else {
		pillH = int32(n)*slotHeightF() - itemSpacingF() + padTopF()*2
	}
	if pillH < 100 {
		pillH = 100
	}

	// 卡片最大高度按行数最多的平台预估
	maxRows := 1
	for _, m := range r.metrics {
		if len(m.Rows) > maxRows {
			maxRows = len(m.Rows)
		}
	}
	if maxRows > 4 {
		maxRows = 4
	}
	titleH := int32(cardTitleFontF()) + 8
	rowH := int32(cardRowLineF())*2 + cardRowSpacingF()*2 + cardBarHeightF() + cardContentSpacingF()
	cardH := int32(cardPadF())*2 + titleH + cardContentSpacingF() + int32(maxRows)*rowH + 16

	wantH := pillH
	if cardH > wantH {
		wantH = cardH
	}
	wantW := int32(cardReachF()) + 12 // 右边缘留一点余量，避免裁到 rail 圆角

	if wantW == r.width && wantH == r.height {
		return false
	}
	r.width, r.height = wantW, wantH
	return true
}

// drainUpdates 取出最新一批数据并重绘。
func (r *Rail) drainUpdates() {
	updated := false
	for {
		select {
		case m := <-r.incoming:
			r.metrics = m
			updated = true
		default:
			if updated {
				r.syncAnims(r.animElapsed)
				r.maybeAlert()
				r.ensureAnimTicker()
				if r.fitHeight() {
					r.applyPlacement()
				} else {
					r.requestPaint()
				}
			}
			return
		}
	}
}

func (r *Rail) onMouseMove(x, y int32) {
	if r.dragging {
		var cur point
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&cur)))
		dx := cur.X - r.dragFrom.X
		dy := cur.Y - r.dragFrom.Y
		pSetWindowPos.Call(uintptr(r.hwnd), 0,
			uintptr(r.dragStart.X+dx), uintptr(r.dragStart.Y+dy),
			0, 0, swpNoZorder|swpNoSize|swpNoActivate)
		return
	}

	// 有移动就撤销待收起状态（指针回来了）
	if r.leavePending {
		r.leavePending = false
		pKillTimer.Call(uintptr(r.hwnd), timerCollapse)
	}

	// 指针进入：取消收细条的计时；如果已经是细条，立刻展开
	r.cancelSliver()
	if r.sliver {
		r.expandFromSliver()
	}

	// 判断指针落在哪个圆环上，决定详情卡显示哪个平台
	idx := int(r.hitTestAt(x, y))
	changed := idx != r.hoverIndex
	r.hoverIndex = idx

	// 订阅离开事件（每次进入都要重新订阅一次）
	if !r.hovered {
		tme := trackMouseEvent{
			Size:      uint32(unsafe.Sizeof(trackMouseEvent{})),
			Flags:     0x00000002, // TME_LEAVE
			TrackHWnd: r.hwnd,
		}
		pTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
		r.expandNow()
		r.paintNow()
		return
	}
	if changed {
		// 换了一个圆环：卡片内容要换，但窗口尺寸不变，只需重绘
		r.requestPaint()
		r.paintNow()
	}
}

// hitTestAt 判断客户区坐标 (x,y) 落在哪个环上。
//
// 用消息自带坐标而不是 GetCursorPos：真实悬停时两者一致，
// 但用消息坐标让自动化测试也走同一条路径。
func (r *Rail) hitTestAt(x, y int32) int32 {
	L := r.computeLayout(r.width, r.height)
	cx := (L.pill.Left + L.pill.Right) / 2
	// 命中半径略大于环，容忍贴边时指针压线
	hitR := int32(ringDiameterF()/2) + 4
	for i, cy := range L.slots {
		dx := x - cx
		dy := y - cy
		if dx*dx+dy*dy <= hitR*hitR {
			return int32(i)
		}
	}
	return r.bestIndex()
}

// bestIndex 保持当前有效选择，否则回落到第一个。
func (r *Rail) bestIndex() int32 {
	if r.hoverIndex >= 0 && r.hoverIndex < len(r.metrics) {
		return int32(r.hoverIndex)
	}
	if len(r.metrics) > 0 {
		return 0
	}
	return -1
}

// onMouseLeave 指针移出窗口。
//
// 不立刻收起，而是投一个定时器延迟再收：指针在胶囊与卡片之间移动、
// 或快速划过圆环间隙时，系统可能短暂报告"离开"，延迟能吃掉这些抖动。
// 真正的离开会在延迟到点后收起。
func (r *Rail) onMouseLeave() {
	if r.dragging || !r.hovered {
		return
	}
	r.leavePending = true
	// 800ms：足够覆盖"从胶囊移到卡片"这类正常移动，又不会让收起显得迟钝
	pSetTimer.Call(uintptr(r.hwnd), timerCollapse, 800, 0)
}

// collapseNow 真正收起（由收起定时器调用）。
func (r *Rail) collapseNow() {
	if r.dragging {
		return
	}
	// 收起前再确认一次：指针可能已经回来了（返回 true 表示还在窗口内）
	if r.cursorInside() {
		r.leavePending = false
		return
	}
	r.leavePending = false
	r.hovered = false
	r.setExpanded(false)
	// 悬停光晕淡出
	r.setHalo(0)
}

// cursorInside 用系统光标位置判断是否在窗口矩形内。
func (r *Rail) cursorInside() bool {
	var cur point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&cur)))
	var wr rect
	getWindowRect(r.hwnd, &wr)
	return cur.X >= wr.Left && cur.X < wr.Right && cur.Y >= wr.Top && cur.Y < wr.Bottom
}

// expandNow 展开当前悬停的环。
func (r *Rail) expandNow() {
	if r.hovered && r.expanded {
		return
	}
	r.hovered = true
	r.expanded = true
	// 悬停光晕淡入（当前悬停的环）
	r.setHalo(1)
	r.requestPaint()
}

// paintNow 同步绘制一次。
//
// 只由窗口过程在 UI 线程里调用，用于需要"立刻看到结果"的场合
// （悬停展开、切换圆环）。异步的 requestPaint 留给后台数据更新。
func (r *Rail) paintNow() {
	r.paint()
}

func (r *Rail) onLDown() {
	r.dragging = true
	var cur point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&cur)))
	r.dragFrom = cur
	var wr rect
	getWindowRect(r.hwnd, &wr)
	r.dragStart = point{wr.Left, wr.Top}
	pSetCapture.Call(uintptr(r.hwnd))
}

func (r *Rail) onLUp() {
	if r.dragging {
		r.dragging = false
		pReleaseCapture.Call()
		r.userPlaced = true
		r.applyPlacement()
	}
}

// setExpanded 切换展开/收起。窗口尺寸固定不变，只改绘制内容，
// 因为收缩窗口会让指针"离开"窗口，触发 WM_MOUSELEAVE，卡片随即消失。
func (r *Rail) setExpanded(exp bool) {
	if r.expanded == exp {
		return
	}
	r.expanded = exp
	r.paintNow()
}

// applyPlacement 把窗口吸附到主屏右边缘、垂直居中。
func (r *Rail) applyPlacement() {
	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	mon, _, _ := pMonitorFromPoint.Call(uintptr(int32(pt.X)), uintptr(int32(pt.Y)), monitorDefaultToNearest)
	var mi monitorInfo
	mi.Size = uint32(unsafe.Sizeof(mi))
	if mon != 0 {
		pGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
	}
	work := mi.Work
	if work.Right == 0 {
		// 拿不到监视器信息时退回主屏尺寸
		cx, _, _ := pGetSystemMetrics.Call(smCxScreen)
		cy, _, _ := pGetSystemMetrics.Call(smCyScreen)
		work = rect{0, 0, int32(cx), int32(cy)}
	}
	var wr rect
	getWindowRect(r.hwnd, &wr)

	// 按吸附边定位。竖排（左/右）：rail 贴边、垂直居中；
	// 横排（上/下）：rail 贴边、水平居中。
	var x, y int32
	w, h := r.width, r.height
	switch r.edge {
	case "left":
		x = work.Left
		y = (work.Top+work.Bottom)/2 - h/2
	case "top":
		x = (work.Left+work.Right)/2 - w/2
		y = work.Top
	case "bottom":
		x = (work.Left+work.Right)/2 - w/2
		y = work.Bottom - h
	default: // right
		x = work.Right - w
		y = (work.Top+work.Bottom)/2 - h/2
	}

	if r.userPlaced {
		x = wr.Left
		y = wr.Top
		// 夹回可视区，避免拖到屏幕外再也点不到
		if x < work.Left {
			x = work.Left
		}
		if x+w > work.Right {
			x = work.Right - w
		}
		if y < work.Top {
			y = work.Top
		}
		if y+h > work.Bottom {
			y = work.Bottom - h
		}
	}
	r.origin = point{x, y}
	pSetWindowPos.Call(uintptr(r.hwnd), 0, uintptr(x), uintptr(y),
		uintptr(r.width), uintptr(r.height), swpNoZorder|swpNoActivate)
	r.requestPaint()
}

func getWindowRect(h windows.Handle, r *rect) {
	pGetWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(r)))
}

var pGetWindowRect = user32.NewProc("GetWindowRect")

// systemDPI 返回主屏的 DPI（96 = 100% 缩放）。
//
// 用 GetDpiForSystem：它在进程 DPI 感知模式下返回系统 DPI，
// 比 GetDeviceCaps 稳，也不会因为窗口还没创建而失败。
func systemDPI() int32 {
	if pGetDpiForSystem.Find() == nil {
		if v, _, _ := pGetDpiForSystem.Call(); v != 0 {
			return int32(v)
		}
	}
	// 退回 GDI：96 是基准值
	dc, _, _ := pGetDC.Call(0)
	if dc == 0 {
		return 96
	}
	defer pReleaseDC.Call(0, dc)
	// LOGPIXELSY = 90
	v, _, _ := pGetDeviceCaps.Call(dc, 90)
	if v == 0 {
		return 96
	}
	return int32(v)
}

// primaryScreenHeight 主屏的物理高度（像素）。
func primaryScreenHeight() int32 {
	h, _, _ := pGetSystemMetrics.Call(smCyScreen)
	if h == 0 {
		return 1080
	}
	return int32(h)
}

var (
	pGetDpiForSystem = user32.NewProc("GetDpiForSystem")
	pGetDeviceCaps   = gdi32.NewProc("GetDeviceCaps")
)

// hitOpaque 判断客户区某点是否落在"看得见的东西"上。
//
// 分层窗口是一整块矩形，但只有 rail（和展开时的卡片+指针）真的画了内容。
// 其余部分是透明像素，必须让点击穿到下面的窗口 —— 否则一条 64px 宽的
// rail 会在屏幕上占掉 380px 宽的点击区域，完全没法用。
func (r *Rail) hitOpaque(x, y int32) bool {
	L := r.computeLayout(r.width, r.height)

	// rail 本体：整个竖直条带都算（含两端圆角外的极小误差，不值得精确）
	if x >= L.pill.Left && x <= L.pill.Right &&
		y >= L.pill.Top && y <= L.pill.Bottom {
		return true
	}

	// 展开时的卡片与指针
	if r.expanded && L.card.Right > L.card.Left {
		if x >= L.card.Left && x <= L.card.Right &&
			y >= L.card.Top && y <= L.card.Bottom {
			return true
		}
		if x >= L.ptr.Left && x <= L.ptr.Right &&
			y >= L.ptr.Top && y <= L.ptr.Bottom {
			return true
		}
	}
	return false
}

// watchdog 每 200ms 检查一次光标是否还在窗口的可见区域上。
//
// 为什么不只用 WM_MOUSELEAVE：那个消息在这几种情况下会漏 —— 窗口被
// 其他窗口遮住时指针"移出"不算离开、切换虚拟桌面、显示器休眠唤醒。
// 漏一次卡片就永久挂在屏幕上（用户报的问题），而光标位置是权威事实。
//
// 只做收起，不做展开：展开仍由 WM_MOUSEMOVE 驱动，避免轮询把指针
// 恰好停在 rail 上时每秒重绘五次。
func (r *Rail) watchdog() {
	if r.dragging {
		return
	}
	// 细条已经是最终态，不需要再收
	if r.sliver {
		return
	}
	// 常态（未展开）也要检查：光标不在 rail 上就启动收细条计时

	var cur point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&cur)))
	var wr rect
	getWindowRect(r.hwnd, &wr)

	// 指针在窗口外 → 先收起（不再展开），并启动「闲置收成细条」的计时。
	//
	// Pulse 的 sliver：鼠标离开一段时间后整条 rail 折成一条 6px 宽的色线，
	// 只在需要的时候展开。这是它不占屏幕的关键，也正好解决"占显示空间"。
	if cur.X < wr.Left || cur.X >= wr.Right || cur.Y < wr.Top || cur.Y >= wr.Bottom {
		r.hovered = false
		r.leavePending = false
		if r.expanded {
			r.setExpanded(false)
		}
		r.armSliver()
		return
	}

	// 指针在窗口内但落在透明区 → 同样收起并准备收细条
	var pt = cur
	pScreenToClient.Call(uintptr(r.hwnd), uintptr(unsafe.Pointer(&pt)))
	if !r.hitOpaque(pt.X, pt.Y) {
		r.hovered = false
		r.leavePending = false
		if r.expanded {
			r.setExpanded(false)
		}
		r.armSliver()
	}
}

// armSliver 启动「闲置收成细条」的 2.5 秒计时。
// 指针回来会取消（见 onMouseMove 的 cancelSliver）。
func (r *Rail) armSliver() {
	if r.sliver || r.sliverArmed {
		return
	}
	r.sliverArmed = true
	pSetTimer.Call(uintptr(r.hwnd), timerSliver, 2500, 0)
}

// cancelSliver 指针回到窗口时取消收细条。
func (r *Rail) cancelSliver() {
	if r.sliver {
		return
	}
	r.sliverArmed = false
	pKillTimer.Call(uintptr(r.hwnd), timerSliver)
}

// collapseToSliver 由闲置计时触发：收成细条。
func (r *Rail) collapseToSliver() {
	if r.dragging || r.hovered {
		return
	}
	r.sliver = true
	r.fitHeight()
	r.applyPlacement()
	r.requestPaint()
}

// expandFromSliver 指针碰到细条：立刻展开成完整 rail。
func (r *Rail) expandFromSliver() {
	if !r.sliver {
		return
	}
	r.sliver = false
	r.hovered = true
	r.expanded = true
	r.fitHeight()
	r.applyPlacement()
	r.setHalo(1)
	r.requestPaint()
}

// ── 动画驱动 ─────────────────────────────

// animFor 取第 i 个环的动画对象，懒创建。
func (r *Rail) animFor(i int) *ringAnim {
	for len(r.anims) <= i {
		a := newRingAnim(0)
		r.anims = append(r.anims, a)
	}
	a := r.anims[i]
	// 确保目标值与当前环一致（初次或数据变化时由 syncAnims 处理）
	if i < len(r.metrics) {
		m := r.metrics[i]
		target := m.Used
		if m.Balance {
			target = 1
		}
		if m.Spent {
			target = 1
		}
		a.arc.set(target)
		if m.ClockFrac >= 0 {
			a.clock.set(m.ClockFrac)
		}
	}
	return a
}

// syncAnims 在新数据到达时更新所有环的动画目标，并触发扫描/活动弧。
func (r *Rail) syncAnims(now float64) {
	n := len(r.metrics)
	for len(r.anims) < n {
		r.anims = append(r.anims, newRingAnim(0))
	}

	// 首次：直接把动画跳到当前值（不播放入场动画）
	if r.lastUsed == nil {
		r.lastUsed = make([]float64, n)
		r.lastClock = make([]float64, n)
		r.lastBusy = make([]bool, n)
		r.notifiedAt = make([]float64, n)
		for i, m := range r.metrics {
			r.anims[i].arc.set(targetOf(m))
			r.anims[i].arc.cur = targetOf(m)
			if m.ClockFrac >= 0 {
				r.anims[i].clock.set(m.ClockFrac)
				r.anims[i].clock.cur = m.ClockFrac
			}
			r.lastUsed[i] = targetOf(m)
			r.lastClock[i] = m.ClockFrac
			r.lastBusy[i] = m.Busy
		}
		return
	}

	// 增量：只有数值变了才动
	for i, m := range r.metrics {
		if i >= n {
			break
		}
		t := targetOf(m)
		if t != r.lastUsed[i] {
			r.anims[i].arc.set(t)
			r.lastUsed[i] = t
		}
		if m.ClockFrac != r.lastClock[i] {
			if m.ClockFrac >= 0 {
				r.anims[i].clock.set(m.ClockFrac)
			}
			r.lastClock[i] = m.ClockFrac
		}
		// 活动状态：从假到真触发白色弧
		if m.Busy && !r.lastBusy[i] {
			r.anims[i].busy.start(now)
			r.anims[i].busyFade.set(1)
		}
		if !m.Busy && r.lastBusy[i] {
			r.anims[i].busy.stop()
			r.anims[i].busyFade.set(0)
		}
		r.lastBusy[i] = m.Busy
	}
}

// targetOf 一个环的弧目标比例。
func targetOf(m Metric) float64 {
	if m.Empty {
		return 0
	}
	if m.Spent {
		return 1
	}
	if m.Balance {
		return 1
	}
	return clamp01(m.Used)
}

// ensureAnimTicker 有动画需要推进时启动 60fps 定时器。
func (r *Rail) ensureAnimTicker() {
	if r.animTickerActive {
		return
	}
	// 先看有没有需要动的
	need := false
	for i := range r.anims {
		if i < len(r.metrics) && r.anims[i].busyNow() {
			need = true
			break
		}
	}
	if !need {
		return
	}
	r.animTickerActive = true
	// 60fps：16ms 一跳
	pSetTimer.Call(uintptr(r.hwnd), timerAnim, 16, 0)
}

// tickAnim 由 WM_TIMER(timerAnim) 驱动，推进一帧。
func (r *Rail) tickAnim() {
	dt := 1.0 / 60.0
	r.animElapsed += dt

	still := false
	for i := range r.anims {
		a := r.anims[i]
		if a == nil {
			continue
		}
		if a.arc.step(dt) {
			still = true
		}
		if a.clock.step(dt) {
			still = true
		}
		if a.refresh.active && a.refresh.step(dt) {
			still = true
		}
		if a.refreshFade.step(dt) {
			still = true
		}
		if a.busy.active && a.busy.step(dt) {
			still = true
		}
		if a.busyFade.step(dt) {
			still = true
		}
		if a.halo.step(dt) {
			still = true
		}
	}

	if !still {
		// 全部到点：停掉定时器，画最后一帧
		pKillTimer.Call(uintptr(r.hwnd), timerAnim)
		r.animTickerActive = false
	}
	r.requestPaint()
}

// setHalo 设置当前悬停环的光晕强度（0~1），并确保动画定时器在跑。
func (r *Rail) setHalo(v float64) {
	if r.hoverIndex < 0 || r.hoverIndex >= len(r.anims) {
		return
	}
	a := r.anims[r.hoverIndex]
	if a == nil {
		return
	}
	a.halo.set(v)
	r.ensureAnimTicker()
	r.requestPaint()
}
