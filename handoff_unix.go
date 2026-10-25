//go:build !windows

package main

// handoff_unix.go — Unix 热更新：dup 出 PTY/listener fd，写交接状态后 exec 新二进制。
// exec 保留 PID 与子进程关系，新进程直接 Wait4 原来的 shell。

import (
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func ptyFromHandoff(hs handoffSession) (Pty, error) {
	if len(hs.Handles) < 1 {
		return nil, fmt.Errorf("会话 %s 缺少 PTY fd", hs.ID)
	}
	f := os.NewFile(uintptr(hs.Handles[0]), "onet-pty")
	if f == nil {
		return nil, fmt.Errorf("无法恢复会话 %s 的 PTY fd %d", hs.ID, hs.Handles[0])
	}
	p := &unixPty{f: f, pid: hs.PID, done: make(chan struct{})}
	go p.waitInherited(hs.PID)
	return p, nil
}

func closeHandoffHandles(handles []uint64) {
	for _, h := range handles {
		_ = unix.Close(int(h))
	}
}

func handoffInheritsListener() bool { return true }

// handoffSupported 标记当前平台支持会话交接（客户端据此提示“会话会保留”）。
// Unix 用 exec 原地替换，PID 与进程组不变，无需 Setsid。
func handoffSupported() bool { return true }

func listenerFromHandoff(st *handoffState) (net.Listener, error) {
	if st.ListenerFD <= 0 {
		return nil, fmt.Errorf("未携带监听 fd")
	}
	f := os.NewFile(uintptr(st.ListenerFD), "onet-listener")
	if f == nil {
		return nil, fmt.Errorf("无法恢复监听 fd %d", st.ListenerFD)
	}
	ln, err := net.FileListener(f)
	_ = f.Close() // FileListener 内部 dup，原 fd 可关闭
	if err != nil {
		return nil, err
	}
	return ln, nil
}

// startHandoffPlatform 在原进程内 exec 新二进制；成功时永不返回。
// 失败时尽量把已停止的读循环接回当前进程，保证旧服务还能用。
func startHandoffPlatform(s *Server, st *handoffState, sessions []*Session, statePath, exe string) error {
	var fds []int
	cleanup := func() {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
	}

	for i, sess := range sessions {
		sess.mu.Lock()
		l := sess.live
		sess.mu.Unlock()
		if l == nil {
			continue
		}
		u, ok := l.pty.(*unixPty)
		if !ok || u.f == nil {
			cleanup()
			return fmt.Errorf("会话 %s 不是 Unix PTY，无法交接", sess.ID)
		}
		fd, err := unix.Dup(int(u.f.Fd()))
		if err != nil {
			cleanup()
			return fmt.Errorf("复制会话 %s 的 PTY fd 失败: %w", sess.ID, err)
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			_ = unix.Close(fd)
			cleanup()
			return fmt.Errorf("清除 PTY fd CLOEXEC 失败: %w", err)
		}
		fds = append(fds, fd)
		st.Sessions[i].Handles = []uint64{uint64(fd)}
		st.Sessions[i].PID = u.Pid()
		st.Sessions[i].Started = l.started.UnixNano()
	}

	var listenerFile *os.File
	if tl, ok := s.ln.(*net.TCPListener); ok {
		f, err := tl.File()
		if err != nil {
			cleanup()
			return fmt.Errorf("复制监听 socket 失败: %w", err)
		}
		fd := int(f.Fd())
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			_ = f.Close()
			cleanup()
			return fmt.Errorf("清除监听 fd CLOEXEC 失败: %w", err)
		}
		st.ListenerFD = fd
		listenerFile = f
	}

	stopSessionsForHandoff(sessions)
	refreshHandoff(st, sessions)
	// 状态文件必须先落盘；exec 后没有机会补写。
	if err := writeHandoffState(statePath, st); err != nil {
		_ = os.Remove(statePath)
		if listenerFile != nil {
			_ = listenerFile.Close()
		}
		resumeAfterFailedHandoff(sessions, st)
		return fmt.Errorf("写入交接状态失败: %w", err)
	}

	appLog.Info("app.handoff_exec", "exe", exe, "sessions", len(sessions), "state", statePath)
	env := withHandoffEnv(os.Environ(), statePath)
	argv := append([]string{exe}, os.Args[1:]...)
	runtime.KeepAlive(listenerFile) // 保持 dup fd 存活到 exec
	if err := unix.Exec(exe, argv, env); err != nil {
		appLog.Error("app.handoff_exec_failed", "err", err)
		_ = os.Remove(statePath)
		if listenerFile != nil {
			_ = listenerFile.Close()
		}
		resumeAfterFailedHandoff(sessions, st)
		return fmt.Errorf("启动新版本失败: %w", err)
	}
	return nil
}

// resumeAfterFailedHandoff 把 dup fd 接回旧 unixPty，并重启读循环。
func resumeAfterFailedHandoff(sessions []*Session, st *handoffState) {
	for i, s := range sessions {
		if i >= len(st.Sessions) || len(st.Sessions[i].Handles) == 0 {
			continue
		}
		s.mu.Lock()
		l := s.live
		s.handoff = false
		s.mu.Unlock()
		if l == nil {
			continue
		}
		u, ok := l.pty.(*unixPty)
		if !ok {
			continue
		}
		f := os.NewFile(uintptr(st.Sessions[i].Handles[0]), "onet-pty-resume")
		if f == nil {
			continue
		}
		u.f = f
		started := l.started
		nl := &livePTY{pty: u, done: make(chan struct{}), started: started}
		s.mu.Lock()
		s.live = nl
		s.mu.Unlock()
		go s.run(nl)
		appLog.Warn("app.handoff_resume", "session", s.ID)
	}
}

// handoffAwaitFinalState Unix 侧没有父子握手：exec 后直接读最终状态。
func handoffAwaitFinalState(path string, initial *handoffState) (*handoffState, error) {
	return initial, nil
}

func handoffSignalServing() {}
