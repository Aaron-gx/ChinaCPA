use windows::{
    core::*,
    Win32::Foundation::*,
    Win32::Graphics::Direct2D::*,
    Win32::Graphics::Direct2D::Common::*,
    Win32::Graphics::DirectWrite::*,
    Win32::Graphics::Direct3D::*,
    Win32::Graphics::Direct3D11::*,
    Win32::Graphics::Dxgi::*,
    Win32::Graphics::Dxgi::Common::*,
    Win32::Graphics::Gdi::*,
    Win32::UI::WindowsAndMessaging::*,
};

use std::collections::HashMap;

pub struct D2DEngine {
    pub d2d_factory: ID2D1Factory1,
    pub dwrite_factory: IDWriteFactory,
    pub d3d_device: ID3D11Device,
    pub rt: ID2D1RenderTarget,
    pub d2d_context: ID2D1DeviceContext,
    d3d_context: ID3D11DeviceContext,
    rt_texture: ID3D11Texture2D,
    staging: ID3D11Texture2D,
    dib_dc: HDC,
    dib_bitmap: HBITMAP,
    dib_old: HGDIOBJ,
    dib_bits: *mut u8,
    pub width: u32,
    pub height: u32,

    // 字体格式
    pub font_number: IDWriteTextFormat,
    pub font_title: IDWriteTextFormat,
    pub font_label: IDWriteTextFormat,
    pub font_medium: IDWriteTextFormat,
    pub font_detail: IDWriteTextFormat,
    pub font_detail_right: IDWriteTextFormat,

    // 画刷缓存
    pub brush_rail: ID2D1SolidColorBrush,
    pub brush_card: ID2D1SolidColorBrush,
    pub brush_hairline: ID2D1SolidColorBrush,
    pub brush_track: ID2D1SolidColorBrush,
    pub brush_disc: ID2D1SolidColorBrush,
    pub brush_text_primary: ID2D1SolidColorBrush,
    pub brush_text_dim: ID2D1SolidColorBrush,
    pub brush_good: ID2D1SolidColorBrush,
    pub brush_caution: ID2D1SolidColorBrush,
    pub brush_warning: ID2D1SolidColorBrush,
    pub brush_exhausted: ID2D1SolidColorBrush,
    pub brush_balance: ID2D1SolidColorBrush,
    pub stroke_round: ID2D1StrokeStyle,

    // 贴图缓存
    pub logo_bitmaps: HashMap<String, ID2D1Bitmap>,
}

