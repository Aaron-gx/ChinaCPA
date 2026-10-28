#![windows_subsystem = "windows"]

use windows::{
    core::*,
    Win32::Foundation::*,
    Win32::Graphics::Gdi::*,
    Win32::System::LibraryLoader::*,
    Win32::System::Threading::*,
    Win32::System::DataExchange::*,
    Win32::System::Memory::*,
    Win32::UI::Input::KeyboardAndMouse::*,
    Win32::UI::WindowsAndMessaging::*,
};

use std::cell::RefCell;
use std::sync::mpsc::{channel, Receiver};

mod client;
mod graphics;
mod metrics;
mod placement;
mod render;
mod spring;

use client::spawn_network_worker;
use graphics::D2DEngine;
use metrics::{demo_metrics, Metric};
use placement::{load_placement, monitor_geom, save_placement, Dock, Edge, Placement};
use render::{render_frame, RingAnimState, LayoutMetrics};
use spring::SpringValue;

const TIMER_ANIM: usize = 1;
const TIMER_WATCHDOG: usize = 2;

const WM_APP_DATA: u32 = WM_USER + 101;
const WM_MOUSELEAVE_MSG: u32 = 0x02A3;

thread_local! {
    static STATE: RefCell<Option<AppState>> = RefCell::new(None);
}

pub(crate) fn log_msg(msg: &str) {
    use std::io::Write;
    let path = if let Ok(mut exe) = std::env::current_exe() {
        exe.set_file_name("rail.log");
        exe
    } else {
        std::path::PathBuf::from("rail.log")
    };
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(path) {
        let _ = writeln!(f, "{}", msg);
    }
}

/// 单调毫秒时钟（状态机延迟与防误触用）
fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

pub struct AppState {
    pub hwnd: HWND,
    pub engine: D2DEngine,
    pub metrics: Vec<Metric>,
    pub anims: Vec<RingAnimState>,
    pub hover_index: Option<usize>,

    // Pulse 标志性物理动力学状态
    pub openness: SpringValue,       // 0.0 (细条) -> 1.0 (展开胶囊)
    pub card_openness: SpringValue,  // 0.0 (收起) -> 1.0 (展开详情卡片)
    pub pointer_y: SpringValue,      // 水滴三角形平滑追踪悬停槽位 Y 轴
    pub busy_spin: f32,              // 动态旋转角度

    pub width: f32,
    pub height: f32,
    pub scale: f32,
    pub base_url: String,
    pub password: String,
    pub rx: Receiver<Vec<Metric>>,
    pub anim_active: bool,
    pub dragging: bool,
    pub did_drag: bool,
    pub drag_start_cursor: POINT,
    pub drag_start_window: RECT,
    /// 停靠布局与持久化状态
    pub place: Placement,
    pub dock: Dock,
    /// 指针最近一次离开内容的时间戳（ms）；0=在内容内
    pub left_at: u64,
    /// rail 本次展开的时刻（ms），点环刷新 0.3s 防误触用
    pub rail_opened_at: u64,
}

fn main() {
    unsafe {
        if let Err(e) = run() {
            log_msg(&format!("FATAL ERROR: {:?}", e));
        }
    }
}

