use windows::{
    core::*,
    Win32::Graphics::Direct2D::*,
    Win32::Graphics::Direct2D::Common::*,
    Win32::Graphics::DirectWrite::*,
    Win32::Foundation::HWND,
};

use windows::Foundation::Numerics::Matrix3x2;

/// draw_detail_card 每帧写入的卡片物理矩形（含尾巴膨胀），供 NCHITTEST 命中。
thread_local! {
    pub static CARD_RECT: std::cell::Cell<(f32, f32, f32, f32)> =
        const { std::cell::Cell::new((0.0, 0.0, 0.0, 0.0)) };
}

const IDENTITY: Matrix3x2 = Matrix3x2 { M11: 1.0, M12: 0.0, M21: 0.0, M22: 1.0, M31: 0.0, M32: 0.0 };

use crate::graphics::D2DEngine;
use crate::metrics::Metric;
use crate::placement::{canon_matrix, canon_to_window, Dock, Edge};
use crate::spring::{FadeValue, SpringValue};

#[derive(Clone)]
pub struct RingAnimState {
    pub arc: SpringValue,
    pub halo: FadeValue,
    /// 刷新扫光进度：0=无，>0 时沿环走一圈（16% 弧，0.85s）。点环刷新后触发。
    pub sweep: f32,
    /// 窗口时钟弧平滑值（reset 跳回时 easeOut 过渡）
    pub clock: SpringValue,
}

impl RingAnimState {
    pub fn new(val: f32) -> Self {
        Self {
            arc: SpringValue::new(val),
            halo: FadeValue::new(0.0, 5.0),
            sweep: 0.0,
            clock: SpringValue::new(0.0),
        }
    }
}

pub struct LayoutMetrics {
    pub scale: f32,
    pub pill_w: f32,
    pub sliver_w: f32,
    pub sliver_h: f32,
    pub flare_w: f32,
    pub flare_h: f32,
    pub corner_r: f32,
    pub ring_d: f32,
    pub ring_line: f32,
    pub item_spacing: f32,
    pub card_w: f32,
    pub card_pad: f32,
    pub card_radius: f32,
    pub pointer_w: f32,
    pub pointer_h: f32,
}

impl LayoutMetrics {
    pub fn new(scale: f32) -> Self {
        Self {
            scale,
            pill_w: 64.0 * scale,
            sliver_w: 6.0 * scale,
            sliver_h: 96.0 * scale,
            flare_w: 38.0 * scale,
            flare_h: 24.0 * scale,
            corner_r: 26.0 * scale,
            ring_d: 36.0 * scale,
            ring_line: 4.0 * scale,
            item_spacing: 30.0 * scale,
            card_w: 250.0 * scale,
            card_pad: 18.0 * scale,
            card_radius: 20.0 * scale,
            pointer_w: 20.0 * scale,
            pointer_h: 40.0 * scale,
        }
    }
}