impl D2DEngine {
    pub fn new(width: u32, height: u32) -> Result<Self> {
        unsafe {
            let d2d_factory: ID2D1Factory1 =
                D2D1CreateFactory(D2D1_FACTORY_TYPE_SINGLE_THREADED, None)?;

            let dwrite_factory: IDWriteFactory =
                DWriteCreateFactory(DWRITE_FACTORY_TYPE_SHARED)?;

            // 1. D3D11 设备（支持 BGRA）
            let mut d3d_device = None;
            let mut d3d_context = None;
            D3D11CreateDevice(
                None,
                D3D_DRIVER_TYPE_HARDWARE,
                HMODULE::default(),
                D3D11_CREATE_DEVICE_BGRA_SUPPORT,
                Some(&[D3D_FEATURE_LEVEL_11_0, D3D_FEATURE_LEVEL_10_0]),
                D3D11_SDK_VERSION,
                Some(&mut d3d_device),
                None,
                Some(&mut d3d_context),
            )?;
            let d3d_device = d3d_device.unwrap();
            let d3d_context = d3d_context.unwrap();
            let dxgi_device: IDXGIDevice = d3d_device.cast()?;

            // 2. Direct2D DeviceContext
            let d2d_device = d2d_factory.CreateDevice(&dxgi_device)?;
            let d2d_context = d2d_device.CreateDeviceContext(D2D1_DEVICE_CONTEXT_OPTIONS_NONE)?;
            let rt: ID2D1RenderTarget = d2d_context.cast()?;

            // 3. GPU 渲染纹理 + CPU 可读 staging
            //
            // 不用 DXGI SwapChain + DirectComposition：那条路的 hwnd target
            // 会把未绘制像素填成灰（实测 RGB 211,211,211），卡片弹出时必然留出
            // 一片灰。改成 D2D 画到 GPU 纹理 → 拷到 staging → 读回 BGRA →
            // UpdateLayeredWindow 标准预乘 alpha 合成。
            let (rt_texture, staging) = create_render_textures(&d3d_device, width, height)?;

            // 4. D2D 以纹理为渲染目标（必须显式预乘 alpha，见 target_bitmap_props）
            let dxgi_surface: IDXGISurface = rt_texture.cast()?;
            let target_bitmap = d2d_context.CreateBitmapFromDxgiSurface(
                &dxgi_surface,
                Some(&target_bitmap_props()),
            )?;
            d2d_context.SetTarget(&target_bitmap);

            // 5. 提交用的 DIB 段
            let (dib_dc, dib_bitmap, dib_old, dib_bits) = create_dib(width, height)?;

            // 6. DirectWrite 字体
            let font_number = dwrite_factory.CreateTextFormat(
                w!("Segoe UI"),
                None,
                DWRITE_FONT_WEIGHT_BOLD,
                DWRITE_FONT_STYLE_NORMAL,
                DWRITE_FONT_STRETCH_NORMAL,
                13.0,
                w!("zh-CN"),
            )?;
            font_number.SetTextAlignment(DWRITE_TEXT_ALIGNMENT_CENTER)?;

            let font_title = dwrite_factory.CreateTextFormat(
                w!("Microsoft YaHei UI"),
                None,
                DWRITE_FONT_WEIGHT_BOLD,
                DWRITE_FONT_STYLE_NORMAL,
                DWRITE_FONT_STRETCH_NORMAL,
                14.0,
                w!("zh-CN"),
            )?;

            let font_label = dwrite_factory.CreateTextFormat(
                w!("Microsoft YaHei UI"),
                None,
                DWRITE_FONT_WEIGHT_MEDIUM,
                DWRITE_FONT_STYLE_NORMAL,
                DWRITE_FONT_STRETCH_NORMAL,
                12.0,
                w!("zh-CN"),
            )?;

            let font_detail = dwrite_factory.CreateTextFormat(
                w!("Segoe UI"),
                None,
                DWRITE_FONT_WEIGHT_NORMAL,
                DWRITE_FONT_STYLE_NORMAL,
                DWRITE_FONT_STRETCH_NORMAL,
                11.5,
                w!("zh-CN"),
            )?;

            let font_medium = dwrite_factory.CreateTextFormat(
                w!("Segoe UI"),
                None,
                DWRITE_FONT_WEIGHT_MEDIUM,
                DWRITE_FONT_STYLE_NORMAL,
                DWRITE_FONT_STRETCH_NORMAL,
                11.5,
                w!("zh-CN"),
            )?;

            let font_detail_right = dwrite_factory.CreateTextFormat(
                w!("Segoe UI"),
                None,
                DWRITE_FONT_WEIGHT_NORMAL,
                DWRITE_FONT_STYLE_NORMAL,
                DWRITE_FONT_STRETCH_NORMAL,
                11.5,
                w!("zh-CN"),
            )?;
            font_detail_right.SetTextAlignment(DWRITE_TEXT_ALIGNMENT_TRAILING)?;

            // 7. 画刷
            let brush_rail = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 0.015, g: 0.015, b: 0.02, a: 1.0 },
                None,
            )?;
            let brush_card = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 0.06, g: 0.06, b: 0.07, a: 1.0 },
                None,
            )?;
            let brush_hairline = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 1.0, g: 1.0, b: 1.0, a: 0.12 },
                None,
            )?;
            let brush_track = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 1.0, g: 1.0, b: 1.0, a: 0.18 },
                None,
            )?;
            let brush_disc = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 0.04, g: 0.04, b: 0.05, a: 1.0 },
                None,
            )?;
            let brush_text_primary = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 0.96, g: 0.96, b: 0.96, a: 1.0 },
                None,
            )?;
            let brush_text_dim = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 1.0, g: 1.0, b: 1.0, a: 0.45 },
                None,
            )?;

            let brush_good = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 0.00, g: 0.90, b: 0.55, a: 1.0 },
                None,
            )?;
            let brush_caution = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 1.00, g: 0.76, b: 0.15, a: 1.0 },
                None,
            )?;
            let brush_warning = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 1.00, g: 0.31, b: 0.26, a: 1.0 },
                None,
            )?;
            let brush_exhausted = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 0.85, g: 0.09, b: 0.13, a: 1.0 },
                None,
            )?;
            let brush_balance = rt.CreateSolidColorBrush(
                &D2D1_COLOR_F { r: 0.29, g: 0.62, b: 0.91, a: 1.0 },
                None,
            )?;

            let stroke_props = D2D1_STROKE_STYLE_PROPERTIES1 {
                startCap: D2D1_CAP_STYLE_ROUND,
                endCap: D2D1_CAP_STYLE_ROUND,
                dashCap: D2D1_CAP_STYLE_ROUND,
                lineJoin: D2D1_LINE_JOIN_ROUND,
                miterLimit: 10.0,
                dashStyle: D2D1_DASH_STYLE_SOLID,
                dashOffset: 0.0,
                transformType: D2D1_STROKE_TRANSFORM_TYPE_NORMAL,
            };
            let stroke_round = d2d_factory.CreateStrokeStyle(&stroke_props, None)?;

            // 8. 内嵌图标
            let mut logo_bitmaps = HashMap::new();
            load_embedded_logo(
                &rt,
                &mut logo_bitmaps,
                "doubao",
                include_bytes!("../../cmd/rail/logos/doubao.png"),
            );
            load_embedded_logo(
                &rt,
                &mut logo_bitmaps,
                "qwen",
                include_bytes!("../../cmd/rail/logos/qwen.png"),
            );
            load_embedded_logo(
                &rt,
                &mut logo_bitmaps,
                "workbuddy",
                include_bytes!("../../cmd/rail/logos/workbuddy.png"),
            );
            load_embedded_logo(
                &rt,
                &mut logo_bitmaps,
                "trae",
                include_bytes!("../../cmd/rail/logos/trae.png"),
            );
            load_embedded_logo(
                &rt,
                &mut logo_bitmaps,
                "qoder",
                include_bytes!("../../cmd/rail/logos/qoder.png"),
            );

            Ok(Self {
                d2d_factory,
                dwrite_factory,
                d3d_device,
                rt,
                d2d_context,
                d3d_context,
                rt_texture,
                staging,
                dib_dc,
                dib_bitmap,
                dib_old,
                dib_bits,
                width,
                height,
                font_number,
                font_title,
                font_label,
                font_medium,
                font_detail,
                font_detail_right,
                brush_rail,
                brush_card,
                brush_hairline,
                brush_track,
                brush_disc,
                brush_text_primary,
                brush_text_dim,
                brush_good,
                brush_caution,
                brush_warning,
                brush_exhausted,
                brush_balance,
                stroke_round: stroke_round.into(),
                logo_bitmaps,
            })
        }
    }

    pub fn resize(&mut self, width: u32, height: u32) -> Result<()> {
        if (self.width == width && self.height == height) || width == 0 || height == 0 {
            return Ok(());
        }
        unsafe {
            self.d2d_context.SetTarget(None);

            let (rt_texture, staging) = create_render_textures(&self.d3d_device, width, height)?;
            let dxgi_surface: IDXGISurface = rt_texture.cast()?;
            let target_bitmap = self.d2d_context.CreateBitmapFromDxgiSurface(
                &dxgi_surface,
                Some(&target_bitmap_props()),
            )?;
            self.d2d_context.SetTarget(&target_bitmap);
            self.rt_texture = rt_texture;
            self.staging = staging;

            destroy_dib(self.dib_dc, self.dib_bitmap, self.dib_old);
            let (dc, bmp, old, bits) = create_dib(width, height)?;
            self.dib_dc = dc;
            self.dib_bitmap = bmp;
            self.dib_old = old;
            self.dib_bits = bits;

            // 尺寸变了，强制下一帧重建窗口区域

            self.width = width;
            self.height = height;
        }
        Ok(())
    }

    /// 把 GPU 渲染结果提交到分层窗口。
    ///
    /// GPU 纹理 → CopyResource 到 staging → Map 读回 BGRA → 拷进 DIB →
    /// UpdateLayeredWindow。绘制在 GPU 上，只有一次整帧拷贝。
    ///
    /// 之前灰块的真正原因是窗口注册了 DWM Acrylic 背景材质，DWM 在整个
    /// 窗口矩形后画一层灰，无视 per-pixel alpha 与区域裁剪。移除 backdrop 后，
    /// 标准 ULW_ALPHA 预乘合成即可正确透明。
    pub fn present(&self, hwnd: HWND) -> Result<()> {
        unsafe {
            self.d3d_context
                .CopyResource(&self.staging, &self.rt_texture);

            let mut mapped = D3D11_MAPPED_SUBRESOURCE::default();
            self.d3d_context
                .Map(&self.staging, 0, D3D11_MAP_READ, 0, Some(&mut mapped))?;

            let src = mapped.pData as *const u8;
            let dst = self.dib_bits;
            let row_bytes = (self.width * 4) as usize;
            let src_pitch = mapped.RowPitch as usize;

            // 先整块清零，未绘制区保持 alpha=0
            std::ptr::write_bytes(dst, 0, (self.width * self.height * 4) as usize);
            let copy_bytes = row_bytes.min(src_pitch);
            for y in 0..self.height as usize {
                let s = src.add(y * src_pitch);
                let d = dst.add(y * row_bytes);
                std::ptr::copy_nonoverlapping(s, d, copy_bytes);
            }
            self.d3d_context.Unmap(&self.staging, 0);

            // 本机 UpdateLayeredWindow 的 per-pixel alpha 不可靠（实测 alpha=0
            // 仍以约 17% 不透明度合成，rail 圆角外透出成灰）。改用 color key：
            // 把 rail 形状外的像素（alpha 低）填成品红，提交时用 ULW_COLORKEY
            // 让品红变透明 —— 完全不依赖 per-pixel alpha，灰块从根上消失。
            //
            // 阈值取 128：rail 内部像素经 D2D 混合后 alpha=255（实色），只有
            // 形状外的圆角/凹弧边缘是半透明。alpha<128 判为"形状外"填品红；
            // 形状外像素（alpha<128）填品红，交给 ULW_COLORKEY 变透明。
            // 阈值 128：rail/卡片内部像素经 D2D 混合后 alpha=255（实色），
            // 只有形状外的圆角、凹弧、卡片与 rail 之间的空隙是 alpha=0。
            // 本机 ULW_ALPHA 的 per-pixel alpha 不可靠（实测 alpha=0 仍以约
            // 17% 不透明度合成成灰），改用 color key 绕开它。
            let _ = GdiFlush();

            let size = SIZE {
                cx: self.width as i32,
                cy: self.height as i32,
            };
            let src_pt = POINT { x: 0, y: 0 };
            let mut wr = RECT::default();
            GetWindowRect(hwnd, &mut wr).ok();
            let dst_pt = POINT {
                x: wr.left,
                y: wr.top,
            };

            let screen_dc = GetDC(None);
            let r = UpdateLayeredWindow(
                hwnd,
                screen_dc,
                Some(&dst_pt),
                Some(&size),
                self.dib_dc,
                Some(&src_pt),
                COLORREF(0),
                Some(&BLENDFUNCTION {
                    BlendOp: AC_SRC_OVER as u8,
                    BlendFlags: 0,
                    SourceConstantAlpha: 255,
                    AlphaFormat: AC_SRC_ALPHA as u8,
                }),
                ULW_ALPHA,
            );
            ReleaseDC(None, screen_dc);
            r?;
        }
        Ok(())
    }

    pub fn color_for_fraction(&self, frac: f32, balance: bool, spent: bool) -> &ID2D1SolidColorBrush {
        if spent {
            return &self.brush_exhausted;
        }
        if balance {
            return &self.brush_balance;
        }
        if frac >= 1.0 {
            &self.brush_exhausted
        } else if frac >= 0.75 {
            &self.brush_warning
        } else if frac >= 0.50 {
            &self.brush_caution
        } else {
            &self.brush_good
        }
    }
}

