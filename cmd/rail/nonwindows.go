//go:build !windows

package main

import (
	"fmt"
	"os"
)

// 浮动栏依赖 Win32 分层窗口与 GDI+，其它平台只有命令行取数。
func main() {
	fmt.Fprintln(os.Stderr, "浮动栏目前仅支持 Windows。")
	fmt.Fprintln(os.Stderr, "其它平台可用 curl 查看同一份数据：")
	fmt.Fprintln(os.Stderr, "  curl -s -X POST http://127.0.0.1:8313/api/auth -d '{\"password\":\"123456\"}' -c /tmp/c.txt")
	fmt.Fprintln(os.Stderr, "  curl -s http://127.0.0.1:8313/api/overview -b /tmp/c.txt")
	os.Exit(1)
}