pub fn render_frame(
    eng: &D2DEngine,
    metrics: &[Metric],
    anims: &[RingAnimState],
    hover_index: Option<usize>,
    openness: f32,
    card_openness: f32,
    pointer_y: f32,
    busy_spin: f32,
    w: f32,
    h: f32,
    scale: f32,
    segmented: bool,
    dock: Dock,
    pulled: f32,
    thinning: f32,
    hwnd: HWND,
) -> Result<()> {
    unsafe {
        let rt = &eng.rt;
        rt.BeginDraw();
        rt.Clear(Some(&D2D1_COLOR_F { r: 0.0, g: 0.0, b: 0.0, a: 0.0 }));

        let lm = LayoutMetrics::new(scale);
        let n = metrics.len().min(8);
        // 规范空间恒为"右边缘竖栏"；四边靠仿射变换映射到物理窗口。
        // 形状在变换下绘制，文字单独用恒等变换保持正立（见各 draw_* 内 SetTransform）。
        let (bu, bv) = match dock {
            Dock::Edge(Edge::Top) | Dock::Edge(Edge::Bottom) => (h, w),
            _ => (w, h),
        };
        let m3x2 = canon_matrix(dock, bu, bv);
        // 形状在规范→物理变换下绘制；文字/图标切回恒等变换保持正立。
        rt.SetTransform(&m3x2);
        if n == 0 {
            rt.EndDraw(None, None)?;
            eng.present(hwnd)?;
            return Ok(());
        }

        // 1. 动态形态插值（完全匹配 Pulse 的 DockBerthShape）
        let slot_h = lm.ring_d + 6.0 * scale + 16.0 * scale + lm.item_spacing;
        let total_content_h = (n as f32) * slot_h - lm.item_spacing;
        let expanded_h = total_content_h + 46.0 * scale * 2.0; // Pulse: verticalPadding = 46
        let collapsed_h = lm.sliver_h;

        let cur_w = lm.sliver_w + (lm.pill_w - lm.sliver_w) * openness;
        let cur_h = collapsed_h + (expanded_h - collapsed_h) * openness;

        // 规范空间几何（bu=沿厚度轴宽, bv=沿长度轴高）
        // 液颈拉伸：pulled>0 时 rail 整体从贴边侧内移 drawn 距离，
        // 并在贴边侧画一条收窄的液颈连回边缘（Pulse DropletTether）。
        let rest = 1.0 - (pulled / 80.0).clamp(0.0, 1.0);
        let drawn = pulled * (1.0 - (1.0 - 0.15) * rest * rest);
        let necking = thinning.clamp(0.0, 1.0);
        let dock_top = (bv - cur_h) / 2.0;
        let dock_bottom = dock_top + cur_h;
        let dock_left = bu - cur_w - drawn;
        let dock_right = bu - drawn;

        // 2. 绘制 Pulse S 型贴边凹弧胶囊
        //    收起态告警染色（Pulse alertTint）：sliver 在最高额度 ≥ 阈值或耗尽
        //    时整条染告警色；展开即回黑（环本身携带状态）。
        //    balance（积分余额型）不参与告警折叠：其 used_frac 恒为 1.0 是
        //    "有余额"而非"快用完"，染色会误报（Pulse 用 isExhausted 表耗尽）。
        let alert = if openness < 0.3 {
            let worst = metrics.iter().fold(None::<&Metric>, |acc, m| {
                if m.balance {
                    return acc;
                }
                match acc {
                    None => Some(m),
                    Some(a) if m.used > a.used => Some(m),
                    Some(a) => Some(a),
                }
            });
            match worst {
                Some(m) if m.spent || m.used >= 0.75 => {
                    Some(eng.color_for_fraction(m.used as f32, false, m.spent))
                }
                _ => None,
            }
        } else {
            None
        };
        draw_pulse_berth_shape(
            eng,
            dock_left,
            dock_top,
            dock_right,
            dock_bottom,
            openness,
            alert,
            &lm,
        )?;

        // 液颈：把 rail 贴边侧拉一条收窄的桥接到屏幕边缘（Pulse fluidTurn 近似）。
        // waistHalf 随张力收细（1-u^2.2 的精简版：necking 越小越细）。
        if drawn > 0.5 {
            let half_h = cur_h * 0.5;
            let waist = half_h * (0.45 * necking).clamp(0.02, 0.45);
            let neck = eng.d2d_factory.CreatePathGeometry()?;
            let s = neck.Open()?;
            let (cy, bx) = ((dock_top + dock_bottom) / 2.0, dock_right);
            s.BeginFigure(D2D_POINT_2F { x: bx, y: cy - waist }, D2D1_FIGURE_BEGIN_FILLED);
            s.AddLine(D2D_POINT_2F { x: bu, y: cy - waist * 0.7 });
            s.AddLine(D2D_POINT_2F { x: bu, y: cy + waist * 0.7 });
            s.AddLine(D2D_POINT_2F { x: bx, y: cy + waist });
            s.EndFigure(D2D1_FIGURE_END_CLOSED);
            s.Close()?;
            rt.FillGeometry(&neck, alert.unwrap_or(&eng.brush_rail), None);
        }

        // 3. 闲置状态居中彩色呼吸指示条（openness < 0.6 时平滑呈现）
        if openness < 0.6 {
            let cx = dock_left + cur_w / 2.0;
            let line_col = if let Some(m) = metrics.first() {
                eng.color_for_fraction(m.used as f32, m.balance, m.spent)
            } else {
                &eng.brush_track
            };
            rt.DrawLine(
                D2D_POINT_2F { x: cx, y: dock_top + 16.0 * scale },
                D2D_POINT_2F { x: cx, y: dock_bottom - 16.0 * scale },
                line_col,
                1.5 * scale,
                &eng.stroke_round,
            );
        }

        // 4. 绘制各模型槽位（openness > 0.35 时随胶囊展开渐现）
        let ring_alpha = ((openness - 0.35) / 0.65).clamp(0.0, 1.0);
        let mut slot_centers = Vec::with_capacity(n);
        let start_y = dock_top + 46.0 * scale + lm.ring_d / 2.0;
        for i in 0..n {
            slot_centers.push(start_y + (i as f32) * slot_h);
        }

        if ring_alpha > 0.01 {
            for (i, m) in metrics.iter().take(n).enumerate() {
                let cy = slot_centers[i];
                let cx = dock_left + cur_w / 2.0;
                let cur_val = anims.get(i).map(|a| a.arc.cur).unwrap_or(m.used as f32);
                let halo_val = anims.get(i).map(|a| a.halo.cur).unwrap_or(0.0);
                let sweep_val = anims.get(i).map(|a| a.sweep).unwrap_or(0.0);
                let clock_val = anims.get(i).map(|a| a.clock.cur).unwrap_or(-1.0);

                draw_usage_ring(
                    eng,
                    cx,
                    cy,
                    m,
                    cur_val,
                    halo_val,
                    hover_index == Some(i),
                    ring_alpha,
                    busy_spin,
                    sweep_val,
                    clock_val,
                    dock,
                    bu,
                    bv,
                    &m3x2,
                    &lm,
                )?;
            }
        }

        // 5. 详情卡片：物理空间正立布局（文字恒水平，四边通用），
        //    水滴尾巴指向悬停环（先映射环心到物理坐标）。
        if card_openness > 0.01 && hover_index.is_some() {
            let idx = hover_index.unwrap().min(n - 1);
            let m = &metrics[idx];
            let target_anchor_y = slot_centers[idx];
            let active_pointer = if pointer_y > 0.0 { pointer_y } else { target_anchor_y };
            let (pcx, pcy) = canon_to_window(dock, dock_left + cur_w / 2.0, active_pointer, bu, bv);
            let side: u8 = match dock {
                Dock::Edge(Edge::Right) | Dock::Floating(true) => 0,
                Dock::Edge(Edge::Left) => 1,
                Dock::Edge(Edge::Top) | Dock::Floating(false) => 2,
                Dock::Edge(Edge::Bottom) => 3,
            };
            draw_detail_card(eng, m, pcx, pcy, w, h, side, card_openness, segmented, &lm)?;
        }

        rt.SetTransform(&IDENTITY);
        rt.EndDraw(None, None)?;
        eng.present(hwnd)?;
    }
    Ok(())
}

