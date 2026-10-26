@echo off
chcp 65001 >nul
title ChinaCPA 一键启动 (网关 + Pulse 侧边栏)
cd /d "%~dp0"

echo [1/2] 正在启动 ChinaCPA 后端网关...
start "ChinaCPA Gateway" chinacpa.exe -config config.json

echo [2/2] 正在启动 Pulse 质感悬浮桌面挂件 (rail.exe)...
start "" "%~dp0rail.exe"

echo.
echo ========================================================
echo  ChinaCPA 网关与 Pulse 风格侧边栏已同时启动！
echo  - 管理面板: http://127.0.0.1:8313/
echo  - 桌面挂件: 已停靠在主屏幕最右侧
echo  - 鼠标移至右边缘展开，鼠标移出自动收起为 6px 呼吸细条
echo  - 鼠标悬停圆环滑出水滴一体化详情卡片，右键打开快捷菜单
echo ========================================================
timeout /t 3 >nul
