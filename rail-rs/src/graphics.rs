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
    Win32::Graphics::DirectComposition::*,
};

use std::collections::HashMap;

pub struct D2DEngine {
    pub d2d_factory: ID2D1Factory1,
    pub dwrite_factory: IDWriteFactory,
    pub d3d_device: ID3D11Device,
    pub d2d_context: ID2D1DeviceContext,
    pub rt: ID2D1RenderTarget,
    pub swap_chain: IDXGISwapChain1,
    pub dcomp_target: IDCompositionTarget,
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
    pub fn new(hwnd: HWND, width: u32, height: u32) -> Result<Self> {
        unsafe {
            let d2d_factory: ID2D1Factory1 = D2D1CreateFactory(
                D2D1_FACTORY_TYPE_SINGLE_THREADED,
                None,
            )?;

            let dwrite_factory: IDWriteFactory = DWriteCreateFactory(DWRITE_FACTORY_TYPE_SHARED)?;

            // 1. 创建 D3D11 设备 (支持 BGRA)
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
            let dxgi_device: IDXGIDevice = d3d_device.cast()?;

            // 2. 创建 Direct2D DeviceContext 和 RenderTarget
            let d2d_device = d2d_factory.CreateDevice(&dxgi_device)?;
            let d2d_context = d2d_device.CreateDeviceContext(D2D1_DEVICE_CONTEXT_OPTIONS_NONE)?;
            let rt: ID2D1RenderTarget = d2d_context.cast()?;

            // 3. 创建 DXGI SwapChainForComposition (预乘 Alpha)
            let dxgi_adapter = dxgi_device.GetAdapter()?;
            let dxgi_factory: IDXGIFactory2 = dxgi_adapter.GetParent()?;

            let swap_chain_desc = DXGI_SWAP_CHAIN_DESC1 {
                Width: width.max(1),
                Height: height.max(1),
                Format: DXGI_FORMAT_B8G8R8A8_UNORM,
                Stereo: false.into(),
                SampleDesc: DXGI_SAMPLE_DESC { Count: 1, Quality: 0 },
                BufferUsage: DXGI_USAGE_RENDER_TARGET_OUTPUT,
                BufferCount: 2,
                Scaling: DXGI_SCALING_STRETCH,
                SwapEffect: DXGI_SWAP_EFFECT_FLIP_SEQUENTIAL,
                AlphaMode: DXGI_ALPHA_MODE_PREMULTIPLIED,
                Flags: 0,
            };

            let swap_chain = dxgi_factory.CreateSwapChainForComposition(&dxgi_device, &swap_chain_desc, None)?;

            // 4. 将 SwapChain 挂载到 DirectComposition Visual Tree
            let dcomp_device: IDCompositionDevice = DCompositionCreateDevice(&dxgi_device)?;
            let dcomp_target = dcomp_device.CreateTargetForHwnd(hwnd, true)?;
            let visual = dcomp_device.CreateVisual()?;
            visual.SetContent(&swap_chain)?;
            dcomp_target.SetRoot(&visual)?;
            dcomp_device.Commit()?;

            // 5. 设置 Direct2D 渲染目标
            let dxgi_back_buffer: IDXGISurface = swap_chain.GetBuffer(0)?;
            let target_bitmap = d2d_context.CreateBitmapFromDxgiSurface(&dxgi_back_buffer, None)?;
            d2d_context.SetTarget(&target_bitmap);

            // 6. DirectWrite 字体（等宽数字、高清晰度）
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
            let brush_rail = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 0.015, g: 0.015, b: 0.02, a: 0.94 }, None)?;
            let brush_card = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 0.06, g: 0.06, b: 0.07, a: 0.96 }, None)?;
            let brush_hairline = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 1.0, g: 1.0, b: 1.0, a: 0.12 }, None)?;
            let brush_track = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 1.0, g: 1.0, b: 1.0, a: 0.18 }, None)?;
            let brush_disc = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 0.04, g: 0.04, b: 0.05, a: 1.0 }, None)?;
            let brush_text_primary = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 0.96, g: 0.96, b: 0.96, a: 1.0 }, None)?;
            let brush_text_dim = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 1.0, g: 1.0, b: 1.0, a: 0.45 }, None)?;

            let brush_good = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 0.00, g: 0.90, b: 0.55, a: 1.0 }, None)?;
            let brush_caution = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 1.00, g: 0.76, b: 0.15, a: 1.0 }, None)?;
            let brush_warning = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 1.00, g: 0.31, b: 0.26, a: 1.0 }, None)?;
            let brush_exhausted = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 0.85, g: 0.09, b: 0.13, a: 1.0 }, None)?;
            let brush_balance = rt.CreateSolidColorBrush(&D2D1_COLOR_F { r: 0.29, g: 0.62, b: 0.91, a: 1.0 }, None)?;

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

            // 8. 加载内嵌图标
            let mut logo_bitmaps = HashMap::new();
            load_embedded_logo(&rt, &mut logo_bitmaps, "doubao", include_bytes!("../../cmd/rail/logos/doubao.png"));
            load_embedded_logo(&rt, &mut logo_bitmaps, "qwen", include_bytes!("../../cmd/rail/logos/qwen.png"));
            load_embedded_logo(&rt, &mut logo_bitmaps, "workbuddy", include_bytes!("../../cmd/rail/logos/workbuddy.png"));
            load_embedded_logo(&rt, &mut logo_bitmaps, "trae", include_bytes!("../../cmd/rail/logos/trae.png"));
            load_embedded_logo(&rt, &mut logo_bitmaps, "qoder", include_bytes!("../../cmd/rail/logos/qoder.png"));

            Ok(Self {
                d2d_factory,
                dwrite_factory,
                d3d_device,
                d2d_context,
                rt,
                swap_chain,
                dcomp_target,
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
            self.swap_chain.ResizeBuffers(
                0,
                width,
                height,
                DXGI_FORMAT_B8G8R8A8_UNORM,
                DXGI_SWAP_CHAIN_FLAG(0),
            )?;
            let dxgi_back_buffer: IDXGISurface = self.swap_chain.GetBuffer(0)?;
            let target_bitmap = self.d2d_context.CreateBitmapFromDxgiSurface(&dxgi_back_buffer, None)?;
            self.d2d_context.SetTarget(&target_bitmap);
            self.width = width;
            self.height = height;
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