unsafe fn draw_pulse_berth_shape(
    eng: &D2DEngine,
    left: f32,
    top: f32,
    right: f32,
    bottom: f32,
    openness: f32,
    alert: Option<&ID2D1SolidColorBrush>,
    lm: &LayoutMetrics,
) -> Result<()> {
    let rt = &eng.rt;
    let path = eng.d2d_factory.CreatePathGeometry()?;
    let sink = path.Open()?;

    let w = right - left;
    let h = bottom - top;

    let f = (lm.flare_h * openness).min(h / 2.0);
    let fw = (lm.flare_w * openness).min(w * 0.7).max(0.0);
    let r = ((lm.corner_r * openness + (lm.sliver_w / 2.0) * (1.0 - openness)).min(w / 2.0)).min((h - f * 2.0) / 2.0).max(0.0);

    // 1. 起始于顶部直边左端: (left + r, top + f)
    sink.BeginFigure(D2D_POINT_2F { x: left + r, y: top + f }, D2D1_FIGURE_BEGIN_FILLED);

    // 2. 直线至凹弧起点: (right - fw, top + f)
    if fw > 0.5 {
        sink.AddLine(D2D_POINT_2F { x: right - fw, y: top + f });
        // 3. 顶部外凹弧过渡至屏幕右边缘: 从 (right - fw, top + f) 到 (right, top)
        sink.AddBezier(&D2D1_BEZIER_SEGMENT {
            point1: D2D_POINT_2F { x: right - fw + fw * 0.55, y: top + f },
            point2: D2D_POINT_2F { x: right, y: top + f * 0.45 },
            point3: D2D_POINT_2F { x: right, y: top },
        });
    } else {
        sink.AddLine(D2D_POINT_2F { x: right, y: top });
    }

    // 4. 沿屏幕边缘直下
    sink.AddLine(D2D_POINT_2F { x: right, y: bottom });

    // 5. 底部外凹弧过渡: 从 (right, bottom) 到 (right - fw, bottom - f)
    if fw > 0.5 {
        sink.AddBezier(&D2D1_BEZIER_SEGMENT {
            point1: D2D_POINT_2F { x: right, y: bottom - f * 0.45 },
            point2: D2D_POINT_2F { x: right - fw + fw * 0.55, y: bottom - f },
            point3: D2D_POINT_2F { x: right - fw, y: bottom - f },
        });
        sink.AddLine(D2D_POINT_2F { x: left + r, y: bottom - f });
    } else {
        sink.AddLine(D2D_POINT_2F { x: left + r, y: bottom });
    }

    // 6. 左下圆角
    sink.AddArc(&D2D1_ARC_SEGMENT {
        point: D2D_POINT_2F { x: left, y: bottom - f - r },
        size: D2D_SIZE_F { width: r, height: r },
        rotationAngle: 0.0,
        sweepDirection: D2D1_SWEEP_DIRECTION_CLOCKWISE,
        arcSize: D2D1_ARC_SIZE_SMALL,
    });

    // 7. 左侧直边
    sink.AddLine(D2D_POINT_2F { x: left, y: top + f + r });

    // 8. 左上圆角
    sink.AddArc(&D2D1_ARC_SEGMENT {
        point: D2D_POINT_2F { x: left + r, y: top + f },
        size: D2D_SIZE_F { width: r, height: r },
        rotationAngle: 0.0,
        sweepDirection: D2D1_SWEEP_DIRECTION_CLOCKWISE,
        arcSize: D2D1_ARC_SIZE_SMALL,
    });

    sink.EndFigure(D2D1_FIGURE_END_CLOSED);
    sink.Close()?;

    rt.FillGeometry(&path, alert.unwrap_or(&eng.brush_rail), None);
    rt.DrawGeometry(&path, &eng.brush_hairline, 0.75, None);

    Ok(())
}

