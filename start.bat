@echo off
chcp 65001 >nul
title ChinaCPA 国产反代网关
cd /d "%~dp0"
if not exist chinacpa.exe (
  echo [ChinaCPA] 未找到 chinacpa.exe，正在构建...
  go build -o chinacpa.exe ./cmd/chinacpa
)
echo [ChinaCPA] 启动中... 面板 http://127.0.0.1:8313/
chinacpa.exe -config config.json
pause
