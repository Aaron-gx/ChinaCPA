use windows::{
    core::*,
    Win32::Graphics::Direct2D::*,
    Win32::Graphics::Direct2D::Common::*,
    Win32::Graphics::DirectWrite::*,
    Win32::Foundation::HWND,
};

use crate::graphics::D2DEngine;
use crate::metrics::Metric;
use crate::spring::{FadeValue, SpringValue};

pub struct RingAnimState {
    pub arc: SpringValue,
    pub halo: FadeValue,
    /// 刷新扫光进度：0=无，>0 时沿环走一圈（16% 弧，0.85s）。点环刷新后触发。
    pub sweep: f32,
}

impl RingAnimState {
    pub fn new(val: f32) -> Self {
        Self {
            arc: SpringValue::new(val),
            halo: FadeValue::new(0.0, 5.0),
            sweep: 0.0,
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
    hwnd: HWND,
) -> Result<()> {
    unsafe {
        let rt = &eng.rt;
        rt.BeginDraw();
        rt.Clear(Some(&D2D1_COLOR_F { r: 0.0, g: 0.0, b: 0.0, a: 0.0 }));

        let lm = LayoutMetrics::new(scale);
        let n = metrics.len().min(8);
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

        let dock_top = (h - cur_h) / 2.0;
        let dock_bottom = dock_top + cur_h;
        let dock_left = w - cur_w;
        let dock_right = w;

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
                    &lm,
                )?;
            }
        }

