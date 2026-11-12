@echo off
chcp 65001 >nul
title ChinaCPA 国产反代网关
cd /d "%~dp0"
tasklist /FI "IMAGENAME eq chinacpa.exe" 2>NUL | find /I "chinacpa.exe" >NUL
if not errorlevel 1 (
  echo [ChinaCPA] 检测到 chinacpa.exe 已在运行，请先关闭旧窗口再启动。
  pause
  exit /b 1
)
echo [ChinaCPA] 构建中...
go build -o chinacpa.exe ./cmd/chinacpa
if errorlevel 1 (
  echo [ChinaCPA] 构建失败！未启动新版，请检查上方编译错误。
  pause
  exit /b 1
)
echo [ChinaCPA] 启动中... 面板 http://127.0.0.1:8313/
chinacpa.exe -config config.json
pause