unsafe fn draw_usage_ring(
    eng: &D2DEngine,
    cx: f32,
    cy: f32,
    m: &Metric,
    val: f32,
    halo_val: f32,
    is_hovered: bool,
    ring_alpha: f32,
    busy_spin: f32,
    sweep: f32,
    clock: f32,
    dock: Dock,
    bu: f32,
    bv: f32,
    m3x2: &Matrix3x2,
    lm: &LayoutMetrics,
) -> Result<()> {
    let rt = &eng.rt;
    let r = lm.ring_d / 2.0;
    // 环内容恒正立：映射环心到物理坐标后切恒等变换绘制
    // （圆环旋转不变，但进度弧 12 点起点与文字不能随边镜像/旋转）。
    let (cx, cy) = canon_to_window(dock, cx, cy, bu, bv);
    rt.SetTransform(&IDENTITY);

    // 1. 悬停微发光晕 (Halo)
    if is_hovered || halo_val > 0.01 {
        let halo_r = r + 4.0 * lm.scale;
        let halo_a = (0.18 * halo_val.max(if is_hovered { 1.0 } else { 0.0 }) * ring_alpha).min(0.25);
        if halo_a > 0.005 {
            let halo_col = D2D1_COLOR_F { r: 1.0, g: 1.0, b: 1.0, a: halo_a };
            if let Ok(brush) = rt.CreateSolidColorBrush(&halo_col, None) {
                rt.DrawEllipse(
                    &D2D1_ELLIPSE { point: D2D_POINT_2F { x: cx, y: cy }, radiusX: halo_r, radiusY: halo_r },
                    &brush,
                    2.0 * lm.scale,
                    None,
                );
            }
        }
    }

    // 2. 灰色轨道
    rt.DrawEllipse(
        &D2D1_ELLIPSE { point: D2D_POINT_2F { x: cx, y: cy }, radiusX: r, radiusY: r },
        &eng.brush_track,
        lm.ring_line,
        None,
    );

    // 3. 动态进度弧 (使用 Direct2D PathGeometry 顺时针圆头绘制)
    //    刷新扫光进行中时，原弧 dim 到 0.3（Pulse refreshMark 行为）
    let sweep_deg = (val * 360.0).clamp(0.0, 359.9);
    if sweep_deg > 0.5 {
        let arc_brush = eng.color_for_fraction(val, m.balance, m.spent);
        if sweep > 0.0 {
            let dim = D2D1_COLOR_F {
                r: arc_brush.GetColor().r,
                g: arc_brush.GetColor().g,
                b: arc_brush.GetColor().b,
                a: 0.3 * ring_alpha,
            };
            if let Ok(b) = rt.CreateSolidColorBrush(&dim, None) {
                draw_arc(eng, cx, cy, r, -90.0, sweep_deg, lm.ring_line, &b)?;
            }
        } else {
            draw_arc(eng, cx, cy, r, -90.0, sweep_deg, lm.ring_line, arc_brush)?;
        }
    }

    // 3b. 刷新扫光：16% 弧沿整圈匀速走（0.85s 走完 sweep 0→1），同环色
    if sweep > 0.0 && sweep < 1.0 {
        let arc_brush = eng.color_for_fraction(val, m.balance, m.spent);
        let mark_start = -90.0 + sweep * 360.0;
        draw_arc(eng, cx, cy, r, mark_start, 57.6, lm.ring_line, arc_brush)?;
    }

    // 4. 中心深色圆盘 (Pulse: centreDiameter = 20pt)
    let disc_r = 10.0 * lm.scale;
    rt.FillEllipse(
        &D2D1_ELLIPSE { point: D2D_POINT_2F { x: cx, y: cy }, radiusX: disc_r, radiusY: disc_r },
        &eng.brush_disc,
    );

    // 5. 忙碌状态旋转圆环 (Busy Traveling Mark)
    if m.busy {
        let busy_r = 13.0 * lm.scale;
        let busy_deg = (busy_spin * 180.0 / std::f32::consts::PI) % 360.0;
        draw_arc(eng, cx, cy, busy_r, busy_deg, 60.0, 1.8 * lm.scale, &eng.brush_text_primary)?;
    }

    // 5b. 窗口时钟弧（Pulse windowClock）：外圈 hairline，半径 = 环外缘 + 4pt·scale。
    //     轨道 0.16 / 弧 0.7 中性灰白、2pt 圆头、12 点起顺时针；数据不全不画。
    if clock > -0.5 {
        let clw = 2.0 * lm.scale;
        let cr = r + lm.ring_line / 2.0 + 4.0 * lm.scale;
        let track_col = D2D1_COLOR_F { r: 1.0, g: 1.0, b: 1.0, a: 0.16 * ring_alpha };
        if let Ok(tb) = rt.CreateSolidColorBrush(&track_col, None) {
            rt.DrawEllipse(
                &D2D1_ELLIPSE { point: D2D_POINT_2F { x: cx, y: cy }, radiusX: cr, radiusY: cr },
                &tb,
                clw,
                None,
            );
        }
        if clock > 0.005 {
            let arc_col = D2D1_COLOR_F { r: 1.0, g: 1.0, b: 1.0, a: 0.7 * ring_alpha };
            if let Ok(ab) = rt.CreateSolidColorBrush(&arc_col, None) {
                let deg = (clock * 360.0).clamp(0.0, 359.9);
                draw_arc(eng, cx, cy, cr, -90.0, deg, clw, &ab)?;
            }
        }
    }

    // 6. 平台图标贴图（随栏整体旋转，与 Pulse 单一变换一致）
    if let Some(bmp) = eng.logo_bitmaps.get(&m.provider) {
        let icon_sz = disc_r * 1.6;
        let dest_rect = D2D_RECT_F {
            left: cx - icon_sz / 2.0,
            top: cy - icon_sz / 2.0,
            right: cx + icon_sz / 2.0,
            bottom: cy + icon_sz / 2.0,
        };
        rt.DrawBitmap(
            bmp,
            Some(&dest_rect),
            ring_alpha,
            D2D1_BITMAP_INTERPOLATION_MODE_LINEAR,
            None,
        );
    } else {
        // 图标兜底：首字母
        let initial: Vec<u16> = m.label.chars().take(1).collect::<String>().encode_utf16().collect();
        let rect = D2D_RECT_F {
            left: cx - disc_r,
            top: cy - disc_r * 0.7,
            right: cx + disc_r,
            bottom: cy + disc_r,
        };
        rt.DrawText(
            &initial,
            &eng.font_title,
            &rect,
            &eng.brush_text_primary,
            D2D1_DRAW_TEXT_OPTIONS_NONE,
            DWRITE_MEASURING_MODE_NATURAL,
        );
    }

    // 7. 百分比/额度文字 (DirectWrite 锐利排版)
    let label_text = if m.empty {
        "—".to_string()
    } else if m.balance {
        m.text.clone()
    } else {
        format!("{:.0}%", val * 100.0)
    };
    let u16_label: Vec<u16> = label_text.encode_utf16().collect();
    let text_w = lm.pill_w - 6.0 * lm.scale;
    let text_h = 16.0 * lm.scale;
    let tcx = cx;
    let tcy = cy + r + 6.0 * lm.scale + text_h / 2.0;
    let text_rect = D2D_RECT_F {
        left: tcx - text_w / 2.0,
        top: tcy - text_h / 2.0,
        right: tcx + text_w / 2.0,
        bottom: tcy + text_h / 2.0,
    };
    let text_brush = if m.spent || (m.used >= 0.75 && !m.balance && !m.empty) {
        if m.spent { &eng.brush_exhausted } else { &eng.brush_warning }
    } else if m.empty {
        &eng.brush_text_dim
    } else {
        &eng.brush_text_primary
    };

    rt.DrawText(
        &u16_label,
        &eng.font_number,
        &text_rect,
        text_brush,
        D2D1_DRAW_TEXT_OPTIONS_NONE,
        DWRITE_MEASURING_MODE_NATURAL,
    );
    rt.SetTransform(m3x2);

    Ok(())
}