unsafe fn run() -> Result<()> {
    log_msg("=== ChinaCPA Rail Starting ===");
    // 单实例锁：防止多开叠栏
    let mutex_name = w!("ChinaCPARail_Rust_SingleInstance");
    SetLastError(windows::Win32::Foundation::WIN32_ERROR(0));
    let _mutex = CreateMutexW(None, true, mutex_name);
    if GetLastError() == ERROR_ALREADY_EXISTS {
        log_msg("Another instance already running, exiting.");
        return Ok(());
    }
    log_msg("Single instance acquired.");

        // 解析参数
        let mut base_url = "http://127.0.0.1:8313".to_string();
        let mut password = "123456".to_string();
        let mut interval = 60u64;
        let mut is_demo = false;

        let args: Vec<String> = std::env::args().collect();
        let mut i = 1;
        while i < args.len() {
            match args[i].as_str() {
                "--url" if i + 1 < args.len() => {
                    base_url = args[i + 1].clone();
                    i += 1;
                }
                "--password" if i + 1 < args.len() => {
                    password = args[i + 1].clone();
                    i += 1;
                }
                "--interval" if i + 1 < args.len() => {
                    if let Ok(v) = args[i + 1].parse() {
                        interval = v;
                    }
                    i += 1;
                }
                "--demo" => {
                    is_demo = true;
                }
                _ => {}
            }
            i += 1;
        }

        let instance = GetModuleHandleW(None)?;
        let class_name = w!("PulseRailRustClass");

        let wc = WNDCLASSEXW {
            cbSize: std::mem::size_of::<WNDCLASSEXW>() as u32,
            style: CS_HREDRAW | CS_VREDRAW | CS_DBLCLKS,
            lpfnWndProc: Some(wndproc),
            hInstance: instance.into(),
            hCursor: LoadCursorW(None, IDC_ARROW)?,
            lpszClassName: class_name,
            ..Default::default()
        };
        RegisterClassExW(&wc);

        // 初始窗口直接建成细条尺寸（6×96），而不是 340×560：
        // 窗口一出现就该是收起的细线，展开由 openness 动画驱动。
        // 早前固定 340×560，收起来之前整块罩在屏幕上，用户看到的是灰块。
        let initial_w = 6;
        let initial_h = 96;

    std::panic::set_hook(Box::new(|info| {
        log_msg(&format!("PANIC: {:?}", info));
    }));

    log_msg("Creating window...");
    let hwnd = CreateWindowExW(
        WS_EX_TOPMOST | WS_EX_TOOLWINDOW | WS_EX_LAYERED,
        class_name,
        w!("ChinaCPA Rail"),
        WS_POPUP,
        0,
        0,
        initial_w,
        initial_h,
        None,
        None,
        instance,
        None,
    )?;
    log_msg(&format!("Window created: {:?}", hwnd));

    log_msg("Initializing D2DEngine...");
    let engine = D2DEngine::new(initial_w as u32, initial_h as u32)?;
    log_msg("D2DEngine initialized.");
        let (tx, rx) = channel();

        let initial_metrics = if is_demo {
            demo_metrics()
        } else {
            let client = client::Client::new(base_url.clone(), password.clone());
            client.fetch().unwrap_or_else(|_| demo_metrics())
        };

        // 恢复停靠布局；按持久化顺序重排 metrics（Pulse orderedAccounts 规则：
        // 已存序在前，新账号按显示名追加）
        let mut place = load_placement();
        let mut metrics = initial_metrics;
        if !place.order.is_empty() {
            metrics.sort_by_key(|m| {
                place
                    .order
                    .iter()
                    .position(|p| p == &m.provider)
                    .unwrap_or(usize::MAX)
            });
        }
        let dock = place.dock();
        let anims: Vec<RingAnimState> =
            metrics.iter().map(|m| RingAnimState::new(m.used as f32)).collect();

        let state = AppState {
            hwnd,
            engine,
            metrics,
            anims,
            hover_index: None,
            openness: SpringValue::new(0.0),
            card_openness: SpringValue::new(0.0),
            pointer_y: SpringValue::new((initial_h / 2) as f32),
            busy_spin: 0.0,
            width: initial_w as f32,
            height: initial_h as f32,
            scale: 1.0,
            base_url: base_url.clone(),
            password: password.clone(),
            rx,
            anim_active: false,
            dragging: false,
            did_drag: false,
            drag_start_cursor: POINT::default(),
            drag_start_window: RECT::default(),
            place: std::mem::take(&mut place),
            dock,
            left_at: 0,
            rail_opened_at: 0,
        };

        STATE.with(|s| {
            *s.borrow_mut() = Some(state);
        });

        // 按持久化 dock + 比例定位（初始即细条尺寸）
        let (dock0, place0) = STATE.with(|s| {
            let st = s.borrow();
            (
                st.as_ref().map(|x| x.dock).unwrap_or(Dock::Edge(Edge::Right)),
                st.as_ref().map(|x| x.place.clone()).unwrap_or_default(),
            )
        });
        place_window(hwnd, initial_w, initial_h, dock0, &place0);
        sync_window_size(hwnd);

        // 启动后台网络与 SSE 数据采集线程
        if !is_demo {
            let hwnd_i = hwnd.0 as usize;
            let sse_tx = tx.clone();
            spawn_network_worker(base_url, password, interval, sse_tx);
            // 收到消息唤醒 UI 线程
            std::thread::spawn(move || loop {
                std::thread::sleep(std::time::Duration::from_millis(50));
                let _ = PostMessageW(HWND(hwnd_i as _), WM_APP_DATA, WPARAM(0), LPARAM(0));
            });
        }

        // 常驻看门狗（200ms 采样指针，320ms 拖尾收起状态机）
        SetTimer(hwnd, TIMER_WATCHDOG, 200, None);

        log_msg("Showing window and initial render...");
        let _ = ShowWindow(hwnd, SW_SHOWNOACTIVATE);
        let _ = UpdateWindow(hwnd);
        draw_now();
        log_msg("Entering message loop...");

        // Win32 消息循环
        let mut msg = MSG::default();
        while GetMessageW(&mut msg, None, 0, 0).as_bool() {
            let _ = TranslateMessage(&msg);
            DispatchMessageW(&msg);
        }
        log_msg("Message loop exited!");
    Ok(())
}