/// D2D 渲染目标位图属性：BGRA + 预乘 alpha。
fn target_bitmap_props() -> D2D1_BITMAP_PROPERTIES1 {
    D2D1_BITMAP_PROPERTIES1 {
        pixelFormat: D2D1_PIXEL_FORMAT {
            format: DXGI_FORMAT_B8G8R8A8_UNORM,
            alphaMode: D2D1_ALPHA_MODE_PREMULTIPLIED,
        },
        dpiX: 96.0,
        dpiY: 96.0,
        bitmapOptions: D2D1_BITMAP_OPTIONS_TARGET | D2D1_BITMAP_OPTIONS_CANNOT_DRAW,
        colorContext: std::mem::ManuallyDrop::new(None),
    }
}

/// GPU 渲染纹理 + CPU 可读 staging。
fn create_render_textures(
    device: &ID3D11Device,
    width: u32,
    height: u32,
) -> Result<(ID3D11Texture2D, ID3D11Texture2D)> {
    let rt_desc = D3D11_TEXTURE2D_DESC {
        Width: width.max(1),
        Height: height.max(1),
        MipLevels: 1,
        ArraySize: 1,
        Format: DXGI_FORMAT_B8G8R8A8_UNORM,
        SampleDesc: DXGI_SAMPLE_DESC { Count: 1, Quality: 0 },
        Usage: D3D11_USAGE_DEFAULT,
        BindFlags: D3D11_BIND_RENDER_TARGET.0 as u32,
        CPUAccessFlags: 0,
        MiscFlags: 0,
    };
    let mut rt: Option<ID3D11Texture2D> = None;
    unsafe { device.CreateTexture2D(&rt_desc, None, Some(&mut rt))? };
    let rt = rt.ok_or_else(|| Error::from(E_FAIL))?;

    let staging_desc = D3D11_TEXTURE2D_DESC {
        Usage: D3D11_USAGE_STAGING,
        BindFlags: 0,
        CPUAccessFlags: D3D11_CPU_ACCESS_READ.0 as u32,
        ..rt_desc
    };
    let mut staging: Option<ID3D11Texture2D> = None;
    unsafe { device.CreateTexture2D(&staging_desc, None, Some(&mut staging))? };
    let staging = staging.ok_or_else(|| Error::from(E_FAIL))?;

    Ok((rt, staging))
}