unsafe fn draw_arc(
    eng: &D2DEngine,
    cx: f32,
    cy: f32,
    r: f32,
    start_deg: f32,
    sweep_deg: f32,
    width: f32,
    brush: &ID2D1SolidColorBrush,
) -> Result<()> {
    let rt = &eng.rt;
    let path = eng.d2d_factory.CreatePathGeometry()?;
    let sink = path.Open()?;

    let start_rad = start_deg.to_radians();
    let end_rad = (start_deg + sweep_deg).to_radians();

    let p0 = D2D_POINT_2F {
        x: cx + r * start_rad.cos(),
        y: cy + r * start_rad.sin(),
    };
    let p1 = D2D_POINT_2F {
        x: cx + r * end_rad.cos(),
        y: cy + r * end_rad.sin(),
    };

    sink.BeginFigure(p0, D2D1_FIGURE_BEGIN_HOLLOW);
    sink.AddArc(&D2D1_ARC_SEGMENT {
        point: p1,
        size: D2D_SIZE_F { width: r, height: r },
        rotationAngle: 0.0,
        sweepDirection: D2D1_SWEEP_DIRECTION_CLOCKWISE,
        arcSize: if sweep_deg > 180.0 { D2D1_ARC_SIZE_LARGE } else { D2D1_ARC_SIZE_SMALL },
    });
    sink.EndFigure(D2D1_FIGURE_END_OPEN);
    sink.Close()?;

    rt.DrawGeometry(&path, brush, width, &eng.stroke_round);
    Ok(())
}