fn reposition_window(hwnd: HWND, w: i32, h: i32) {
    unsafe {
        let screen_w = GetSystemMetrics(SM_CXSCREEN);
        let screen_h = GetSystemMetrics(SM_CYSCREEN);
        let x = screen_w - w;
        let y = (screen_h - h) / 2;
        SetWindowPos(
            hwnd,
            HWND_TOPMOST,
            x,
            y,
            w,
            h,
            SWP_NOACTIVATE | SWP_NOZORDER,
        ).ok();
    }
}

/// 按当前 dock + 持久化比例放置窗口（窗口 = rail 本体 + 卡片空间）。
/// 阶段 2a 先支持右/左边（位置侧），渲染镜像在 2b。
unsafe fn place_window(hwnd: HWND, w: i32, h: i32, dock: Dock, place: &Placement) {
    let g = monitor_geom(hwnd);
    let room = placement::room_for(dock, place.snaps, &g);
    let (x, y) = match dock {
        Dock::Edge(Edge::Left) => (room.left, 0i32),
        Dock::Edge(Edge::Right) => (room.right - w, 0i32),
        Dock::Edge(Edge::Top) => (0, room.top),
        Dock::Edge(Edge::Bottom) => (0, room.bottom - h),
        Dock::Floating(_) => (0, 0),
    };
    // 沿边轴用比例定位（y-down：v 沿工作区高度/宽度分布）
    let (x, y) = match dock {
        Dock::Edge(Edge::Left) | Dock::Edge(Edge::Right) => {
            let span = (room.bottom - room.top - h).max(0);
            (x, room.top + (place.v * span as f64).round() as i32)
        }
        Dock::Edge(Edge::Top) | Dock::Edge(Edge::Bottom) => {
            let span = (room.right - room.left - w).max(0);
            (room.left + (place.h * span as f64).round() as i32, y)
        }
        Dock::Floating(_) => (x, y),
    };
    SetWindowPos(
        hwnd,
        HWND_TOPMOST,
        x,
        y,
        w,
        h,
        SWP_NOACTIVATE | SWP_NOZORDER,
    ).ok();
}

/// 按当前 openness 把窗口物理尺寸同步到"细条 ↔ 展开胶囊"。
///
/// Pulse 的 rail 是一整条实心窗口：收起时窗口本身就窄到 6px，展开时才
/// 长到 64px（卡片作为 overlay 另算，不占窗口）。
///
/// 这里必须真的改窗口尺寸，而不是只改绘制范围 —— 分层/透明窗口的
/// 未绘制像素在视觉上仍是一块盖在屏幕上的区域，鼠标能穿透、眼睛穿不过。
/// 按当前 openness 把窗口物理尺寸同步到细条 ↔ 展开胶囊。
fn sync_window_size(hwnd: HWND) {
    unsafe {
        STATE.with(|s| {
            if let Some(state) = s.borrow_mut().as_mut() {
                let scale = state.scale;
                let lm = LayoutMetrics::new(scale);
                let openness = state.openness.cur.clamp(0.0, 1.0);

                // 宽度：细条 6pt ↔ 胶囊 64pt 随 openness 插值。
                // 卡片展开时额外加一个卡片宽度，避免卡片被窗口裁掉。
                let card_extra = if state.card_openness.cur > 0.01 {
                    lm.card_w + 28.0 * scale
                } else {
                    0.0
                };
                let want_w = lm.sliver_w + (lm.pill_w - lm.sliver_w) * openness + card_extra;

                // 高度：细条 96pt ↔ 内容高度随 openness 插值。
                let n = state.metrics.len().min(8);
                let slot_h = lm.ring_d + 6.0 * scale + 16.0 * scale + lm.item_spacing;
                let content_h = if n == 0 {
                    lm.sliver_h
                } else {
                    (n as f32) * slot_h - lm.item_spacing + 46.0 * scale * 2.0
                };
                let want_h = lm.sliver_h + (content_h - lm.sliver_h).max(0.0) * openness;

                // 内容变多时窗口要跟着长，但收缩时用当前内容高度兜底，
                // 避免 hover 期间高度忽大忽小。
                let want_h = want_h.max(lm.sliver_h * 0.5);

                let mut wr = RECT::default();
                GetWindowRect(hwnd, &mut wr).ok();
                let cur_w = (wr.right - wr.left) as f32;
                let cur_h = (wr.bottom - wr.top) as f32;

                // 1px 以内不动，避免每帧都 SetWindowPos 抖屏
                if (want_w - cur_w).abs() < 1.0 && (want_h - cur_h).abs() < 1.0 {
                    return;
                }

                place_window(hwnd, want_w.round() as i32, want_h.round() as i32, state.dock, &state.place.clone());

                // 先更新逻辑尺寸，区域与绘制都按新窗口尺寸计算
                state.width = want_w;
                state.height = want_h;

                // D2D 交换链也必须跟着改尺寸。
                //
                // 窗口缩到细条而交换链还停在 340×560 时，DXGI 会把旧尺寸的
                // 后台缓冲按新窗口裁切/拉伸，画出来就是一块灰 —— 那才是
                // 用户看到的"灰色遮罩"，窗口尺寸只是它的一半原因。
                let new_w = want_w.round().max(1.0) as u32;
                let new_h = want_h.round().max(1.0) as u32;
                if let Err(e) = state.engine.resize(new_w, new_h) {
                    log_msg(&format!("engine resize failed: {:?}", e));
                }

                // 记录到 state，供 hit-test 与绘制使用
                state.width = want_w;
                state.height = want_h;
            }
        });
    }
}

