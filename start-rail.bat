@echo off
chcp 65001 >nul
title ChinaCPA 浮动栏
cd /d "%~dp0"
if not exist rail.exe (
  echo [Rail] 未找到 rail.exe，正在构建...
  go build -o rail.exe ./cmd/rail
)
echo [Rail] 启动中... 贴屏幕右侧，鼠标悬停展开，右键菜单
rail.exe %*
