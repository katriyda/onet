//go:build !windows

package main

// pty_unix.go — Unix/Linux/macOS 后端：ptygo（内部为 creack/pty）。
// Unix 下子进程退出后 master fd 读返回 EOF，天然无 Windows 的管道阻塞问题。

import (
	"os"

	"github.com/gsw945/ptyterminal-go/ptygo"
)

// spawnShell 拉起 shell（$SHELL 或 bash）。
func spawnShell(cfg Config, name string, cols, rows int) (Pty, error) {
	shell := cfg.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "bash"
		}
	}
	cwd := cfg.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	return ptygo.Start(ptygo.Options{
		Name: name,
		Rows: rows,
		Cols: cols,
		Cwd:  cwd,
		App:  shell,
	})
}