unsafe extern "system" fn wndproc(hwnd: HWND, msg: u32, wparam: WPARAM, lparam: LPARAM) -> LRESULT {
    match msg {
        WM_PAINT => {
            let mut ps = PAINTSTRUCT::default();
            BeginPaint(hwnd, &mut ps);
            draw_now();
            let _ = EndPaint(hwnd, &ps);
            LRESULT(0)
        }
        WM_TIMER => {
            match wparam.0 {
                TIMER_ANIM => {
                    let mut still_busy = false;
                    STATE.with(|s| {
                        if let Some(state) = s.borrow_mut().as_mut() {
                            let dt = 1.0 / 60.0;
                            if state.openness.step(dt) {
                                still_busy = true;
                            }
                            if state.card_openness.step(dt) {
                                still_busy = true;
                            }
                            if state.pointer_y.step(dt) {
                                still_busy = true;
                            }
                            state.busy_spin = (state.busy_spin + dt * 4.0) % (std::f32::consts::PI * 2.0);

                            for a in &mut state.anims {
                                if a.arc.step(dt) {
                                    still_busy = true;
                                }
                                if a.halo.step(dt) {
                                    still_busy = true;
                                }
                                if a.clock.step(dt) {
                                    still_busy = true;
                                }
                                // 刷新扫光：0.85s 走完一圈（Pulse refreshMark）
                                if a.sweep > 0.0 {
                                    a.sweep = (a.sweep + dt / 0.85).min(1.0);
                                    still_busy = true; // 含收尾帧：清零后还要再画一次
                                    if a.sweep >= 1.0 {
                                        a.sweep = 0.0;
                                    }
                                }
                            }
                        }
                    });
                    // 先按当前 openness 同步窗口物理尺寸，再绘制。
                    //
                    // 早前这里只更新 openness 而不改窗口尺寸：窗口从创建起就固定
                    // 340×560，收成细条时只是内部绘制变窄，那一大块窗口仍然罩在
                    // 屏幕上，把后面的浏览器整片盖住 —— 用户看到的就是一块灰。
                    sync_window_size(hwnd);
                    draw_now();
                    if !still_busy {
                        STATE.with(|s| {
                            if let Some(state) = s.borrow_mut().as_mut() {
                                state.anim_active = false;
                            }
                        });
                        KillTimer(hwnd, TIMER_ANIM).ok();
                    }
                }
                TIMER_WATCHDOG => {
                    // Pulse 状态机：指针离开"内容区"后延迟 320ms 收起；
                    // 拖拽/自由态不自动收起（自由 rail 是用户故意放的）。
                    let mut pt = POINT::default();
                    GetCursorPos(&mut pt).ok();
                    let mut wr = RECT::default();
                    GetWindowRect(hwnd, &mut wr).ok();

                    let over_content = STATE.with(|s| {
                        if let Some(state) = s.borrow().as_ref() {
                            let scale = state.scale;
                            let lm = LayoutMetrics::new(scale);
                            let lx = pt.x - wr.left;
                            let ly = pt.y - wr.top;
                            let slack = 8.0;
                            if state.openness.cur < 0.3 {
                                // sliver 命中：20pt 磁吸宽 + slack
                                let hit = match state.dock {
                                    Dock::Edge(Edge::Left) => {
                                        (lx as f32) <= 20.0 * scale + slack
                                    }
                                    Dock::Edge(Edge::Right) => {
                                        (lx as f32) >= state.width - 20.0 * scale - slack
                                    }
                                    Dock::Edge(Edge::Top) => {
                                        (ly as f32) <= 20.0 * scale + slack
                                    }
                                    Dock::Edge(Edge::Bottom) => {
                                        (ly as f32) >= state.height - 20.0 * scale - slack
                                    }
                                    Dock::Floating(_) => true,
                                };
                                hit
                            } else {
                                // 展开态：rail 条带 + 卡片条带（全幅，Pulse isOverContent）
                                let rail_band = match state.dock {
                                    Dock::Edge(Edge::Left) => {
                                        (lx as f32) <= lm.pill_w + slack
                                    }
                                    Dock::Edge(Edge::Right) => {
                                        (lx as f32) >= state.width - lm.pill_w - slack
                                    }
                                    Dock::Edge(Edge::Top) => {
                                        (ly as f32) <= lm.pill_w + slack
                                    }
                                    Dock::Edge(Edge::Bottom) => {
                                        (ly as f32) >= state.height - lm.pill_w - slack
                                    }
                                    Dock::Floating(_) => true,
                                };
                                rail_band
                            }
                        } else {
                            false
                        }
                    });

                    let mut need_anim = false;
                    STATE.with(|s| {
                        if let Some(state) = s.borrow_mut().as_mut() {
                            if over_content || state.dragging {
                                state.left_at = 0;
                                return;
                            }
                            let free = !state.dock.is_docked();
                            if free {
                                return; // 自由态永不自动收起
                            }
                            if state.left_at == 0 {
                                state.left_at = now_ms();
                                return;
                            }
                            if now_ms() - state.left_at < 320 {
                                return; // Pulse scheduleHide 延迟
                            }
                            if state.card_openness.target > 0.01 || state.hover_index.is_some() {
                                state.card_openness.set(0.0);
                                state.hover_index = None;
                                for a in &mut state.anims {
                                    a.halo.set(0.0);
                                }
                                need_anim = true;
                            }
                            if state.openness.target > 0.01 && state.place.auto_collapse {
                                state.openness.set(0.0);
                                need_anim = true;
                            }
                        }
                    });
                    if need_anim {
                        ensure_anim_ticker(hwnd);
                    }
                }
                _ => {}
            }
            LRESULT(0)
        }
        WM_MOUSEMOVE => {
            let x = (lparam.0 & 0xFFFF) as i16 as f32;
            let y = ((lparam.0 >> 16) & 0xFFFF) as i16 as f32;
            let mut need_anim = false;

            STATE.with(|s| {
                if let Some(state) = s.borrow_mut().as_mut() {
                    if state.dragging {
                        let mut cur = POINT::default();
                        GetCursorPos(&mut cur).ok();
                        let dx = cur.x - state.drag_start_cursor.x;
                        let dy = cur.y - state.drag_start_cursor.y;
                        if dx != 0 || dy != 0 {
                            state.did_drag = true;
                        }
                        SetWindowPos(
                            hwnd,
                            None,
                            state.drag_start_window.left + dx,
                            state.drag_start_window.top + dy,
                            0,
                            0,
                            SWP_NOSIZE | SWP_NOZORDER | SWP_NOACTIVATE,
                        ).ok();
                        return;
                    }

                    state.left_at = 0;
                    if state.openness.target < 0.99 {
                        if state.openness.cur < 0.3 {
                            // 刚展开：记录展开时刻（点环刷新 0.3s 防误触）
                            state.rail_opened_at = now_ms();
                        }
                        state.openness.set(1.0);
                        need_anim = true;
                    }

                    let scale = state.scale;
                    let lm = LayoutMetrics::new(scale);
                    let slot_h = lm.ring_d + 6.0 * scale + 16.0 * scale + lm.item_spacing;
                    let n = state.metrics.len().min(8);
                    let total_content_h = (n as f32) * slot_h - lm.item_spacing;
                    let expanded_h = total_content_h + 46.0 * scale * 2.0;
                    let dock_top = (state.height - expanded_h) / 2.0;
                    let start_y = dock_top + 46.0 * scale + lm.ring_d / 2.0;
                    let dock_left = state.width - lm.pill_w;

                    let mut hovered_slot = None;
                    if x >= dock_left && x <= state.width {
                        for i in 0..n {
                            let cy = start_y + (i as f32) * slot_h;
                            if (y - cy).abs() <= 26.0 * scale {
                                hovered_slot = Some((i, cy));
                                break;
                            }
                        }
                    }

                    if let Some((idx, cy)) = hovered_slot {
                        if state.hover_index != Some(idx) {
                            state.hover_index = Some(idx);
                            state.card_openness.set(1.0);
                            state.pointer_y.set(cy);

                            for (j, a) in state.anims.iter_mut().enumerate() {
                                if j == idx {
                                    a.halo.set(1.0);
                                } else {
                                    a.halo.set(0.0);
                                }
                            }
                            need_anim = true;
                        }
                    } else {
                        // 在胶囊空白处，收起卡片
                        if state.card_openness.target > 0.01 {
                            state.card_openness.set(0.0);
                            state.hover_index = None;
                            for a in &mut state.anims {
                                a.halo.set(0.0);
                            }
                            need_anim = true;
                        }
                    }
                }
            });

            if need_anim {
                ensure_anim_ticker(hwnd);
            }

            let mut tme = TRACKMOUSEEVENT {
                cbSize: std::mem::size_of::<TRACKMOUSEEVENT>() as u32,
                dwFlags: TME_LEAVE,
                hwndTrack: hwnd,
                dwHoverTime: 0,
            };
            TrackMouseEvent(&mut tme).ok();

            LRESULT(0)
        }
        WM_MOUSELEAVE_MSG => {
            STATE.with(|s| {
                if let Some(state) = s.borrow_mut().as_mut() {
                    state.card_openness.set(0.0);
                    state.hover_index = None;
                    for a in &mut state.anims {
                        a.halo.set(0.0);
                    }
                }
            });
            ensure_anim_ticker(hwnd);
            LRESULT(0)
        }
        WM_NCHITTEST => {
            let x = (lparam.0 & 0xFFFF) as i16 as i32;
            let y = ((lparam.0 >> 16) & 0xFFFF) as i16 as i32;

            let mut pt = POINT { x, y };
            let _ = ScreenToClient(hwnd, &mut pt);

            let is_opaque = STATE.with(|s| {
                if let Some(state) = s.borrow().as_ref() {
                    let lm = LayoutMetrics::new(state.scale);
                    let px = pt.x as f32;

                    if state.openness.cur < 0.3 {
                        // 细条命中区域（Pulse: 20pt 磁吸边缘）
                        px >= state.width - 20.0 * state.scale && px <= state.width
                    } else {
                        let pill_hit = px >= state.width - lm.pill_w && px <= state.width;
                        let card_hit = state.card_openness.cur > 0.1
                            && px >= state.width - lm.pill_w - lm.card_w - 28.0 * state.scale
                            && px <= state.width - lm.pill_w;
                        pill_hit || card_hit
                    }
                } else {
                    false
                }
            });

            if is_opaque {
                LRESULT(HTCLIENT as isize)
            } else {
                LRESULT(HTTRANSPARENT as isize)
            }
        }
        WM_LBUTTONDOWN => {
            STATE.with(|s| {
                if let Some(state) = s.borrow_mut().as_mut() {
                    state.dragging = true;
                    GetCursorPos(&mut state.drag_start_cursor).ok();
                    GetWindowRect(hwnd, &mut state.drag_start_window).ok();
                    SetCapture(hwnd);
                }
            });
            LRESULT(0)
        }
        WM_LBUTTONUP => {
            let ux = (lparam.0 & 0xFFFF) as i16 as f32;
            let uy = ((lparam.0 >> 16) & 0xFFFF) as i16 as f32;
            let mut refresh_req: Option<(String, String)> = None;
            let mut sweep_idx: Option<usize> = None;
            STATE.with(|s| {
                if let Some(state) = s.borrow_mut().as_mut() {
                    if state.dragging {
                        state.dragging = false;
                        ReleaseCapture().ok();
                        if state.did_drag {
                            // 拖拽结束：把 rail 沿边轴的位置比例写回并持久化
                            let mut wr = RECT::default();
                            GetWindowRect(hwnd, &mut wr).ok();
                            let g = monitor_geom(hwnd);
                            let room = placement::room_for(state.dock, state.place.snaps, &g);
                            match state.dock {
                                Dock::Edge(Edge::Left) | Dock::Edge(Edge::Right) => {
                                    let span = (room.bottom - room.top - (wr.bottom - wr.top)).max(0);
                                    if span > 0 {
                                        state.place.v = ((wr.top - room.top) as f64 / span as f64).clamp(0.0, 1.0);
                                    }
                                }
                                Dock::Edge(Edge::Top) | Dock::Edge(Edge::Bottom) => {
                                    let span = (room.right - room.left - (wr.right - wr.left)).max(0);
                                    if span > 0 {
                                        state.place.h = ((wr.left - room.left) as f64 / span as f64).clamp(0.0, 1.0);
                                    }
                                }
                                Dock::Floating(_) => {}
                            }
                            save_placement(&state.place.clone());
                            let dock = state.dock;
                            let place = state.place.clone();
                            let w = wr.right - wr.left;
                            let h = wr.bottom - wr.top;
                            place_window(hwnd, w, h, dock, &place);
                        } else if state.openness.cur > 0.9
                            && now_ms().saturating_sub(state.rail_opened_at) >= 300
                        {
                            // 点环刷新（Pulse ringClick）：短按 + 展开保护期 0.3s
                            // + 落点在环圆内（半径 ×1.08 容差）
                            let scale = state.scale;
                            let lm = LayoutMetrics::new(scale);
                            let slot_h = lm.ring_d + 6.0 * scale + 16.0 * scale + lm.item_spacing;
                            let n = state.metrics.len().min(8);
                            let expanded_h = (n as f32) * slot_h - lm.item_spacing + 46.0 * scale * 2.0;
                            let dock_top = (state.height - expanded_h) / 2.0;
                            let start_y = dock_top + 46.0 * scale + lm.ring_d / 2.0;
                            let cx = state.width - lm.pill_w / 2.0;
                            let hit_r = lm.ring_d / 2.0 * 1.08;
                            for i in 0..n {
                                let cy = start_y + (i as f32) * slot_h;
                                let dx = ux - cx;
                                let dy = uy - cy;
                                if dx * dx + dy * dy <= hit_r * hit_r {
                                    if let Some(m) = state.metrics.get(i) {
                                        if let Some(uid) = m.uids.first() {
                                            refresh_req = Some((m.provider.clone(), uid.clone()));
                                            sweep_idx = Some(i);
                                        }
                                    }
                                    break;
                                }
                            }
                        }
                        state.did_drag = false;
                    }
                }
            });
            if let (Some((provider, uid)), Some(idx)) = (refresh_req, sweep_idx) {
                let (base, pass) = STATE.with(|s| {
                    let st = s.borrow();
                    match st.as_ref() {
                        Some(x) => (x.base_url.clone(), x.password.clone()),
                        None => (String::new(), String::new()),
                    }
                });
                log_msg(&format!("ring-click refresh provider={} uid={}", provider, uid));
                client::spawn_refresh(base, pass, provider, uid);
                STATE.with(|s| {
                    if let Some(state) = s.borrow_mut().as_mut() {
                        if let Some(a) = state.anims.get_mut(idx) {
                            a.sweep = 0.001; // 启动刷新扫光
                        }
                    }
                });
                ensure_anim_ticker(hwnd);
            }
            LRESULT(0)
        }
        WM_LBUTTONDBLCLK => {
            let url = STATE.with(|s| {
                s.borrow().as_ref().map(|st| st.base_url.clone()).unwrap_or_else(|| "http://127.0.0.1:8313".to_string())
            });
            open_url(&url);
            LRESULT(0)
        }
        WM_RBUTTONUP => {
            show_context_menu(hwnd);
            LRESULT(0)
        }
        WM_APP_DATA => {
            let mut need_anim = false;
            let mut updated = false;
            STATE.with(|s| {
                if let Some(state) = s.borrow_mut().as_mut() {
                    while let Ok(new_metrics) = state.rx.try_recv() {
                        while state.anims.len() < new_metrics.len() {
                            state.anims.push(RingAnimState::new(0.0));
                        }
                        for (i, m) in new_metrics.iter().enumerate() {
                            state.anims[i].arc.set(m.used as f32);
                            // 窗口时钟：-1 = 数据不全，弹簧目标 -1（绘制侧 >-0.5 才画）
                            state.anims[i].clock.set(m.clock_frac as f32);
                        }
                        state.metrics = new_metrics;
                        need_anim = true;
                        updated = true;
                    }
                }
            });
            if need_anim {
                ensure_anim_ticker(hwnd);
            }
            if updated {
                draw_now();
            }
            LRESULT(0)
        }
        WM_DESTROY => {
            PostQuitMessage(0);
            LRESULT(0)
        }
        _ => DefWindowProcW(hwnd, msg, wparam, lparam),
    }
}

