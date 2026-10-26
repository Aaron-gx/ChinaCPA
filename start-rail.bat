@echo off
chcp 65001 >nul
title ChinaCPA 浮动栏 (Pulse 质感)
cd /d "%~dp0"
if not exist rail.exe (
  echo [Rail] 未找到 rail.exe，正在使用 Cargo 构建...
  cargo build --release --manifest-path rail-rs/Cargo.toml
  copy /y rail-rs\target\release\rail-rs.exe rail.exe
)
echo [Rail] 启动中... 贴屏幕右侧，鼠标悬停展开，右键菜单
rail.exe %*
