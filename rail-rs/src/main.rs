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
use placement::{load_placement, monitor_geom, save_placement, window_to_canon, Dock, Edge, Placement};
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
    /// 拖拽中判定出的目标停靠（只记录，松手才应用，避免拖拽中反复翻转）
    pub drag_target: Option<Dock>,
    /// 程序主动 ReleaseCapture 时置真，让随之而来的 WM_CAPTURECHANGED
    /// 知道这是自己放的、不要当成“拖拽被抢走”。用完即清。
    pub releasing_capture: bool,
    /// 停靠布局与持久化状态
    pub place: Placement,
    pub dock: Dock,
    /// 指针最近一次离开内容的时间戳（ms）；0=在内容内
    pub left_at: u64,
    /// rail 本次展开的时刻（ms），点环刷新 0.3s 防误触用
    pub rail_opened_at: u64,

    // ── 液态吸附（Pulse DropletTether）──
    /// 液颈细度 0..1（1=完全贴边无颈，0=极细）；贴上瞬间置 0.85 后按 exp(-dt/0.22) 衰减
    pub thinning: f32,
    /// 断开融合 0..1；断边瞬间置 1 后按 exp(-dt/0.14) 衰减
    pub fusion: f32,
    /// 液颈拉伸距离（px，0=贴边）；由指针-贴边距离推出，≥80 触发断裂
    pub pulled: f32,
    /// 正在断开的计时（ms），0=不在断裂中
    pub parting_since: u64,
    /// 断开进度 0..1（两段回缩）
    pub parted: f32,
    /// 拖拽速度（px/s，用于抛掷拉伸）
    pub vel: (f32, f32),
    pub last_drag_pt: POINT,
    pub last_drag_ms: u64,
}