fn ensure_anim_ticker(hwnd: HWND) {
    STATE.with(|s| {
        if let Some(state) = s.borrow_mut().as_mut() {
            if !state.anim_active {
                state.anim_active = true;
                unsafe {
                    SetTimer(hwnd, TIMER_ANIM, 16, None);
                }
            }
        }
    });
}

fn draw_now() {
    STATE.with(|s| {
        if let Some(state) = s.borrow_mut().as_mut() {
            if let Err(e) = render_frame(
                &state.engine,
                &state.metrics,
                &state.anims,
                state.hover_index,
                state.openness.cur,
                state.card_openness.cur,
                state.pointer_y.cur,
                state.busy_spin,
                state.width,
                state.height,
                state.scale,
                state.hwnd,
            ) {
                log_msg(&format!("render_frame err: {:?}", e));
            }
        }
    });
}

fn show_context_menu(hwnd: HWND) {
    unsafe {
        let menu = match CreatePopupMenu() {
            Ok(m) => m,
            Err(_) => return,
        };

        let cmd_open = 1;
        let cmd_copy = 2;
        let cmd_quit = 3;

        AppendMenuW(menu, MF_STRING, cmd_open, w!("打开管理面板")).ok();
        AppendMenuW(menu, MF_STRING, cmd_copy, w!("复制接口地址 (http://127.0.0.1:8313/v1)")).ok();
        AppendMenuW(menu, MF_SEPARATOR, 0, None).ok();
        AppendMenuW(menu, MF_STRING, cmd_quit, w!("退出浮动栏")).ok();

        let mut pt = POINT::default();
        GetCursorPos(&mut pt).ok();
        let _ = SetForegroundWindow(hwnd);

        let selected = TrackPopupMenu(
            menu,
            TPM_LEFTALIGN | TPM_RETURNCMD | TPM_NONOTIFY,
            pt.x,
            pt.y,
            0,
            hwnd,
            None,
        );
        DestroyMenu(menu).ok();

        match selected.0 as usize {
            1 => {
                let url = STATE.with(|s| {
                    s.borrow().as_ref().map(|st| st.base_url.clone()).unwrap_or_else(|| "http://127.0.0.1:8313".to_string())
                });
                open_url(&url);
            }
            2 => {
                let api_url = STATE.with(|s| {
                    s.borrow().as_ref().map(|st| format!("{}/v1", st.base_url.trim_end_matches('/'))).unwrap_or_else(|| "http://127.0.0.1:8313/v1".to_string())
                });
                set_clipboard_text(hwnd, &api_url);
            }
            3 => {
                DestroyWindow(hwnd).ok();
            }
            _ => {}
        }
    }
}

fn open_url(url: &str) {
    let wide: Vec<u16> = url.encode_utf16().chain(std::iter::once(0)).collect();
    unsafe {
        windows::Win32::UI::Shell::ShellExecuteW(
            None,
            w!("open"),
            PCWSTR(wide.as_ptr()),
            None,
            None,
            SW_SHOW,
        );
    }
}

fn set_clipboard_text(hwnd: HWND, text: &str) {
    unsafe {
        if OpenClipboard(hwnd).is_ok() {
            EmptyClipboard().ok();
            let wide: Vec<u16> = text.encode_utf16().chain(std::iter::once(0)).collect();
            let size = wide.len() * 2;
            if let Ok(hmem) = GlobalAlloc(GMEM_MOVEABLE, size) {
                let ptr = GlobalLock(hmem);
                if !ptr.is_null() {
                    std::ptr::copy_nonoverlapping(wide.as_ptr() as *const u8, ptr as *mut u8, size);
                    GlobalUnlock(hmem).ok();
                    // 13 is CF_UNICODETEXT
                    SetClipboardData(13, HANDLE(hmem.0 as _)).ok();
                }
            }
            CloseClipboard().ok();
        }
    }
}
