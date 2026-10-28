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

/// 拖拽中的实时吸附判定（Pulse dragDock 的 y-down 移植）。
/// pointer/rail 为屏幕坐标；vertical 为 rail 当前轴。
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
    let (l, r) = if vertical {
        (rail.left as f32, rail.right as f32)
    } else {
        (pointer.x as f32, pointer.x as f32)
    };
    let stays_on_bottom =
        current == Dock::Edge(Edge::Bottom) && (screen.full.bottom - rail.bottom) as f32 <= DOCK_DISTANCE;
    if (pointer.y as f32 - screen.work.top as f32) <= DOCK_DISTANCE {
        Dock::Edge(Edge::Top)
    } else if stays_on_bottom || (screen.full.bottom - pointer.y) as f32 <= DOCK_DISTANCE {
        Dock::Edge(Edge::Bottom)
    } else if (screen.work.bottom - pointer.y) as f32 <= DOCK_DISTANCE {
        // 指针进入任务栏条带 → 自由横条（Pulse：Dock 上方）
        Dock::Floating(false)
    } else if (l - screen.work.left as f32) <= DOCK_DISTANCE {
        Dock::Edge(Edge::Left)
    } else if (screen.work.right as f32 - r) <= DOCK_DISTANCE {
        Dock::Edge(Edge::Right)
    } else {
        Dock::Floating(vertical)
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

/// 规范空间（右边缘竖栏）尺寸 → 窗口物理尺寸。
/// Top/Bottom 需交换宽高。
pub fn window_size(dock: Dock, canon_w: i32, canon_h: i32) -> (i32, i32) {
    match dock {
        Dock::Edge(Edge::Top) | Dock::Edge(Edge::Bottom) => (canon_h, canon_w),
        _ => (canon_w, canon_h),
    }
}

/// 窗口物理尺寸 → 规范空间尺寸（绘制用 U,V）。
pub fn canonical_size(dock: Dock, win_w: i32, win_h: i32) -> (f32, f32) {
    match dock {
        Dock::Edge(Edge::Top) | Dock::Edge(Edge::Bottom) => (win_h as f32, win_w as f32),
        _ => (win_w as f32, win_h as f32),
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
pub fn canon_matrix(dock: Dock, bu: f32, bv: f32) -> windows::Foundation::Numerics::Matrix3x2 {
    use windows::Foundation::Numerics::Matrix3x2;
    match dock {
        Dock::Edge(Edge::Right) | Dock::Floating(true) => Matrix3x2 { M11: 1.0, M12: 0.0, M21: 0.0, M22: 1.0, M31: 0.0, M32: 0.0 },
        Dock::Edge(Edge::Left) => Matrix3x2 { M11: -1.0, M12: 0.0, M21: 0.0, M22: 1.0, M31: bu, M32: 0.0 },
        Dock::Edge(Edge::Top) | Dock::Floating(false) => Matrix3x2 { M11: 0.0, M12: 1.0, M21: -1.0, M22: 0.0, M31: 0.0, M32: bu },
        Dock::Edge(Edge::Bottom) => Matrix3x2 { M11: 0.0, M12: -1.0, M21: 1.0, M22: 0.0, M31: bv, M32: 0.0 },
    }
}


/// 规范矩形 (l,t,r,b) → 物理矩形（四边旋转/镜像后仍轴对齐，取对角归一）。
pub fn map_rect(dock: Dock, l: f32, t: f32, r: f32, b: f32, bu: f32, bv: f32) -> (f32, f32, f32, f32) {
    let (x1, y1) = canon_to_window(dock, l, t, bu, bv);
    let (x2, y2) = canon_to_window(dock, r, b, bu, bv);
    (x1.min(x2), y1.min(y2), x1.max(x2), y1.max(y2))
}