unsafe fn draw_detail_card(
    eng: &D2DEngine,
    m: &Metric,
    ring_cx: f32,
    ring_cy: f32,
    win_w: f32,
    win_h: f32,
    side: u8, // 0=Right 1=Left 2=Top 3=Bottom
    card_openness: f32,
    segmented: bool,
    lm: &LayoutMetrics,
) -> Result<()> {
    let rt = &eng.rt;
    let scale = lm.scale;

    let rows_count = m.rows.len().clamp(1, 4);
    let header_h = 20.0 * scale;
    let row_h = 56.0 * scale;
    let burn = burn_forecast(m);
    let burn_extra = if burn.is_some() { 16.0 * scale } else { 0.0 };
    let card_h = lm.card_pad * 2.0 + header_h + 14.0 * scale + (rows_count as f32) * row_h - 14.0 * scale + burn_extra;
    let card_w = lm.card_w;
    let gap = 8.0 * scale;
    let reach = lm.pointer_w;
    let half_tail = lm.pointer_h * 0.5;
    let r_card = lm.card_radius;
    let slide = (1.0 - card_openness) * 16.0 * scale;

    // 卡片矩形（物理空间恒正立）+ 尾巴方向
    // tail_dir: 0=+x(右) 1=-x(左) 2=+y(下) 3=-y(上)
    let (mut card_l, mut card_t, tail_dir) = match side {
        0 => (ring_cx - lm.pill_w / 2.0 - gap - card_w - slide, 0.0, 1),
        1 => (ring_cx + lm.pill_w / 2.0 + gap + slide, 0.0, 0),
        2 => (0.0, ring_cy + lm.pill_w / 2.0 + gap + slide, 3),
        _ => (0.0, ring_cy - lm.pill_w / 2.0 - gap - card_h - slide, 2),
    };
    // 沿边轴居中 + clamp
    match side {
        0 | 1 => {
            card_t = (ring_cy - card_h / 2.0).clamp(12.0 * scale, (win_h - card_h - 12.0 * scale).max(12.0 * scale));
        }
        _ => {
            card_l = (ring_cx - card_w / 2.0).clamp(12.0 * scale, (win_w - card_w - 12.0 * scale).max(12.0 * scale));
        }
    }
    let card_r = card_l + card_w;
    let card_b = card_t + card_h;
    let infl = reach + 2.0;
    CARD_RECT.with(|c| c.set((card_l - infl, card_t - infl, card_r + infl, card_b + infl)));

    // 尾巴锚点（沿卡片边），clamp 到圆角之间
    let (ax, ay) = match side {
        0 | 1 => (
            ring_cx,
            ring_cy.clamp(card_t + r_card + 20.0 * scale, (card_b - r_card - 20.0 * scale).max(card_t + r_card + 20.0 * scale)),
        ),
        _ => (
            ring_cx.clamp(card_l + r_card + 20.0 * scale, (card_r - r_card - 20.0 * scale).max(card_l + r_card + 20.0 * scale)),
            ring_cy,
        ),
    };

    // 1. 卡片主体（圆角矩形）
    rt.FillRoundedRectangle(
        &D2D1_ROUNDED_RECT {
            rect: D2D_RECT_F { left: card_l, top: card_t, right: card_r, bottom: card_b },
            radiusX: r_card,
            radiusY: r_card,
        },
        &eng.brush_card,
    );

    // 2. 水滴尾巴（同色填充，四方向）
    let tail = eng.d2d_factory.CreatePathGeometry()?;
    let s = tail.Open()?;
    match tail_dir {
        0 => {
            // +x 右
            s.BeginFigure(D2D_POINT_2F { x: card_r, y: ay - half_tail }, D2D1_FIGURE_BEGIN_FILLED);
            s.AddBezier(&D2D1_BEZIER_SEGMENT {
                point1: D2D_POINT_2F { x: card_r, y: ay - half_tail * 0.5 },
                point2: D2D_POINT_2F { x: card_r + reach * 0.55, y: ay - half_tail * 0.22 },
                point3: D2D_POINT_2F { x: card_r + reach, y: ay },
            });
            s.AddBezier(&D2D1_BEZIER_SEGMENT {
                point1: D2D_POINT_2F { x: card_r + reach * 0.55, y: ay + half_tail * 0.22 },
                point2: D2D_POINT_2F { x: card_r, y: ay + half_tail * 0.5 },
                point3: D2D_POINT_2F { x: card_r, y: ay + half_tail },
            });
        }
        1 => {
            // -x 左
            s.BeginFigure(D2D_POINT_2F { x: card_l, y: ay - half_tail }, D2D1_FIGURE_BEGIN_FILLED);
            s.AddBezier(&D2D1_BEZIER_SEGMENT {
                point1: D2D_POINT_2F { x: card_l, y: ay - half_tail * 0.5 },
                point2: D2D_POINT_2F { x: card_l - reach * 0.55, y: ay - half_tail * 0.22 },
                point3: D2D_POINT_2F { x: card_l - reach, y: ay },
            });
            s.AddBezier(&D2D1_BEZIER_SEGMENT {
                point1: D2D_POINT_2F { x: card_l - reach * 0.55, y: ay + half_tail * 0.22 },
                point2: D2D_POINT_2F { x: card_l, y: ay + half_tail * 0.5 },
                point3: D2D_POINT_2F { x: card_l, y: ay + half_tail },
            });
        }
        2 => {
            // +y 下
            s.BeginFigure(D2D_POINT_2F { x: ax - half_tail, y: card_b }, D2D1_FIGURE_BEGIN_FILLED);
            s.AddBezier(&D2D1_BEZIER_SEGMENT {
                point1: D2D_POINT_2F { x: ax - half_tail * 0.5, y: card_b },
                point2: D2D_POINT_2F { x: ax - half_tail * 0.22, y: card_b + reach * 0.55 },
                point3: D2D_POINT_2F { x: ax, y: card_b + reach },
            });
            s.AddBezier(&D2D1_BEZIER_SEGMENT {
                point1: D2D_POINT_2F { x: ax + half_tail * 0.22, y: card_b + reach * 0.55 },
                point2: D2D_POINT_2F { x: ax + half_tail * 0.5, y: card_b },
                point3: D2D_POINT_2F { x: ax + half_tail, y: card_b },
            });
        }
        _ => {
            // -y 上
            s.BeginFigure(D2D_POINT_2F { x: ax - half_tail, y: card_t }, D2D1_FIGURE_BEGIN_FILLED);
            s.AddBezier(&D2D1_BEZIER_SEGMENT {
                point1: D2D_POINT_2F { x: ax - half_tail * 0.5, y: card_t },
                point2: D2D_POINT_2F { x: ax - half_tail * 0.22, y: card_t - reach * 0.55 },
                point3: D2D_POINT_2F { x: ax, y: card_t - reach },
            });
            s.AddBezier(&D2D1_BEZIER_SEGMENT {
                point1: D2D_POINT_2F { x: ax + half_tail * 0.22, y: card_t - reach * 0.55 },
                point2: D2D_POINT_2F { x: ax + half_tail * 0.5, y: card_t },
                point3: D2D_POINT_2F { x: ax + half_tail, y: card_t },
            });
        }
    }
    s.EndFigure(D2D1_FIGURE_END_CLOSED);
    s.Close()?;
    rt.FillGeometry(&tail, &eng.brush_card, None);
    rt.DrawRoundedRectangle(
        &D2D1_ROUNDED_RECT {
            rect: D2D_RECT_F { left: card_l, top: card_t, right: card_r, bottom: card_b },
            radiusX: r_card,
            radiusY: r_card,
        },
        &eng.brush_hairline,
        0.75,
        None,
    );

    // 3. 头部：图标 + 标题
    let mut cur_y = card_t + lm.card_pad;
    let icon_sz = 16.0 * scale;
    if let Some(bmp) = eng.logo_bitmaps.get(&m.provider) {
        let dest = D2D_RECT_F {
            left: card_l + lm.card_pad,
            top: cur_y + 1.0 * scale,
            right: card_l + lm.card_pad + icon_sz,
            bottom: cur_y + 1.0 * scale + icon_sz,
        };
        rt.DrawBitmap(bmp, Some(&dest), 1.0, D2D1_BITMAP_INTERPOLATION_MODE_LINEAR, None);
    }
    let title_u16: Vec<u16> = m.full.encode_utf16().collect();
    let title_rect = D2D_RECT_F {
        left: card_l + lm.card_pad + icon_sz + 8.0 * scale,
        top: cur_y - 2.0 * scale,
        right: card_r - lm.card_pad,
        bottom: cur_y + 18.0 * scale,
    };
    rt.DrawText(&title_u16, &eng.font_title, &title_rect, &eng.brush_text_primary, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);
    cur_y += header_h + 14.0 * scale;

    // 4. 用量行
    let row_w = card_w - lm.card_pad * 2.0;
    for (row_i, row) in m.rows.iter().enumerate().take(4) {
        let label_u16: Vec<u16> = row.label.encode_utf16().collect();
        let r1 = D2D_RECT_F {
            left: card_l + lm.card_pad,
            top: cur_y,
            right: card_l + lm.card_pad + row_w * 0.55,
            bottom: cur_y + 16.0 * scale,
        };
        rt.DrawText(&label_u16, &eng.font_label, &r1, &eng.brush_text_primary, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);
        if !row.detail.is_empty() {
            let detail_u16: Vec<u16> = row.detail.encode_utf16().collect();
            let r_amount = D2D_RECT_F {
                left: card_l + lm.card_pad + row_w * 0.45,
                top: cur_y,
                right: card_r - lm.card_pad,
                bottom: cur_y + 16.0 * scale,
            };
            rt.DrawText(&detail_u16, &eng.font_detail_right, &r_amount, &eng.brush_text_dim, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);
        }
        let frac = (row.used_pct as f32 / 100.0).clamp(0.0, 1.0);
        if segmented {
            cur_y += 14.0 * scale + 4.0 * scale;
            let bar_h = 12.0 * scale;
            let tick_w = 2.5 * scale;
            let step = tick_w + 2.0 * scale;
            let count = (((row_w + 2.0 * scale) / step).floor() as i32).max(1);
            let mut lit = (frac * count as f32).round() as i32;
            if frac > 0.0 {
                lit = lit.max(1);
            }
            if frac < 1.0 {
                lit = lit.min(count - 1);
            }
            let bar_brush = eng.color_for_fraction(frac, false, frac >= 0.999);
            for t in 0..count {
                let x0 = card_l + lm.card_pad + t as f32 * step;
                let rect = D2D_RECT_F { left: x0, top: cur_y, right: x0 + tick_w, bottom: cur_y + bar_h };
                let b = if t < lit { bar_brush } else { &eng.brush_track };
                rt.FillRoundedRectangle(&D2D1_ROUNDED_RECT { rect, radiusX: tick_w / 2.0, radiusY: tick_w / 2.0 }, b);
            }
            cur_y += bar_h + 4.0 * scale;
        } else {
            cur_y += 14.0 * scale + 7.0 * scale;
            let bar_h = 6.0 * scale;
            let bar_track = D2D_RECT_F {
                left: card_l + lm.card_pad,
                top: cur_y,
                right: card_r - lm.card_pad,
                bottom: cur_y + bar_h,
            };
            rt.FillRoundedRectangle(&D2D1_ROUNDED_RECT { rect: bar_track, radiusX: bar_h / 2.0, radiusY: bar_h / 2.0 }, &eng.brush_track);
            if frac > 0.005 {
                let fill_w = (row_w * frac).max(bar_h);
                let bar_fill = D2D_RECT_F {
                    left: card_l + lm.card_pad,
                    top: cur_y,
                    right: card_l + lm.card_pad + fill_w,
                    bottom: cur_y + bar_h,
                };
                let bar_brush = eng.color_for_fraction(frac, false, frac >= 0.999);
                rt.FillRoundedRectangle(&D2D1_ROUNDED_RECT { rect: bar_fill, radiusX: bar_h / 2.0, radiusY: bar_h / 2.0 }, bar_brush);
            }
            cur_y += bar_h + 7.0 * scale;
        }
        let pct_str = format!("{:.0}% Used", row.used_pct);
        let pct_u16: Vec<u16> = pct_str.encode_utf16().collect();
        let r_pct = D2D_RECT_F {
            left: card_l + lm.card_pad,
            top: cur_y,
            right: card_l + lm.card_pad + row_w * 0.5,
            bottom: cur_y + 16.0 * scale,
        };
        let pct_brush = if frac >= 0.75 { &eng.brush_warning } else { &eng.brush_text_primary };
        rt.DrawText(&pct_u16, &eng.font_medium, &r_pct, pct_brush, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);
        if !row.reset.is_empty() {
            let reset_u16: Vec<u16> = row.reset.encode_utf16().collect();
            let r_reset = D2D_RECT_F {
                left: card_l + lm.card_pad + row_w * 0.4,
                top: cur_y,
                right: card_r - lm.card_pad,
                bottom: cur_y + 16.0 * scale,
            };
            rt.DrawText(&reset_u16, &eng.font_detail_right, &r_reset, &eng.brush_text_dim, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);
        }
        cur_y += 16.0 * scale;
        if row_i == 0 {
            if let Some((text, warn)) = &burn {
                let bu: Vec<u16> = text.encode_utf16().collect();
                let r_burn = D2D_RECT_F {
                    left: card_l + lm.card_pad,
                    top: cur_y,
                    right: card_r - lm.card_pad,
                    bottom: cur_y + 14.0 * scale,
                };
                let b_brush = if *warn { &eng.brush_warning } else { &eng.brush_text_dim };
                rt.DrawText(&bu, &eng.font_detail, &r_burn, b_brush, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);
                cur_y += 14.0 * scale;
            }
        }
        cur_y += 14.0 * scale;
    }

    Ok(())
}