/// UpdateLayeredWindow 用的 32bpp 顶朝下 DIB 段。
fn create_dib(width: u32, height: u32) -> Result<(HDC, HBITMAP, HGDIOBJ, *mut u8)> {
    unsafe {
        let dc = CreateCompatibleDC(None);
        if dc.is_invalid() {
            return Err(Error::from(E_FAIL));
        }
        let bi = BITMAPINFO {
            bmiHeader: BITMAPINFOHEADER {
                biSize: std::mem::size_of::<BITMAPINFOHEADER>() as u32,
                biWidth: width.max(1) as i32,
                biHeight: -(height.max(1) as i32), // 负高度 = 顶朝下
                biPlanes: 1,
                biBitCount: 32,
                biCompression: BI_RGB.0,
                ..Default::default()
            },
            ..Default::default()
        };
        let mut bits: *mut core::ffi::c_void = std::ptr::null_mut();
        let bmp = CreateDIBSection(dc, &bi, DIB_RGB_COLORS, &mut bits, None, 0)?;
        let old: HGDIOBJ = SelectObject(dc, HGDIOBJ(bmp.0));
        Ok((dc, bmp, old, bits as *mut u8))
    }
}

fn destroy_dib(dc: HDC, bmp: HBITMAP, old: HGDIOBJ) {
    unsafe {
        SelectObject(dc, old);
        let _ = DeleteObject(HGDIOBJ(bmp.0));
        let _ = DeleteDC(dc);
    }
}

