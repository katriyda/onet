//go:build !windows

package main

// pty_unix.go — Unix/Linux/macOS 后端：直接使用 creack/pty。
// Unix 下子进程退出后 master fd 读返回 EOF。

import (
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// unixPty 封装 creack/pty 的 pty session。
type unixPty struct {
	cmd  *exec.Cmd
	f    *os.File
	done chan struct{}

	once     sync.Once
	closeErr error
	waitErr  error
	exitCode int
}

func spawnShell(cfg Config, name string, cols, rows int) (Pty, error) {
	_ = name // 保留接口兼容

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

	cmd := exec.Command(shell)
	cmd.Dir = cwd

	env := os.Environ()
	sanitized := make([]string, 0, len(env)+8)
	sanitized = append(sanitized, env...)
	sanitized = append(sanitized,
		"TERM=xterm-256color",
		"TMUX=",
		"TMUX_PANE=",
		"STY=",
		"WINDOW=",
		"WINDOWID=",
		"TERMCAP=",
		"COLUMNS=",
		"LINES=",
	)
	cmd.Env = sanitized

	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	f, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
	if err != nil {
		return nil, err
	}

	c := &unixPty{
		cmd:  cmd,
		f:    f,
		done: make(chan struct{}),
	}
	go c.waitLoop()
	return c, nil
}

func (c *unixPty) waitLoop() {
	c.waitErr = c.cmd.Wait()
	if c.cmd.ProcessState != nil {
		c.exitCode = c.cmd.ProcessState.ExitCode()
	}
	close(c.done)
}

func (c *unixPty) Read(p []byte) (int, error) {
	return c.f.Read(p)
}

func (c *unixPty) Write(p []byte) (int, error) {
	return c.f.Write(p)
}

func (c *unixPty) Close() error {
	c.once.Do(func() {
		c.closeErr = c.f.Close()
		select {
		case <-c.done:
		default:
			_ = c.Kill()
		}
	})
	return c.closeErr
}

func (c *unixPty) Pid() int {
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *unixPty) ExitCode() int {
	select {
	case <-c.done:
		return c.exitCode
	default:
		return 0
	}
}

func (c *unixPty) Resize(cols, rows int) error {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return pty.Setsize(c.f, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
}

func (c *unixPty) Kill() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	pid := c.cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGHUP); err == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

func (c *unixPty) Done() <-chan struct{} {
	return c.done
}