        // 5. 展开 Pulse 经典水滴气泡详情卡片 (UsageBubbleShape)
        if card_openness > 0.01 && hover_index.is_some() {
            let idx = hover_index.unwrap().min(n - 1);
            let m = &metrics[idx];
            let target_anchor_y = slot_centers[idx];
            let active_pointer_y = if pointer_y > 0.0 { pointer_y } else { target_anchor_y };

            draw_detail_card(
                eng,
                m,
                dock_left,
                active_pointer_y,
                card_openness,
                h,
                &lm,
            )?;
        }

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
    lm: &LayoutMetrics,
) -> Result<()> {
    let rt = &eng.rt;
    let r = lm.ring_d / 2.0;

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

    // 6. 平台图标贴图
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
    let text_top = cy + r + 6.0 * lm.scale;
    let text_rect = D2D_RECT_F {
        left: cx - text_w / 2.0,
        top: text_top,
        right: cx + text_w / 2.0,
        bottom: text_top + 16.0 * lm.scale,
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
    dock_left: f32,
    pointer_y: f32,
    card_openness: f32,
    screen_h: f32,
    lm: &LayoutMetrics,
) -> Result<()> {
    let rt = &eng.rt;
    let scale = lm.scale;

    let rows_count = m.rows.len().clamp(1, 4);
    let header_h = 20.0 * scale;
    let row_h = 56.0 * scale;
    let card_h = lm.card_pad * 2.0 + header_h + 14.0 * scale + (rows_count as f32) * row_h - 14.0 * scale;

    let slide_offset = (1.0 - card_openness) * 16.0 * scale;
    let card_right = dock_left - lm.pointer_w - 8.0 * scale + slide_offset;
    let card_left = card_right - lm.card_w;

    let mut card_top = pointer_y - card_h / 2.0;
    if card_top < 12.0 * scale {
        card_top = 12.0 * scale;
    }
    if card_top + card_h > screen_h - 12.0 * scale {
        card_top = screen_h - 12.0 * scale - card_h;
    }
    let card_bottom = card_top + card_h;

    let r_card = lm.card_radius;
    let py = pointer_y.clamp(card_top + r_card + 20.0 * scale, card_bottom - r_card - 20.0 * scale);
    let half_tail = lm.pointer_h * 0.5;
    let reach = lm.pointer_w;

    // 1. 构建一体化水滴气泡外形（UsageBubbleShape，无缝融合指针三角形与卡片主体）
    let bubble_path = eng.d2d_factory.CreatePathGeometry()?;
    let sink = bubble_path.Open()?;

    sink.BeginFigure(D2D_POINT_2F { x: card_left + r_card, y: card_top }, D2D1_FIGURE_BEGIN_FILLED);
    sink.AddLine(D2D_POINT_2F { x: card_right - r_card, y: card_top });
    sink.AddArc(&D2D1_ARC_SEGMENT {
        point: D2D_POINT_2F { x: card_right, y: card_top + r_card },
        size: D2D_SIZE_F { width: r_card, height: r_card },
        rotationAngle: 0.0,
        sweepDirection: D2D1_SWEEP_DIRECTION_CLOCKWISE,
        arcSize: D2D1_ARC_SIZE_SMALL,
    });

    sink.AddLine(D2D_POINT_2F { x: card_right, y: py - half_tail });

    // 水滴指针上侧流体曲线 (Pulse fluid turn 参数)
    sink.AddBezier(&D2D1_BEZIER_SEGMENT {
        point1: D2D_POINT_2F { x: card_right, y: py - half_tail * 0.5 },
        point2: D2D_POINT_2F { x: card_right + reach * 0.55, y: py - half_tail * 0.22 },
        point3: D2D_POINT_2F { x: card_right + reach, y: py },
    });

    // 水滴指针下侧流体曲线
    sink.AddBezier(&D2D1_BEZIER_SEGMENT {
        point1: D2D_POINT_2F { x: card_right + reach * 0.55, y: py + half_tail * 0.22 },
        point2: D2D_POINT_2F { x: card_right, y: py + half_tail * 0.5 },
        point3: D2D_POINT_2F { x: card_right, y: py + half_tail },
    });

    sink.AddLine(D2D_POINT_2F { x: card_right, y: card_bottom - r_card });
    sink.AddArc(&D2D1_ARC_SEGMENT {
        point: D2D_POINT_2F { x: card_right - r_card, y: card_bottom },
        size: D2D_SIZE_F { width: r_card, height: r_card },
        rotationAngle: 0.0,
        sweepDirection: D2D1_SWEEP_DIRECTION_CLOCKWISE,
        arcSize: D2D1_ARC_SIZE_SMALL,
    });

    sink.AddLine(D2D_POINT_2F { x: card_left + r_card, y: card_bottom });
    sink.AddArc(&D2D1_ARC_SEGMENT {
        point: D2D_POINT_2F { x: card_left, y: card_bottom - r_card },
        size: D2D_SIZE_F { width: r_card, height: r_card },
        rotationAngle: 0.0,
        sweepDirection: D2D1_SWEEP_DIRECTION_CLOCKWISE,
        arcSize: D2D1_ARC_SIZE_SMALL,
    });

    sink.AddLine(D2D_POINT_2F { x: card_left, y: card_top + r_card });
    sink.AddArc(&D2D1_ARC_SEGMENT {
        point: D2D_POINT_2F { x: card_left + r_card, y: card_top },
        size: D2D_SIZE_F { width: r_card, height: r_card },
        rotationAngle: 0.0,
        sweepDirection: D2D1_SWEEP_DIRECTION_CLOCKWISE,
        arcSize: D2D1_ARC_SIZE_SMALL,
    });

    sink.EndFigure(D2D1_FIGURE_END_CLOSED);
    sink.Close()?;

    rt.FillGeometry(&bubble_path, &eng.brush_card, None);
    rt.DrawGeometry(&bubble_path, &eng.brush_hairline, 0.75, None);

    // 2. 卡片头部：图标 + 标题
    let mut cur_y = card_top + lm.card_pad;
    let icon_sz = 16.0 * scale;
    if let Some(bmp) = eng.logo_bitmaps.get(&m.provider) {
        let dest = D2D_RECT_F {
            left: card_left + lm.card_pad,
            top: cur_y + 1.0 * scale,
            right: card_left + lm.card_pad + icon_sz,
            bottom: cur_y + 1.0 * scale + icon_sz,
        };
        rt.DrawBitmap(bmp, Some(&dest), 1.0, D2D1_BITMAP_INTERPOLATION_MODE_LINEAR, None);
    }

    let title_u16: Vec<u16> = m.full.encode_utf16().collect();
    let title_rect = D2D_RECT_F {
        left: card_left + lm.card_pad + icon_sz + 8.0 * scale,
        top: cur_y - 2.0 * scale,
        right: card_right - lm.card_pad,
        bottom: cur_y + 18.0 * scale,
    };
    rt.DrawText(&title_u16, &eng.font_title, &title_rect, &eng.brush_text_primary, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);

    cur_y += header_h + 14.0 * scale;

    // 3. 渲染各用量行（Pulse 经典三行布局：名称+额度 / 进度条胶囊 / 百分比+重置时间）
    let row_w = lm.card_w - lm.card_pad * 2.0;
    for row in m.rows.iter().take(4) {
        // 第一行：左侧名称，右侧额度明细
        let label_u16: Vec<u16> = row.label.encode_utf16().collect();
        let r1 = D2D_RECT_F {
            left: card_left + lm.card_pad,
            top: cur_y,
            right: card_left + lm.card_pad + row_w * 0.55,
            bottom: cur_y + 16.0 * scale,
        };
        rt.DrawText(&label_u16, &eng.font_label, &r1, &eng.brush_text_primary, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);

        if !row.detail.is_empty() {
            let detail_u16: Vec<u16> = row.detail.encode_utf16().collect();
            let r_amount = D2D_RECT_F {
                left: card_left + lm.card_pad + row_w * 0.45,
                top: cur_y,
                right: card_right - lm.card_pad,
                bottom: cur_y + 16.0 * scale,
            };
            rt.DrawText(&detail_u16, &eng.font_detail_right, &r_amount, &eng.brush_text_dim, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);
        }

        // 第二行：Pulse 胶囊进度条 (高度 6pt)
        cur_y += 14.0 * scale + 7.0 * scale;
        let bar_h = 6.0 * scale;
        let bar_track = D2D_RECT_F {
            left: card_left + lm.card_pad,
            top: cur_y,
            right: card_right - lm.card_pad,
            bottom: cur_y + bar_h,
        };
        rt.FillRoundedRectangle(&D2D1_ROUNDED_RECT { rect: bar_track, radiusX: bar_h / 2.0, radiusY: bar_h / 2.0 }, &eng.brush_track);

        let frac = (row.used_pct as f32 / 100.0).clamp(0.0, 1.0);
        if frac > 0.005 {
            let fill_w = (row_w * frac).max(bar_h);
            let bar_fill = D2D_RECT_F {
                left: card_left + lm.card_pad,
                top: cur_y,
                right: card_left + lm.card_pad + fill_w,
                bottom: cur_y + bar_h,
            };
            let bar_brush = eng.color_for_fraction(frac, false, frac >= 0.999);
            rt.FillRoundedRectangle(&D2D1_ROUNDED_RECT { rect: bar_fill, radiusX: bar_h / 2.0, radiusY: bar_h / 2.0 }, bar_brush);
        }

        // 第三行：左侧已用百分比，右侧重置倒计时
        cur_y += bar_h + 7.0 * scale;
        let pct_str = format!("{:.0}% Used", row.used_pct);
        let pct_u16: Vec<u16> = pct_str.encode_utf16().collect();
        let r_pct = D2D_RECT_F {
            left: card_left + lm.card_pad,
            top: cur_y,
            right: card_left + lm.card_pad + row_w * 0.5,
            bottom: cur_y + 16.0 * scale,
        };
        let pct_brush = if frac >= 0.75 { &eng.brush_warning } else { &eng.brush_text_primary };
        rt.DrawText(&pct_u16, &eng.font_medium, &r_pct, pct_brush, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);

        if !row.reset.is_empty() {
            let reset_u16: Vec<u16> = row.reset.encode_utf16().collect();
            let r_reset = D2D_RECT_F {
                left: card_left + lm.card_pad + row_w * 0.4,
                top: cur_y,
                right: card_right - lm.card_pad,
                bottom: cur_y + 16.0 * scale,
            };
            rt.DrawText(&reset_u16, &eng.font_detail_right, &r_reset, &eng.brush_text_dim, D2D1_DRAW_TEXT_OPTIONS_NONE, DWRITE_MEASURING_MODE_NATURAL);
        }

        cur_y += 14.0 * scale + 14.0 * scale;
    }

    Ok(())
}