const TETHER_BREAK: f32 = 80.0;
const TETHER_GRIP: f32 = 0.15;
const TETHER_PARTING_MS: u64 = 220;

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
            releasing_capture: false,
            drag_start_cursor: POINT::default(),
            drag_start_window: RECT::default(),
            drag_target: None,
            place: std::mem::take(&mut place),
            dock,
            left_at: 0,
            rail_opened_at: 0,
            thinning: 1.0,
            fusion: 0.0,
            pulled: 0.0,
            parting_since: 0,
            parted: 0.0,
            vel: (0.0, 0.0),
            last_drag_pt: POINT::default(),
            last_drag_ms: 0,
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
        // 收起态尺寸按轴向摆：竖栏 6 宽 × 96 长，横条 96 宽 × 6 高。
        // 早前一律用 initial_w×initial_h（6×96），自由横条重启后会变成
        // 一根 96 宽 6 高的竖形细条，与它展开后的横向绘制对不上。
        let collapsed_lm = render::LayoutMetrics::new(1.0);
        let (cw, ch) = placement::window_size(
            dock0,
            collapsed_lm.sliver_w as i32,
            collapsed_lm.sliver_h as i32,
        );
        place_window(hwnd, cw, ch, dock0, &place0, 0.0, 1);
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
///
/// 自由态存的是**胶囊中心**的比例（见 `ratios_for_centre`），所以放置时
/// 要先按「胶囊在当前窗口尺寸里处在哪」把窗口推到位。若直接按窗口左上角
/// 铺开，收起（6pt）和展开（342pt）会算出不同的栏位置 —— 悬停展开时栏
/// 被挤出屏幕再夹回来，看起来就是「一碰就往上跳」。
unsafe fn place_window(
    hwnd: HWND,
    w: i32,
    h: i32,
    dock: Dock,
    place: &Placement,
    openness: f32,
    ring_count: usize,
) {
    let g = monitor_geom(hwnd);
    let room = placement::room_for(dock, place.snaps, &g);
    let (x, y) = match dock {
        Dock::Edge(Edge::Left) => (room.left, 0i32),
        Dock::Edge(Edge::Right) => (room.right - w, 0i32),
        Dock::Edge(Edge::Top) => (0, room.top),
        Dock::Edge(Edge::Bottom) => (0, room.bottom - h),
        Dock::Floating(_) => {
            let lm = LayoutMetrics::new(1.0);
            let (bl, bt, br, bb) =
                capsule_band(dock, w as f32, h as f32, openness, ring_count, &lm);
            let band_w = (br - bl) as f64;
            let band_h = (bb - bt) as f64;
            // 按比例求出胶囊左上角应在的位置，再换算窗口左上角
            let band_x = room.left as f64 + place.h * ((room.right - room.left) as f64 - band_w);
            let band_y = room.top as f64 + place.v * ((room.bottom - room.top) as f64 - band_h);
            let band_x = band_x.clamp(room.left as f64, (room.right as f64 - band_w).max(room.left as f64));
            let band_y = band_y.clamp(room.top as f64, (room.bottom as f64 - band_h).max(room.top as f64));
            (
                (band_x - bl as f64).round() as i32,
                (band_y - bt as f64).round() as i32,
            )
        }
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
    // 自由态收进屏幕（停靠态本来就贴着边）
    let (x, y) = if dock.is_docked() {
        (x, y)
    } else {
        (
            x.clamp(room.left, (room.right - w).max(room.left)),
            y.clamp(room.top, (room.bottom - h).max(room.top)),
        )
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

/// 胶囊（栏本体）在窗口物理坐标里占的那条带：(left, top, right, bottom)。
///
/// 必须与 `render_frame` 的几何一致：规范空间里栏沿长度轴居中、沿厚度轴
/// 贴着 `bu`（屏幕边那一侧），再经轴向变换映射回物理空间。窗口比栏大
/// （一侧留了卡片空间），所以「离某条边多远」推不出栏在哪。
fn capsule_band(
    dock: Dock,
    win_w: f32,
    win_h: f32,
    openness: f32,
    n: usize,
    lm: &LayoutMetrics,
) -> (f32, f32, f32, f32) {
    let (bu, bv) = placement::canonical_size(dock, win_w as i32, win_h as i32);
    // 必须用**当前实际环数**：用上限会让短栏的命中带比画出来的长一截，
    // 指针落在空白处也算「在栏上」，拖拽起始点就会错位。
    let expanded = lm.expanded_length(dock, n.max(1));

    // 与 render_frame / sync_window_size 相同的插值。
    // 自由态不收起（一直是展开胶囊），所以它的厚度不随 openness 变。
    let thickness = lm.cross_thickness(dock);
    let cur_thickness = if dock.is_docked() {
        lm.sliver_w + (thickness - lm.sliver_w) * openness
    } else {
        thickness
    };
    let cur_length = if dock.is_docked() {
        lm.sliver_h + (expanded - lm.sliver_h).max(0.0) * openness
    } else {
        expanded
    };

    let v0 = (bv - cur_length) / 2.0;
    let v1 = v0 + cur_length;
    let u0 = bu - cur_thickness;
    let u1 = bu;

    let (x0, y0) = placement::canon_to_window(dock, u0, v0, bu, bv);
    let (x1, y1) = placement::canon_to_window(dock, u1, v1, bu, bv);
    (x0.min(x1), y0.min(y1), x0.max(x1), y0.max(y1))
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

                // 厚度：细条 6pt ↔ 胶囊厚度随 openness 插值。
                // 竖栏 64pt，横栏 78pt（环 36 + 间隙 6 + 文字 16 + 上下各 10）。
                // 卡片展开时额外加一个卡片宽度，避免卡片被窗口裁掉。
                let card_extra = if state.card_openness.cur > 0.01 {
                    lm.card_w + 28.0 * scale
                } else {
                    0.0
                };
                let thickness = lm.cross_thickness(state.dock);
                let want_w = lm.sliver_w + (thickness - lm.sliver_w) * openness + card_extra;

                // 长度：细条 96pt ↔ 展开后长度随 openness 插值。
                // 竖栏两端各留 46，横栏各留 10；节距也按轴向不同。
                let n = state.metrics.len().min(8);
                let content_h = if n == 0 {
                    lm.sliver_h
                } else {
                    lm.expanded_length(state.dock, n)
                };
                let want_h = lm.sliver_h + (content_h - lm.sliver_h).max(0.0) * openness;

                // 自由态不收起，所以厚度始终是完整胶囊（与停靠态展开一致）。
                // 早前给自由态单独做了「薄胶囊」收起档，但它既然不会收起，
                // 那一档只会让展开态莫名其妙地窄 8pt。
                let (want_w, want_h) = if state.dock.is_docked() {
                    (want_w, want_h)
                } else {
                    let w = thickness + card_extra;
                    (w, want_h)
                };

                // 内容变多时窗口要跟着长，但收缩时用当前内容高度兜底，
                // 避免 hover 期间高度忽大忽小。
                let want_h = want_h.max(lm.sliver_h * 0.5);

                // 规范尺寸 → 物理尺寸（Top/Bottom 交换宽高）
                let (pw, ph) = placement::window_size(state.dock, want_w.round().max(1.0) as i32, want_h.round().max(1.0) as i32);

                let mut wr = RECT::default();
                GetWindowRect(hwnd, &mut wr).ok();
                let cur_w = (wr.right - wr.left) as f32;
                let cur_h = (wr.bottom - wr.top) as f32;

                // 1px 以内不动，避免每帧都 SetWindowPos 抖屏（物理对物理比较）
                if (pw as f32 - cur_w).abs() < 1.0 && (ph as f32 - cur_h).abs() < 1.0 {
                    state.width = pw as f32;
                    state.height = ph as f32;
                    return;
                }

                if state.dragging {
                    // 拖拽中尺寸变化分两种：
                    //   1) 展开/收起（openness 变化）—— 只改尺寸，锚点保持，
                    //      否则栏会随着窗口从 356 缩到 108 而整体挪位；
                    //   2) 轴向互换（横条 ↔ 竖栏）—— 宽高交换，必须重算基准，
                    //      否则跟手公式 `drag_start_window.left + dx` 会算回起点。
                    // 用「旧宽高是否与新宽高互换」判别第 2 种。
                    let axis_swapped = (cur_w - ph as f32).abs() < 2.0
                        && (cur_h - pw as f32).abs() < 2.0
                        && (cur_w - cur_h).abs() > 2.0;
                    if axis_swapped {
                        let mut cur = POINT::default();
                        GetCursorPos(&mut cur).ok();
                        let old_w = state.drag_start_window.right - state.drag_start_window.left;
                        let old_h = state.drag_start_window.bottom - state.drag_start_window.top;
                        // 指针在旧窗口里的相对位置
                        let rx = if old_w > 0 { (cur.x - state.drag_start_window.left) as f64 / old_w as f64 } else { 0.5 };
                        let ry = if old_h > 0 { (cur.y - state.drag_start_window.top) as f64 / old_h as f64 } else { 0.5 };
                        let nx = cur.x - (rx * pw as f64).round() as i32;
                        let ny = cur.y - (ry * ph as f64).round() as i32;
                        SetWindowPos(hwnd, HWND_TOPMOST, nx, ny, pw, ph, SWP_NOACTIVATE | SWP_NOZORDER).ok();
                        state.drag_start_window = RECT {
                            left: nx,
                            top: ny,
                            right: nx + pw,
                            bottom: ny + ph,
                        };
                        state.drag_start_cursor = cur;
                    } else {
                        // 纯尺寸变化：保持左上角，下一帧继续按位移跟手
                        let mut wr2 = RECT::default();
                        GetWindowRect(hwnd, &mut wr2).ok();
                        SetWindowPos(hwnd, HWND_TOPMOST, wr2.left, wr2.top, pw, ph, SWP_NOACTIVATE | SWP_NOZORDER).ok();
                    }
                } else {
                    let n_rings = state.metrics.len().min(8);
                    place_window(
                        hwnd,
                        pw,
                        ph,
                        state.dock,
                        &state.place.clone(),
                        state.openness.cur.clamp(0.0, 1.0),
                        n_rings,
                    );
                }

                // 逻辑尺寸 = 物理尺寸（渲染内部再换算规范空间）
                state.width = pw as f32;
                state.height = ph as f32;

                // D2D 交换链也必须跟着改尺寸。
                let (pw2, ph2) = (pw, ph);
                let new_w = pw2.max(1) as u32;
                let new_h = ph2.max(1) as u32;
                if let Err(e) = state.engine.resize(new_w, new_h) {
                    log_msg(&format!("engine resize failed: {:?}", e));
                }

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
                    let mut tether_freed = false;
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
                                // 液颈细度/融合指数衰减（Pulse: exp(-dt/0.22) / exp(-dt/0.14)）
                                state.thinning *= (-dt / 0.22).exp();
                                state.fusion *= (-dt / 0.14).exp();
                                if state.thinning < 0.005 {
                                    state.thinning = 0.0;
                                    still_busy = true;
                                }
                                if state.fusion < 0.005 {
                                    state.fusion = 0.0;
                                    still_busy = true;
                                }
                                // 断裂进程：0.22s 内 parted 0→1（两段回缩），完成转自由态
                                if state.parting_since > 0 {
                                    let p = (now_ms() - state.parting_since) as f32 / TETHER_PARTING_MS as f32;
                                    if p >= 1.0 {
                                        state.parted = 1.0;
                                        state.parting_since = 0;
                                        // standFree：转为自由态（保留轴向）
                                        let v = state.dock.vertical();
                                        state.dock = Dock::Floating(v);
                                        state.place.set_dock(state.dock);
                                        state.pulled = 0.0;
                                        state.thinning = 0.0;
                                        tether_freed = true;
                                    } else {
                                        state.parted = 1.0 - (1.0 - p) * (1.0 - p);
                                    }
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
                    if tether_freed {
                        // 液颈断开转自由态：按**胶囊中心**重算比例并持久化，
                        // 与松手路径一致（窗口含卡片空间，不能用窗口左上角）。
                        STATE.with(|s| {
                            if let Some(st) = s.borrow_mut().as_mut() {
                                let mut wr = RECT::default();
                                GetWindowRect(hwnd, &mut wr).ok();
                                let g = monitor_geom(hwnd);
                                let room = placement::room_for(st.dock, st.place.snaps, &g);
                                let lm = LayoutMetrics::new(st.scale);
                                let (bl, bt, br, bb) = capsule_band(
                                    st.dock,
                                    (wr.right - wr.left) as f32,
                                    (wr.bottom - wr.top) as f32,
                                    st.openness.cur,
                                    st.metrics.len().min(8),
                                    &lm,
                                );
                                let cx = wr.left as f32 + (bl + br) / 2.0;
                                let cy = wr.top as f32 + (bt + bb) / 2.0;
                                let (h, v) = placement::ratios_for_centre(
                                    cx, cy, room, br - bl, bb - bt,
                                );
                                st.place.h = h;
                                st.place.v = v;
                                save_placement(&st.place.clone());
                            }
                        });
                    }
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
                            let lx = (pt.x - wr.left) as f32;
                            let ly = (pt.y - wr.top) as f32;
                            let slack = 8.0f32;
                            let win_w = (wr.right - wr.left) as f32;
                            let win_h = (wr.bottom - wr.top) as f32;
                            // 指针必须在窗口矩形内（含 slack 容差）才算"在内容上"
                            if lx < -slack || ly < -slack || lx > win_w + slack || ly > win_h + slack {
                                return false;
                            }
                            // 与命中检测同一套几何：栏在窗口里占的那条带。
                            // 早前按「离停靠边多远」判断，横向长栏的栏身落在
                            // 窗口中部，指针明明在栏上却被判成离开，一悬停就收。
                            let (bl, bt, br, bb) = capsule_band(
                                state.dock,
                                win_w,
                                win_h,
                                state.openness.cur,
                                state.metrics.len().min(8),
                                &lm,
                            );
                            lx >= bl - slack
                                && lx <= br + slack
                                && ly >= bt - slack
                                && ly <= bb + slack
                        } else {
                            false
                        }
                    });

                    let mut need_anim = false;
                    // 拖拽中一律不收起：指针正在拖着这根栏，任何「离开内容」的
                    // 判断都可能是窗口尺寸/轴向变化造成的假象（拖出屏幕边时
                    // 窗口刚断裂、尺寸还在收缩），收起会让拖拽视觉中断。
                    let is_dragging = STATE.with(|s| {
                        s.borrow().as_ref().map(|st| st.dragging).unwrap_or(false)
                    });
                    if !is_dragging {
                    STATE.with(|s| {
                        if let Some(state) = s.borrow_mut().as_mut() {
                            if over_content {
                                state.left_at = 0;
                                return;
                            }
                            // 自由态不自动收起：用户把它摆在桌面上就是要一直看得见。
                            // （早前试过让它也收起，结果指针一移开就缩成一条细线，
                            // 连额度都读不了 —— 那是更差的行为，所以退回原位。）
                            // 停靠态才收起：它贴着屏幕边，缩成细条不占地方。
                            if !state.dock.is_docked() {
                                // 但卡片要收：卡片是「悬停才看」的详情
                                if state.card_openness.target > 0.01 || state.hover_index.is_some() {
                                    state.card_openness.set(0.0);
                                    state.hover_index = None;
                                    for a in &mut state.anims {
                                        a.halo.set(0.0);
                                    }
                                    need_anim = true;
                                }
                                return;
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
                    }
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
            let mut dock_changed = false;

            STATE.with(|s| {
                if let Some(state) = s.borrow_mut().as_mut() {
                    if state.dragging {
                        let mut cur = POINT::default();
                        GetCursorPos(&mut cur).ok();
                        let dx = cur.x - state.drag_start_cursor.x;
                        let dy = cur.y - state.drag_start_cursor.y;
                        if dx != 0 || dy != 0 {
                            state.did_drag = true;
                            // Pulse：拖拽开始即收起详情卡（deselect）
                            if state.card_openness.target > 0.01 {
                                state.card_openness.set(0.0);
                                state.hover_index = None;
                            }
                        }
                        let mut new_x = state.drag_start_window.left + dx;
                        let mut new_y = state.drag_start_window.top + dy;
                        {
                            // 限制窗口留在指针所在屏内（跨屏拖拽不会算出屏外坐标）
                            if let Some(sg) = placement::monitor_at(cur) {
                                let ww = state.drag_start_window.right - state.drag_start_window.left;
                                let hh = state.drag_start_window.bottom - state.drag_start_window.top;
                                new_x = new_x.clamp(sg.full.left, (sg.full.right - ww).max(sg.full.left));
                                new_y = new_y.clamp(sg.full.top, (sg.full.bottom - hh).max(sg.full.top));
                            }
                        }

                        // ── 液态吸附（Pulse DropletTether）──
                        //
                        // 窗口**始终**跟着指针走（`new_x/new_y`），液颈只是贴边侧
                        // 画出来的装饰。早前让窗口真的滞后（lag），结果拖离贴边时
                        // 窗口停在旧位置、指针跑掉，再加上尺寸/轴向变化，
                        // 表现为「拖到一半突然跳走、不跟手」。视觉上的吸附感
                        // 由 drawn/thinning 两个值驱动绘制，不需要窗口让路。
                        state.pulled = 0.0;
                        state.thinning = 1.0;
                        SetWindowPos(hwnd, None, new_x, new_y, 0, 0, SWP_NOSIZE | SWP_NOZORDER | SWP_NOACTIVATE).ok();

                        // Pulse dragDock：拖拽中实时判定吸附边。
                        //
                        // **只记录，不应用。** 拖拽中一旦真的换 dock，宽高要互换
                        // （横条 402×78 ↔ 竖栏 64×502），窗口左上角跟着重算，
                        // 指针还在动、dock 又来回翻转 —— 用户看到的就是「拖到左半边
                        // 突然跳到右半边」「快到边时突然往下走」。松手时再真正吸附，
                        // 拖拽过程因此始终是一条跟着指针平移的栏，不再跳。
                        let rail = RECT {
                            left: new_x,
                            top: new_y,
                            right: new_x + (state.drag_start_window.right - state.drag_start_window.left),
                            bottom: new_y + (state.drag_start_window.bottom - state.drag_start_window.top),
                        };
                        let g = monitor_geom(hwnd);
                        let nd = placement::drag_dock(cur, rail, &g, state.dock, state.place.snaps);
                        state.drag_target = Some(nd);
                        return;
                    }

                    state.left_at = 0;
                    // 物理客户端坐标 → 规范空间（右边缘竖栏）
                    let (bu, bv) =
                        placement::canonical_size(state.dock, state.width as i32, state.height as i32);
                    let (x, y) = window_to_canon(state.dock, x, y, bu, bv);
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
                    let n = state.metrics.len().min(8);
                    let expanded_h = lm.expanded_length(state.dock, n);
                    let dock_top = (bv - expanded_h) / 2.0;
                    // 厚度轴：只有落在胶囊厚度内才算悬停到环（横栏 78，竖栏 64）。
                    let dock_left = bu - lm.cross_thickness(state.dock);

                    let mut hovered_slot = None;
                    if x >= dock_left && x <= bu {
                        for i in 0..n {
                            let cy = lm.ring_along(state.dock, dock_top, i);
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

            if dock_changed {
                // sync_window_size 内部已按 state.dragging 分岔：拖拽中只换尺寸、
                // 保持左上角，松开后位置由 WM_LBUTTONUP 对齐；非拖拽才 place_window。
                sync_window_size(hwnd);
                need_anim = true;
            }

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
                    let py = pt.y as f32;
                    // 胶囊在窗口里占的那一条带，用物理坐标算，而不是「离某条边多远」。
                    //
                    // 窗口比栏大：竖栏两侧留了卡片宽度，横栏上下留了卡片高度。
                    // 早前用 edge_dist（离停靠边的距离）判断，横向自由条因为
                    // 被归进「横栏 → 看 py」，要求 py <= 78，可栏实际在窗口
                    // 中间那条带上（展开态 y≈139..217），于是整条栏被判成透明，
                    // 鼠标穿到下层窗口，连拖都拖不动 —— 这就是「不会靠右」的
                    // 真正原因：拖拽压根没开始。
                    let (band_l, band_t, band_r, band_b) = capsule_band(
                        state.dock,
                        state.width,
                        state.height,
                        state.openness.cur,
                        state.metrics.len().min(8),
                        &lm,
                    );
                    let in_band = px >= band_l && px <= band_r && py >= band_t && py <= band_b;

                    if state.openness.cur < 0.3 {
                        // 收起态：细条本体命中即可
                        in_band
                    } else {
                        let card_hit = state.card_openness.cur > 0.1
                            && render::CARD_RECT.with(|c| {
                                let (l, t, rr, b) = c.get();
                                px >= l && px <= rr && py >= t && py <= b
                            });
                        in_band || card_hit
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
        WM_CAPTURECHANGED => {
            // 鼠标捕获丢了而 WM_LBUTTONUP 又不会到达本窗口时，dragging 会永久
            // 为真：此后每次 WM_MOUSEMOVE 都进拖拽分支、看门狗也因 `dragging`
            // 拒绝收起 —— 用户看到的就是「动也动不了」。
            //
            // 两个必须遵守的细节：
            // 1. `ReleaseCapture` 是同步派发这条消息的，可能发生在别处正可变
            //    借用 STATE 期间，所以用 try_borrow_mut，别 panic。
            // 2. 程序自己 ReleaseCapture 时会先置 `releasing_capture`，
            //    这条消息要认出来并跳过，否则正常松手也会被当成丢失。
            STATE.with(|s| {
                if let Ok(mut slot) = s.try_borrow_mut() {
                    if let Some(state) = slot.as_mut() {
                        if state.releasing_capture {
                            state.releasing_capture = false;
                            return;
                        }
                        if state.dragging {
                            state.dragging = false;
                            state.did_drag = false;
                            state.pulled = 0.0;
                            state.thinning = 1.0;
                            state.parting_since = 0;
                            state.parted = 0.0;
                            log_msg("capture lost mid-drag: dragging reset");
                        }
                    }
                }
            });
            LRESULT(0)
        }
        WM_MOUSEACTIVATE => {
            // 这个窗口是 SW_SHOWNOACTIVATE 显示、不抢焦点的置顶浮窗。
            // 不处理这条消息时，点它会走「先激活窗口」的默认路径，
            // WM_LBUTTONDOWN 在激活过程中被吞掉 —— 表现就是点了没反应、
            // 拖不动，只剩悬停还能用。返回 MA_NOACTIVATE 让点击直接
            // 派发到本窗口，不改变前台的焦点。
            LRESULT(MA_NOACTIVATE as isize)
        }
        WM_LBUTTONDOWN => {
            STATE.with(|s| {
                if let Some(state) = s.borrow_mut().as_mut() {
                    state.dragging = true;
                    GetCursorPos(&mut state.drag_start_cursor).ok();
                    GetWindowRect(hwnd, &mut state.drag_start_window).ok();
                }
            });
            // SetCapture 会同步派发 WM_CAPTURECHANGED，必须在 STATE 借用之外调用，
            // 否则那条消息里的 borrow 与本处冲突，直接 panic。
            SetCapture(hwnd);
            LRESULT(0)
        }
        WM_LBUTTONUP => {
            let ux = (lparam.0 & 0xFFFF) as i16 as f32;
            let uy = ((lparam.0 >> 16) & 0xFFFF) as i16 as f32;
            let mut refresh_req: Option<(String, String)> = None;
            let mut sweep_idx: Option<usize> = None;
            // ReleaseCapture 会同步派发 WM_CAPTURECHANGED，不能放在 STATE 借用内。
            // 先置「自己放」标志，那条消息才不会把正常松手当成捕获丢失。
            let was_dragging = STATE.with(|s| {
                let mut slot = s.borrow_mut();
                match slot.as_mut() {
                    Some(st) if st.dragging => {
                        st.releasing_capture = true;
                        true
                    }
                    _ => false,
                }
            });
            if was_dragging {
                ReleaseCapture().ok();
            }
            STATE.with(|s| {
                if let Some(state) = s.borrow_mut().as_mut() {
                    if state.dragging {
                        state.dragging = false;
                        // 兜底：正常情况下 WM_CAPTURECHANGED 已消费掉这个标志，
                        // 但那条消息若因借用冲突被跳过，这里必须清掉，否则
                        // 下一次真的丢失捕获时会被误判成「自己放的」而不重置。
                        state.releasing_capture = false;
                        // 拖拽结束：自由态保留当前位置；停靠态按比例贴回
                        state.parting_since = 0;
                        state.parted = 0.0;
                        state.pulled = 0.0;
                        state.thinning = 1.0;
                        if state.did_drag {
                            let mut wr = RECT::default();
                            GetWindowRect(hwnd, &mut wr).ok();
                            let g = monitor_geom(hwnd);
                            // 应用拖拽中判定出的目标停靠（拖拽期间只记录、不应用）。
                            if let Some(target) = state.drag_target.take() {
                                if target != state.dock {
                                    state.dock = target;
                                    state.place.set_dock(target);
                                }
                            }
                            let room = placement::room_for(state.dock, state.place.snaps, &g);
                            if state.dock.is_docked() {
                                // 停靠态：只更新沿边轴的比例，位置交给 place_window。
                                // 用**窗口里胶囊的实际位置**算，不用窗口左上角 ——
                                // 窗口含卡片空间，左上角与栏并非同一点。
                                let lm = LayoutMetrics::new(state.scale);
                                let (bl, bt, br, bb) = capsule_band(
                                    state.dock,
                                    (wr.right - wr.left) as f32,
                                    (wr.bottom - wr.top) as f32,
                                    state.openness.cur,
                                    state.metrics.len().min(8),
                                    &lm,
                                );
                                let band_len = match state.dock {
                                    Dock::Edge(Edge::Left) | Dock::Edge(Edge::Right) => bb - bt,
                                    _ => br - bl,
                                };
                                let band_pos = match state.dock {
                                    Dock::Edge(Edge::Left) | Dock::Edge(Edge::Right) => {
                                        wr.top as f32 + bt - room.top as f32
                                    }
                                    _ => wr.left as f32 + bl - room.left as f32,
                                };
                                let room_len = match state.dock {
                                    Dock::Edge(Edge::Left) | Dock::Edge(Edge::Right) => {
                                        (room.bottom - room.top) as f32
                                    }
                                    _ => (room.right - room.left) as f32,
                                };
                                let span = (room_len - band_len).max(1.0);
                                let ratio = (band_pos / span).clamp(0.0, 1.0) as f64;
                                match state.dock {
                                    Dock::Edge(Edge::Left) | Dock::Edge(Edge::Right) => {
                                        state.place.v = ratio
                                    }
                                    _ => state.place.h = ratio,
                                }
                                save_placement(&state.place.clone());
                                // 尺寸按新轴向重算由本函数末尾的 sync_window_size 完成。
                                // 不能在这里直接调它：它自己也要 borrow_mut，
                                // 在 STATE 可变借用内部重入会直接 panic（实测崩过）。
                            } else {
                                // 自由态：位置两轴都要记，否则重启回到旧位置。
                                // 同样按胶囊中心算：窗口比栏大，用窗口左上角会让
                                // 「悬停展开时窗口变高」把栏顶出屏幕、再被夹回来，
                                // 视觉上就是指针一碰栏就整条往上跳。
                                let lm = LayoutMetrics::new(state.scale);
                                let (bl, bt, br, bb) = capsule_band(
                                    state.dock,
                                    (wr.right - wr.left) as f32,
                                    (wr.bottom - wr.top) as f32,
                                    state.openness.cur,
                                    state.metrics.len().min(8),
                                    &lm,
                                );
                                let cx = wr.left as f32 + (bl + br) / 2.0;
                                let cy = wr.top as f32 + (bt + bb) / 2.0;
                                let (rh, rv) = placement::ratios_for_centre(
                                    cx,
                                    cy,
                                    room,
                                    br - bl,
                                    bb - bt,
                                );
                                state.place.h = rh;
                                state.place.v = rv;
                                save_placement(&state.place.clone());
                            }
                        } else if state.openness.cur > 0.9
                            && now_ms().saturating_sub(state.rail_opened_at) >= 300
                        {
                            // 点环刷新（Pulse ringClick）：短按 + 展开保护期 0.3s
                            // + 落点在环圆内（半径 ×1.08 容差）
                            let scale = state.scale;
                            let lm = LayoutMetrics::new(scale);
                            let (bu, bv) =
                                placement::canonical_size(state.dock, state.width as i32, state.height as i32);
                            let (ux, uy) = window_to_canon(state.dock, ux, uy, bu, bv);
                            let n = state.metrics.len().min(8);
                            let expanded_h = lm.expanded_length(state.dock, n);
                            let dock_top = (bv - expanded_h) / 2.0;
                            let cx = lm.ring_cross(state.dock, bu);
                            let hit_r = lm.ring_d / 2.0 * 1.08;
                            for i in 0..n {
                                let cy = lm.ring_along(state.dock, dock_top, i);
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
            sync_window_size(hwnd);
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
                state.place.bar_style == "segmented",
                state.dock,
                state.pulled,
                state.thinning,
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
        let cmd_up = 4;
        let cmd_down = 5;
        let cmd_reset_order = 6;
        let cmd_bar = 7;

        AppendMenuW(menu, MF_STRING, cmd_open, w!("打开管理面板")).ok();
        AppendMenuW(menu, MF_STRING, cmd_copy, w!("复制接口地址 (http://127.0.0.1:8313/v1)")).ok();
        AppendMenuW(menu, MF_SEPARATOR, 0, None).ok();
        AppendMenuW(menu, MF_STRING, cmd_up, w!("上移当前环")).ok();
        AppendMenuW(menu, MF_STRING, cmd_down, w!("下移当前环")).ok();
        AppendMenuW(menu, MF_STRING, cmd_reset_order, w!("重置顺序")).ok();
        AppendMenuW(menu, MF_STRING, cmd_bar, w!("切换进度条样式（连续/刻度）")).ok();
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
            4 | 5 => {
                // 上移/下移当前悬停环（Pulse PlacementPane 箭头重排）
                STATE.with(|s| {
                    if let Some(state) = s.borrow_mut().as_mut() {
                        let Some(hi) = state.hover_index else { return };
                        if hi == 0 && selected.0 as usize == 4 { return; }
                        if hi + 1 >= state.metrics.len() && selected.0 as usize == 5 { return; }
                        let j = if selected.0 as usize == 4 { hi - 1 } else { hi + 1 };
                        state.metrics.swap(hi, j);
                        state.anims.swap(hi, j);
                        state.hover_index = Some(j);
                        state.place.order = state.metrics.iter().map(|x| x.provider.clone()).collect();
                        save_placement(&state.place.clone());
                    }
                });
                draw_now();
            }
            6 => {
                // 重置顺序：回到 metrics.rs 的默认 provider 序
                STATE.with(|s| {
                    if let Some(state) = s.borrow_mut().as_mut() {
                        state.place.order.clear();
                        let order = ["qwen", "workbuddy", "trae", "qoder", "qwenwork", "doubao"];
                        let mut pairs: Vec<(usize, Metric, RingAnimState)> = state
                            .metrics
                            .iter()
                            .zip(state.anims.iter())
                            .map(|(mm, aa)| (order.iter().position(|p| *p == mm.provider).unwrap_or(usize::MAX), mm.clone(), aa.clone()))
                            .collect();
                        pairs.sort_by_key(|(k, _, _)| *k);
                        state.metrics = pairs.iter().map(|(_, mm, _)| mm.clone()).collect();
                        state.anims = pairs.iter().map(|(_, _, aa)| aa.clone()).collect();
                        save_placement(&state.place.clone());
                    }
                });
                draw_now();
            }
            7 => {
                // 切换进度条样式（continuous <-> segmented），持久化
                STATE.with(|s| {
                    if let Some(state) = s.borrow_mut().as_mut() {
                        state.place.bar_style = if state.place.bar_style == "segmented" {
                            "continuous".to_string()
                        } else {
                            "segmented".to_string()
                        };
                        save_placement(&state.place.clone());
                    }
                });
                draw_now();
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