/// Pulse BurnRate：由 used + 窗口 elapsed 推算耗尽时刻。数据不全绝不猜 → None。
fn burn_forecast(m: &Metric) -> Option<(String, bool)> {
    if m.window_seconds <= 0 || m.resets_at_unix <= 0 || m.spent || m.balance {
        return None;
    }
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .ok()?
        .as_secs() as i64;
    let until_reset = m.resets_at_unix - now;
    if until_reset <= 0 {
        return None;
    }
    let elapsed = (1.0 - until_reset as f64 / m.window_seconds as f64).clamp(0.0, 1.0);
    if elapsed < 0.03 {
        return None; // 开窗期突发会放大成天文速率，Pulse 同规则
    }
    let used = m.used;
    if used >= 1.0 {
        return Some(("预计已耗尽".to_string(), true));
    }
    let elapsed_seconds = until_reset as f64 / (1.0 - elapsed) * elapsed;
    let per_hour = used / elapsed_seconds * 3600.0;
    if per_hour <= 0.0 {
        return None;
    }
    let until_empty = (1.0 - used) / per_hour * 3600.0;
    let exhausts = until_empty < until_reset as f64;
    if exhausts && until_empty <= 7200.0 {
        Some((format!("预计 {} 内耗尽", approx_duration(until_empty)), true))
    } else if exhausts {
        Some(("撑不到下次重置".to_string(), true))
    } else {
        Some(("预计够用至重置".to_string(), false))
    }
}

/// Pulse approximate()：15 分钟 / 半小时一档的口语化时长。
fn approx_duration(secs: f64) -> String {
    let minutes = (secs / 60.0).round();
    if minutes < 15.0 {
        "不到 15 分钟".to_string()
    } else if minutes < 68.0 {
        let quarters = ((minutes / 15.0).round() as i32).max(1);
        if quarters == 4 {
            "约 1 小时".to_string()
        } else {
            format!("约 {} 分钟", quarters * 15)
        }
    } else {
        let hours = (minutes / 30.0).round() / 2.0;
        if (hours - hours.floor()).abs() < 0.01 {
            format!("约 {} 小时", hours as i32)
        } else {
            format!("约 {:.1} 小时", hours)
        }
    }
}
