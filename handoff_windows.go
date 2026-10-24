//go:build windows

package main

// handoff_windows.go — Windows 热更新：启动新进程，把 ConPTY/PTY 句柄复制进子进程，
// 通过 stdin/stdout 做 LOAD/READY/GO/SERVING 握手；旧进程确认新进程已服务后退出。
// 监听 socket 不复制：父进程先关闭 listener，子进程重新 bind（见 handoffInheritsListener）。

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func ptyFromHandoff(hs handoffSession) (Pty, error) {
	return newInheritedConpty(hs)
}

// closeHandoffHandles 关闭死会话在子进程中复制出来的原始句柄。
// 这里没有父进程的 HPCON 指针，只能逐个 CloseHandle。
func closeHandoffHandles(handles []uint64) {
	for _, h := range handles {
		if h != 0 {
			_ = windows.CloseHandle(windows.Handle(h))
		}
	}
}

// handoffInheritsListener Windows 不复制监听 socket；子进程在父进程关闭监听后重新 bind。
// Windows socket 与 IOCP 关联，跨进程复制监听句柄不可靠。
func handoffInheritsListener() bool { return false }

// listenerFromHandoff 仅在 handoffInheritsListener 为 true 时调用；Windows 不用。
func listenerFromHandoff(_ *handoffState) (net.Listener, error) {
	return nil, fmt.Errorf("Windows 不支持复制监听 socket")
}

// readPseudoConsole 从 HPCON 指向的进程堆内存读取 PseudoConsole ABI。
// HPCON 是 winconpty 内部的 PseudoConsole*，不能像内核句柄一样 DuplicateHandle。
func readPseudoConsole(addr uintptr) (pseudoConsole, error) {
	var pc pseudoConsole
	const size = 3 * 8 // 64 位 Windows：hSignal / hPtyReference / hConPtyProcess
	var buf [size]byte
	var n uintptr
	if err := windows.ReadProcessMemory(windows.CurrentProcess(), addr, &buf[0], size, &n); err != nil {
		return pc, err
	}
	if n < size {
		return pc, fmt.Errorf("读取 %d 字节，期望 %d", n, size)
	}
	pc.hSignal = windows.Handle(binary.LittleEndian.Uint64(buf[0:8]))
	pc.hPtyReference = windows.Handle(binary.LittleEndian.Uint64(buf[8:16]))
	pc.hConPtyProcess = windows.Handle(binary.LittleEndian.Uint64(buf[16:24]))
	if pc.hSignal == 0 || pc.hPtyReference == 0 || pc.hConPtyProcess == 0 {
		return pc, fmt.Errorf("PseudoConsole 句柄为空")
	}
	return pc, nil
}

// duplicateTo 把 handle 复制进目标进程，返回目标进程中的 handle 值。
func duplicateTo(target windows.Handle, h windows.Handle) (uint64, error) {
	if h == 0 {
		return 0, fmt.Errorf("无效句柄")
	}
	var out windows.Handle
	err := windows.DuplicateHandle(
		windows.CurrentProcess(), h, target, &out,
		0, false, windows.DUPLICATE_SAME_ACCESS)
	if err != nil {
		return 0, err
	}
	return uint64(out), nil
}

func duplicateSessionHandles(sess *Session, target windows.Handle) ([]uint64, int, int64, error) {
	sess.mu.Lock()
	l := sess.live
	sess.mu.Unlock()
	if l == nil {
		return nil, 0, 0, nil
	}
	c, ok := l.pty.(*conptyConn)
	if !ok || c.cmdInFile == nil || c.cmdOutFile == nil {
		return nil, 0, 0, fmt.Errorf("会话 %s 不是 ConPTY", sess.ID)
	}
	var pc pseudoConsole
	if c.inherited != nil {
		pc = *c.inherited
	} else {
		if c.hpc == 0 {
			return nil, 0, 0, fmt.Errorf("会话 %s 的 ConPTY 句柄为空", sess.ID)
		}
		var err error
		pc, err = readPseudoConsole(uintptr(c.hpc))
		if err != nil {
			return nil, 0, 0, fmt.Errorf("读取会话 %s 的 ConPTY 句柄失败: %w", sess.ID, err)
		}
	}
	src := []windows.Handle{
		pc.hSignal,
		pc.hPtyReference,
		pc.hConPtyProcess,
		c.ptyIn,
		c.ptyOut,
		windows.Handle(c.cmdInFile.Fd()),
		windows.Handle(c.cmdOutFile.Fd()),
		c.pi.Process,
	}
	out := make([]uint64, len(src))
	for i, h := range src {
		v, err := duplicateTo(target, h)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("复制会话 %s 句柄 %d 失败: %w", sess.ID, i, err)
		}
		out[i] = v
	}
	return out, c.Pid(), l.started.UnixNano(), nil
}

// lineReader 从子进程 stdout 读取协议行，带超时。
type lineReader struct {
	br *bufio.Reader
}

func (r *lineReader) read(timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.br.ReadString('\n')
		ch <- result{strings.TrimSpace(line), err}
	}()
	select {
	case v := <-ch:
		return v.line, v.err
	case <-time.After(timeout):
		return "", fmt.Errorf("等待新进程响应超时")
	}
}

