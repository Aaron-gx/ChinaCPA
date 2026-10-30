//! 停靠布局：四边 Dock + 自由态 + 位置比例持久化（rail.json）。
//! 移植 Pulse PanelPlacement 的判定几何（y-down 屏幕坐标）。

use serde::{Deserialize, Serialize};
use windows::Win32::Foundation::{HWND, POINT, RECT};
use windows::Win32::Graphics::Gdi::{
    GetMonitorInfoW, MonitorFromPoint, MonitorFromWindow, HMONITOR, MONITORINFO,
    MONITOR_DEFAULTTOPRIMARY,
};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Edge {
    Left,
    Right,
    Top,
    Bottom,
}

impl Edge {
    pub fn vertical(self) -> bool {
        matches!(self, Edge::Left | Edge::Right)
    }
    pub fn as_str(self) -> &'static str {
        match self {
            Edge::Left => "left",
            Edge::Right => "right",
            Edge::Top => "top",
            Edge::Bottom => "bottom",
        }
    }
    pub fn from_str(s: &str) -> Option<Edge> {
        match s {
            "left" => Some(Edge::Left),
            "right" => Some(Edge::Right),
            "top" => Some(Edge::Top),
            "bottom" => Some(Edge::Bottom),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Dock {
    Edge(Edge),
    /// 自由态：保留脱离前的轴向（竖柱 / 横条）
    Floating(bool), // true = vertical
}

impl Dock {
    pub fn edge(self) -> Option<Edge> {
        match self {
            Dock::Edge(e) => Some(e),
            Dock::Floating(_) => None,
        }
    }
    pub fn vertical(self) -> bool {
        match self {
            Dock::Edge(e) => e.vertical(),
            Dock::Floating(v) => v,
        }
    }
    /// 栏横置：环与文字沿厚度轴上下排布。
    ///
    /// **必须与 `canon_matrix` 的轴向分组完全一致**。那里把 `Floating(false)`
    /// 与 `Top` 归为一类（都要转 90° 画），这里若只认 `Top`/`Bottom`，同一根
    /// 自由横条就会「按横栏绘制、按竖栏定窗口尺寸」—— 画出来的东西跑到窗口
    /// 外面，只剩左上角一块碎片。从顶栏拖离屏幕边得到的正是 `Floating(false)`。
    pub fn lies_across(self) -> bool {
        matches!(
            self,
            Dock::Edge(Edge::Top) | Dock::Edge(Edge::Bottom) | Dock::Floating(false)
        )
    }
    pub fn is_docked(self) -> bool {
        matches!(self, Dock::Edge(_))
    }
    pub fn as_str(self) -> String {
        match self {
            Dock::Edge(e) => e.as_str().to_string(),
            Dock::Floating(true) => "float_v".to_string(),
            Dock::Floating(false) => "float_h".to_string(),
        }
    }
    pub fn from_str(s: &str) -> Option<Dock> {
        match s {
            "float_v" => Some(Dock::Floating(true)),
            "float_h" => Some(Dock::Floating(false)),
            other => Edge::from_str(other).map(Dock::Edge),
        }
    }
}

/// 吸附阈值（Pulse dockDistance = 32pt）
pub const DOCK_DISTANCE: f32 = 32.0;

/// rail.json 持久化结构
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct Placement {
    pub dock: String,
    /// rail 在参照系两轴的位置比例（0..1）
    pub h: f64,
    pub v: f64,
    pub display: String,
    /// 环顺序（provider id）
    pub order: Vec<String>,
    /// 进度条样式 continuous / segmented
    pub bar_style: String,
    pub auto_collapse: bool,
    pub snaps: bool,
}

impl Default for Placement {
    fn default() -> Self {
        Self {
            dock: "right".to_string(),
            h: 1.0,
            v: 0.5,
            display: String::new(),
            order: Vec::new(),
            bar_style: "continuous".to_string(),
            auto_collapse: true,
            snaps: true,
        }
    }
}

impl Placement {
    pub fn dock(&self) -> Dock {
        Dock::from_str(&self.dock).unwrap_or(Dock::Edge(Edge::Right))
    }
    pub fn set_dock(&mut self, d: Dock) {
        self.dock = d.as_str();
    }
}

fn config_path() -> std::path::PathBuf {
    if let Ok(mut exe) = std::env::current_exe() {
        exe.set_file_name("rail.json");
        exe
    } else {
        std::path::PathBuf::from("rail.json")
    }
}

pub fn load_placement() -> Placement {
    let path = config_path();
    std::fs::read_to_string(&path)
        .ok()
        .and_then(|s| serde_json::from_str::<Placement>(&s).ok())
        .unwrap_or_default()
}

pub fn save_placement(p: &Placement) {
    let path = config_path();
    if let Ok(s) = serde_json::to_string_pretty(p) {
        let _ = std::fs::write(path, s);
    }
}

/// 屏幕几何：整屏 + 工作区（去掉任务栏）
#[derive(Debug, Clone, Copy)]
pub struct ScreenGeom {
    pub full: RECT,
    pub work: RECT,
}

pub fn monitor_geom(hwnd: HWND) -> ScreenGeom {
    unsafe {
        let mon: HMONITOR = MonitorFromWindow(hwnd, MONITOR_DEFAULTTOPRIMARY);
        let mut mi = MONITORINFO {
            cbSize: std::mem::size_of::<MONITORINFO>() as u32,
            ..Default::default()
        };
        if GetMonitorInfoW(mon, &mut mi).as_bool() {
            return ScreenGeom {
                full: mi.rcMonitor,
                work: mi.rcWork,
            };
        }
        ScreenGeom {
            full: RECT {
                left: 0,
                top: 0,
                right: 1920,
                bottom: 1080,
            },
            work: RECT {
                left: 0,
                top: 0,
                right: 1920,
                bottom: 1040,
            },
        }
    }
}

pub fn monitor_at(pt: POINT) -> Option<ScreenGeom> {
    unsafe {
        let mon = MonitorFromPoint(pt, MONITOR_DEFAULTTOPRIMARY);
        let mut mi = MONITORINFO {
            cbSize: std::mem::size_of::<MONITORINFO>() as u32,
            ..Default::default()
        };
        if GetMonitorInfoW(mon, &mut mi).as_bool() {
            return Some(ScreenGeom {
                full: mi.rcMonitor,
                work: mi.rcWork,
            });
        }
        None
    }
}

/// 拖拽中的实时吸附判定（Pulse dragDock 的移植）。
///
/// 判据取「指针到屏幕边」与「栏的可视边缘到屏幕边」中**较近的那个**。
///
/// 只看指针时，横向长栏吸不上侧边：它展开有 400pt 宽，抓中段往右拖，
/// 栏的右缘已经贴住屏幕右边，指针却还差两百多点，永远过不了 32pt 阈值
/// —— 用户看到的就是「栏明明贴边了却不靠右」。竖栏窄，指针本来就在栏上，
/// 两个判据结果一致，所以加这一条不影响它。
///
/// 拖拽时窗口就是栏本体（卡片在按下时已收起，不占额外宽度），
/// `rail` 因此可以直接当作栏的可视边缘使用。
pub fn drag_dock(
    pointer: POINT,
    rail: RECT,
    screen: &ScreenGeom,
    current: Dock,
    snaps: bool,
) -> Dock {
    let vertical = current.vertical();
    if !snaps {
        return Dock::Floating(vertical);
    }
    let px = pointer.x as f32;
    let py = pointer.y as f32;
    let work = screen.work;

    // 每条边的判据分两种情况：
    //  * 当前就停在这条边 → 只看**指针**。栏本身贴着边，用栏边缘会恒判贴边、
    //    永远拖不下来（这是「贴边就卡住」的成因）。
    //  * 否则 → 取指针与栏边缘中**更近**的一个。长横条拖向侧边时，栏的右缘
    //    先够到屏幕边、指针还差两百多点，用 max 就永远吸不上（这是
    //    「不会靠右」的成因）。
    let near_top = if current == Dock::Edge(Edge::Top) {
        py - work.top as f32
    } else {
        (py - work.top as f32).min((rail.top - work.top) as f32)
    };
    let near_left = if current == Dock::Edge(Edge::Left) {
        px - work.left as f32
    } else {
        (px - work.left as f32).min((rail.left - work.left) as f32)
    };
    let near_bottom = if current == Dock::Edge(Edge::Bottom) {
        work.bottom as f32 - py
    } else {
        (work.bottom as f32 - py).min((work.bottom - rail.bottom) as f32)
    };
    let near_right = if current == Dock::Edge(Edge::Right) {
        work.right as f32 - px
    } else {
        (work.right as f32 - px).min((work.right - rail.right) as f32)
    };

    // 已停靠底部时保持：贴着屏幕自己的底边（不是工作区）就算还在底部。
    let stays_on_bottom =
        current == Dock::Edge(Edge::Bottom) && (screen.full.bottom - rail.bottom) as f32 <= DOCK_DISTANCE;

    // 候选边按**距离**选最近的那条，而不是固定顺序。
    // 固定「顶优先」时，拖到右上角（指针 y≈0、x≈屏幕右边）永远判成顶栏，
    // 用户的意图明明是靠右。
    //
    // 当前已停靠的边给一个滞回余量：否则在两个阈值都满足的角上会来回翻，
    // 每翻一次宽高就互换一次，看起来就是在抖动。
    let mut best: Option<(f32, Dock)> = None;
    let mut consider = |dist: f32, edge: Edge, cur_bias: f32| {
        if dist <= DOCK_DISTANCE {
            let d = dist - cur_bias;
            if best.map(|(bd, _)| d < bd).unwrap_or(true) {
                best = Some((d, Dock::Edge(edge)));
            }
        }
    };
    let bias = |e: Edge| if current == Dock::Edge(e) { DOCK_DISTANCE * 0.5 } else { 0.0 };
    consider(near_top, Edge::Top, bias(Edge::Top));
    consider(near_left, Edge::Left, bias(Edge::Left));
    consider(near_right, Edge::Right, bias(Edge::Right));
    if stays_on_bottom {
        consider(near_bottom, Edge::Bottom, DOCK_DISTANCE * 0.5);
    } else {
        consider(near_bottom, Edge::Bottom, bias(Edge::Bottom));
    }

    match best {
        Some((_, dock)) => dock,
        None => Dock::Floating(vertical),
    }
}

/// 参照系：停靠/开吸附 → 工作区；自由关吸附 → 整屏（可压过任务栏，Pulse #85）
pub fn room_for(dock: Dock, snaps: bool, g: &ScreenGeom) -> RECT {
    match dock {
        Dock::Edge(_) => g.work,
        Dock::Floating(_) if snaps => g.work,
        Dock::Floating(_) => g.full,
    }
}

/// rail 左下角在参照系两轴的比例（Pulse ratios()，y-down 版）
pub fn ratios(rail: RECT, room: RECT, rail_w: f32, rail_h: f32) -> (f64, f64) {
    let h = if (room.right - room.left) as f32 > rail_w {
        (rail.left - room.left) as f64 / ((room.right - room.left) as f64 - rail_w as f64)
    } else {
        0.5
    };
    let v = if (room.bottom - room.top) as f32 > rail_h {
        (rail.top - room.top) as f64 / ((room.bottom - room.top) as f64 - rail_h as f64)
    } else {
        0.5
    };
    (h.clamp(0.0, 1.0), v.clamp(0.0, 1.0))
}

/// 由比例反推 rail 左上角（屏幕坐标）
pub fn position_from_ratios(h: f64, v: f64, room: RECT, rail_w: i32, rail_h: i32) -> (i32, i32) {
    let x = room.left as f64 + h * ((room.right - room.left) as f64 - rail_w as f64);
    let y = room.top as f64 + v * ((room.bottom - room.top) as f64 - rail_h as f64);
    (x.round() as i32, y.round() as i32)
}

/// 按**胶囊中心**反推比例。
///
/// 自由态存的是栏的位置，不是窗口的位置：窗口含卡片空间，收起/展开时
/// 宽高会变（细条 6pt、展开 342pt），若按窗口左上角记录，同一个比例在
/// 不同尺寸下指向不同的栏位置 —— 悬停展开时栏会被挤出屏幕再夹回来，
/// 视觉上就是「指针一碰栏，整条突然往上跑」。
/// 用胶囊中心做锚点，尺寸怎么变，栏都停在同一处。
pub fn ratios_for_centre(
    centre_x: f32,
    centre_y: f32,
    room: RECT,
    rail_w: f32,
    rail_h: f32,
) -> (f64, f64) {
    let room_w = (room.right - room.left) as f32;
    let room_h = (room.bottom - room.top) as f32;
    let h = if room_w > rail_w {
        ((centre_x - room.left as f32) - rail_w / 2.0) / (room_w - rail_w)
    } else {
        0.5
    };
    let v = if room_h > rail_h {
        ((centre_y - room.top as f32) - rail_h / 2.0) / (room_h - rail_h)
    } else {
        0.5
    };
    (h.clamp(0.0, 1.0) as f64, v.clamp(0.0, 1.0) as f64)
}

/// 规范空间（右边缘竖栏）尺寸 → 窗口物理尺寸。
/// 横置的栏（顶/底/自由横条）交换宽高。
pub fn window_size(dock: Dock, canon_w: i32, canon_h: i32) -> (i32, i32) {
    if dock.lies_across() {
        (canon_h, canon_w)
    } else {
        (canon_w, canon_h)
    }
}

/// 窗口物理尺寸 → 规范空间尺寸（绘制用 U,V）。
pub fn canonical_size(dock: Dock, win_w: i32, win_h: i32) -> (f32, f32) {
    if dock.lies_across() {
        (win_h as f32, win_w as f32)
    } else {
        (win_w as f32, win_h as f32)
    }
}

/// 规范空间 (u,v) → 窗口物理 (x,y)。BU/BV = 规范空间宽高（含卡片空间）。
/// 规范空间约定：u=横向（0=卡片侧，BU=屏幕边），v=沿栏纵向。
pub fn canon_to_window(dock: Dock, u: f32, v: f32, bu: f32, bv: f32) -> (f32, f32) {
    match dock {
        Dock::Edge(Edge::Right) | Dock::Floating(true) => (u, v),
        Dock::Edge(Edge::Left) => (bu - u, v),
        Dock::Edge(Edge::Top) | Dock::Floating(false) => (v, bu - u),
        Dock::Edge(Edge::Bottom) => (bv - v, u),
    }
}

/// 窗口物理 (x,y) → 规范空间 (u,v)（命中检测逆变换）。
pub fn window_to_canon(dock: Dock, x: f32, y: f32, bu: f32, bv: f32) -> (f32, f32) {
    match dock {
        Dock::Edge(Edge::Right) | Dock::Floating(true) => (x, y),
        Dock::Edge(Edge::Left) => (bu - x, y),
        Dock::Edge(Edge::Top) | Dock::Floating(false) => (bu - y, x),
        Dock::Edge(Edge::Bottom) => (y, bv - x),
    }
}

/// 规范空间 → 窗口物理的 D2D 仿射矩阵（形状绘制用；文字不走矩阵保持正立）。
///
/// D2D 约定：x' = x·M11 + y·M21 + M31；y' = x·M12 + y·M22 + M32。
/// 必须与 canon_to_window 完全一致，否则形状会画到窗口外。
pub fn canon_matrix(dock: Dock, bu: f32, bv: f32) -> windows::Foundation::Numerics::Matrix3x2 {
    use windows::Foundation::Numerics::Matrix3x2;
    match dock {
        // (u,v) → (u,v)
        Dock::Edge(Edge::Right) | Dock::Floating(true) => {
            Matrix3x2 { M11: 1.0, M12: 0.0, M21: 0.0, M22: 1.0, M31: 0.0, M32: 0.0 }
        }
        // (u,v) → (bu-u, v)：水平镜像
        Dock::Edge(Edge::Left) => {
            Matrix3x2 { M11: -1.0, M12: 0.0, M21: 0.0, M22: 1.0, M31: bu, M32: 0.0 }
        }
        // (u,v) → (v, bu-u)：u=bu（贴屏边）→ y=0，u=0（卡片侧）→ y=bu
        Dock::Edge(Edge::Top) | Dock::Floating(false) => {
            Matrix3x2 { M11: 0.0, M12: -1.0, M21: 1.0, M22: 0.0, M31: 0.0, M32: bu }
        }
        // (u,v) → (bv-v, u)
        Dock::Edge(Edge::Bottom) => {
            Matrix3x2 { M11: 0.0, M12: 1.0, M21: -1.0, M22: 0.0, M31: bv, M32: 0.0 }
        }
    }
}


/// 规范矩形 (l,t,r,b) → 物理矩形（四边旋转/镜像后仍轴对齐，取对角归一）。
pub fn map_rect(dock: Dock, l: f32, t: f32, r: f32, b: f32, bu: f32, bv: f32) -> (f32, f32, f32, f32) {
    let (x1, y1) = canon_to_window(dock, l, t, bu, bv);
    let (x2, y2) = canon_to_window(dock, r, b, bu, bv);
    (x1.min(x2), y1.min(y2), x1.max(x2), y1.max(y2))
}