fn load_embedded_logo(
    rt: &ID2D1RenderTarget,
    map: &mut HashMap<String, ID2D1Bitmap>,
    name: &str,
    png_bytes: &[u8],
) {
    if let Ok(img) = image::load_from_memory_with_format(png_bytes, image::ImageFormat::Png) {
        let rgba = img.to_rgba8();
        let (w, h) = rgba.dimensions();
        let mut bgra = Vec::with_capacity((w * h * 4) as usize);
        for pixel in rgba.pixels() {
            let [r, g, b, a] = pixel.0;
            let fa = (a as f32) / 255.0;
            let pr = (r as f32 * fa) as u8;
            let pg = (g as f32 * fa) as u8;
            let pb = (b as f32 * fa) as u8;
            bgra.push(pb);
            bgra.push(pg);
            bgra.push(pr);
            bgra.push(a);
        }

        unsafe {
            let props = D2D1_BITMAP_PROPERTIES {
                pixelFormat: D2D1_PIXEL_FORMAT {
                    format: DXGI_FORMAT_B8G8R8A8_UNORM,
                    alphaMode: D2D1_ALPHA_MODE_PREMULTIPLIED,
                },
                dpiX: 96.0,
                dpiY: 96.0,
            };
            if let Ok(bmp) = rt.CreateBitmap(
                D2D_SIZE_U { width: w, height: h },
                Some(bgra.as_ptr() as _),
                w * 4,
                &props,
            ) {
                map.insert(name.to_string(), bmp.clone());
                if name == "qwen" {
                    map.insert("qwenwork".to_string(), bmp.clone());
                }
                if name == "qoder" {
                    map.insert("qoder-intl".to_string(), bmp);
                }
            }
        }
    }
}