// startHandoffPlatform 启动新版本子进程，完成句柄交接并等待其开始服务。
// 成功时通知 main 优雅退出（不杀会话）；失败时恢复旧读循环并继续服务。
func startHandoffPlatform(s *Server, st *handoffState, sessions []*Session, statePath, exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = withHandoffEnv(os.Environ(), statePath)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("创建子进程 stdin 失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建子进程 stdout 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动新版本失败: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var stopOnce sync.Once
	var stopErr error
	stopChild := func() {
		stopOnce.Do(func() {
			_ = cmd.Process.Kill()
			select {
			case stopErr = <-waitCh:
			case <-time.After(2 * time.Second):
			}
		})
	}
	success := false
	defer func() {
		if !success {
			stopChild()
			_ = os.Remove(statePath)
		}
	}()

	target, err := windows.OpenProcess(windows.PROCESS_DUP_HANDLE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("打开子进程句柄失败: %w", err)
	}
	defer windows.CloseHandle(target)

	for i, sess := range sessions {
		handles, pid, started, err := duplicateSessionHandles(sess, target)
		if err != nil {
			return err
		}
		if len(handles) == 0 {
			continue
		}
		st.Sessions[i].Handles = handles
		st.Sessions[i].PID = pid
		st.Sessions[i].Started = started
	}

	if err := writeHandoffState(statePath, st); err != nil {
		return fmt.Errorf("写入交接状态失败: %w", err)
	}
	if _, err := fmt.Fprintln(stdin, "LOAD"); err != nil {
		return fmt.Errorf("通知新进程加载失败: %w", err)
	}
	lr := &lineReader{br: bufio.NewReader(stdout)}
	line, err := lr.read(30 * time.Second)
	if err != nil {
		return fmt.Errorf("新进程加载失败: %w", err)
	}
	if line != "READY" {
		return fmt.Errorf("新进程未就绪: %s", line)
	}

	// 从这里开始旧读循环停止；后续失败必须恢复。
	stopSessionsForHandoff(sessions)
	refreshHandoff(st, sessions)
	if err := writeHandoffState(statePath, st); err != nil {
		resumeWindowsSessions(sessions, st)
		return fmt.Errorf("写入最终交接状态失败: %w", err)
	}
	// 最终状态已落盘，释放监听让子进程重新 bind。Serve 的返回错误由 handoffClosing 抑制。
	s.handoffClosing.Store(true)
	if err := s.ln.Close(); err != nil {
		s.handoffClosing.Store(false)
		resumeWindowsSessions(sessions, st)
		return fmt.Errorf("释放监听失败: %w", err)
	}
	if _, err := fmt.Fprintln(stdin, "GO"); err != nil {
		resumeWindowsSessions(sessions, st)
		_ = s.reopenListener()
		return fmt.Errorf("通知新进程接管失败: %w", err)
	}
	_ = stdin.Close()

	line, err = lr.read(30 * time.Second)
	if err != nil || line != "SERVING" {
		stopChild()
		appLog.Error("app.handoff_serving_failed", "line", line, "err", err, "wait_err", stopErr)

		resumeWindowsSessions(sessions, st)
		if rerr := s.reopenListener(); rerr != nil {
			appLog.Error("app.handoff_relisten_failed", "err", rerr)
		}
		if err != nil {
			return fmt.Errorf("新进程未能开始服务: %w", err)
		}
		return fmt.Errorf("新进程未能开始服务: %s", line)
	}

	success = true
	_ = os.Remove(statePath)
	appLog.Info("app.handoff_serving", "exe", exe, "sessions", len(sessions))
	// 先断开旧 WS，main 随后 Shutdown 并退出；子进程继续持有监听 socket 与 PTY。
	s.closeClients()
	s.closeHandoff()
	return nil
}

// resumeWindowsSessions 在新进程失败后重启旧 ConPTY 读循环。
func resumeWindowsSessions(sessions []*Session, st *handoffState) {
	for i, s := range sessions {
		if i >= len(st.Sessions) {
			break
		}
		s.mu.Lock()
		l := s.live
		s.handoff = false
		s.mu.Unlock()
		if l == nil {
			continue
		}
		c, ok := l.pty.(*conptyConn)
		if !ok {
			continue
		}
		if err := c.RestartRead(); err != nil {
			appLog.Error("app.handoff_resume_failed", "session", s.ID, "err", err)
			s.mu.Lock()
			if s.live == l {
				s.live = nil
				s.dead = true
				s.exitCode = c.ExitCode()
			}
			s.mu.Unlock()
			if s.onStateChange != nil {
				s.onStateChange()
			}
			continue
		}
		nl := &livePTY{pty: c, done: make(chan struct{}), started: l.started}
		s.mu.Lock()
		s.live = nl
		s.mu.Unlock()
		go s.run(nl)
		appLog.Warn("app.handoff_resume", "session", s.ID)
	}
}

// Windows 父子握手：子进程加载初始状态并回报 READY，父进程 GO 后重新读取最终状态。
func handoffAwaitFinalState(path string, initial *handoffState) (*handoffState, error) {
	line, err := readStdinLine(30 * time.Second)
	if err != nil {
		return nil, fmt.Errorf("等待 LOAD 失败: %w", err)
	}
	if line != "LOAD" {
		return nil, fmt.Errorf("收到未知握手命令 %q", line)
	}
	if err := writeStdoutLine("READY"); err != nil {
		return nil, err
	}
	line, err = readStdinLine(30 * time.Second)
	if err != nil {
		return nil, fmt.Errorf("等待 GO 失败: %w", err)
	}
	if line != "GO" {
		return nil, fmt.Errorf("收到未知握手命令 %q", line)
	}
	return loadHandoffState(path)
}

var stdinReader = bufio.NewReader(os.Stdin)

func readStdinLine(timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := stdinReader.ReadString('\n')
		ch <- result{strings.TrimSpace(line), err}
	}()
	select {
	case v := <-ch:
		return v.line, v.err
	case <-time.After(timeout):
		return "", fmt.Errorf("读取 stdin 超时")
	}
}

var stdoutMu sync.Mutex

func writeStdoutLine(line string) error {
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	_, err := fmt.Fprintln(os.Stdout, line)
	return err
}

func handoffSignalServing() {
	_ = writeStdoutLine("SERVING")
}
