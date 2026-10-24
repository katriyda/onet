package main

// handoff.go — 运行中的进程把 PTY、监听 socket 与会话缓冲交给新二进制。
// 平台差异：Unix 用 exec 原地替换；Windows 启动子进程并复制句柄。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	handoffStateVersion = 1
	handoffStateEnv     = "ONET_HANDOFF_STATE"
	handoffStateMax     = 32 << 20
)

// handoffSession 是可跨进程传递的单个会话快照。
// Handles 的含义由平台决定：Unix 为 master fd，Windows 为 ConPTY 句柄组。
type handoffSession struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Cols     int      `json:"cols"`
	Rows     int      `json:"rows"`
	Dead     bool     `json:"dead"`
	ExitCode int      `json:"exit_code"`
	PID      int      `json:"pid"`
	Started  int64    `json:"started"` // UnixNano
	Buffer   []byte   `json:"buffer,omitempty"`
	Handles  []uint64 `json:"handles,omitempty"`
}

type handoffState struct {
	Version    int              `json:"version"`
	Seq        int              `json:"seq"`
	Sessions   []handoffSession `json:"sessions"`
	ListenerFD int              `json:"listener_fd,omitempty"` // Unix
}

// writeHandoffState 原子写入状态文件；新进程只会在父进程完成后读取。
func writeHandoffState(path string, st *handoffState) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encErr := json.NewEncoder(f).Encode(st)
	if cerr := f.Close(); encErr == nil {
		encErr = cerr
	}
	if encErr != nil {
		_ = os.Remove(tmp)
		return encErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func loadHandoffState(path string) (*handoffState, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > handoffStateMax {
		return nil, fmt.Errorf("交接状态文件过大: %d 字节", fi.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st handoffState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("解析交接状态失败: %w", err)
	}
	if st.Version != handoffStateVersion {
		return nil, fmt.Errorf("不支持的交接状态版本 %d", st.Version)
	}
	return &st, nil
}

func createHandoffStatePath() (string, error) {
	f, err := os.CreateTemp("", "onet-handoff-*.json")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	_ = os.Chmod(path, 0o600)
	return path, nil
}

// handoffSnapshot 读取某个会话的稳定快照（调用方无需持锁）。
func (s *Session) handoffSnapshot() handoffSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	hs := handoffSession{
		ID:       s.ID,
		Name:     s.Name,
		Cols:     s.cols,
		Rows:     s.rows,
		Dead:     s.dead,
		ExitCode: s.exitCode,
	}
	if s.live != nil {
		hs.PID = s.live.pty.Pid()
		hs.Started = s.live.started.UnixNano()
	}
	if s.buf != nil {
		hs.Buffer = s.buf.Bytes()
	}
	return hs
}

// snapshotHandoff 生成会话列表快照；返回的 sessions 与状态项一一对应。
func snapshotHandoff(mgr *Manager) (*handoffState, []*Session) {
	sessions := mgr.sessionsSnapshot()
	mgr.mu.Lock()
	seq := mgr.seq
	mgr.mu.Unlock()
	st := &handoffState{
		Version:  handoffStateVersion,
		Seq:      seq,
		Sessions: make([]handoffSession, len(sessions)),
	}
	for i, s := range sessions {
		st.Sessions[i] = s.handoffSnapshot()
	}
	return st, sessions
}

// refreshHandoff 在停止旧读循环后刷新缓冲与存活状态，保留平台句柄。
func refreshHandoff(st *handoffState, sessions []*Session) {
	for i, s := range sessions {
		if i >= len(st.Sessions) {
			break
		}
		prev := st.Sessions[i]
		fresh := s.handoffSnapshot()
		fresh.Handles = prev.Handles
		if !fresh.Dead {
			// live 在交接期间保留；保险起见仍沿用交接前的 PID/启动时间。
			fresh.PID = prev.PID
			fresh.Started = prev.Started
		}
		st.Sessions[i] = fresh
	}
}

// stopSessionsForHandoff 停止所有旧读循环。PTY 本身保留给新进程。
func stopSessionsForHandoff(sessions []*Session) {
	for _, s := range sessions {
		s.suspendForHandoff()
	}
}

// withHandoffEnv 返回带去重后的交接状态路径环境变量。
func withHandoffEnv(env []string, path string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, handoffStateEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, handoffStateEnv+"="+path)
}

// RestoreHandoff 用交接状态重建 Manager。PTY 读循环稍后由 StartRestoredReads 启动。
func (m *Manager) RestoreHandoff(st *handoffState) error {
	m.mu.Lock()
	if len(m.sessions) != 0 {
		m.mu.Unlock()
		return fmt.Errorf("恢复交接状态时会话列表非空")
	}
	m.seq = st.Seq
	m.mu.Unlock()

	for i := range st.Sessions {
		hs := st.Sessions[i]
		cols, rows := hs.Cols, hs.Rows
		if cols <= 0 {
			cols = 80
		}
		if rows <= 0 {
			rows = 24
		}
		s := m.newSession(hs.ID, hs.Name, cols, rows)
		s.dead = hs.Dead
		s.exitCode = hs.ExitCode
		if len(hs.Buffer) > 0 {
			s.buf.Write(hs.Buffer)
		}

		if len(hs.Handles) > 0 {
			if hs.Dead {
				closeHandoffHandles(hs.Handles)
			} else {
				p, err := ptyFromHandoff(hs)
				if err != nil {
					appLog.Error("handoff.restore_pty_failed", "session", hs.ID, "err", err)
					closeHandoffHandles(hs.Handles)
					s.dead = true
					s.exitCode = -1
				} else {
					started := time.Unix(0, hs.Started)
					if hs.Started == 0 {
						started = time.Now()
					}
					s.live = &livePTY{pty: p, done: make(chan struct{}), started: started}
				}
			}
		} else if !s.dead {
			appLog.Error("handoff.restore_missing_pty", "session", hs.ID)
			s.dead = true
			s.exitCode = -1
		}

		m.mu.Lock()
		if _, exists := m.sessions[hs.ID]; exists {
			m.mu.Unlock()
			if s.live != nil {
				_ = s.live.pty.Close()
			}
			return fmt.Errorf("恢复时会话 ID 重复: %q", hs.ID)
		}
		m.sessions[hs.ID] = s
		m.order = append(m.order, hs.ID)
		m.mu.Unlock()
	}

	appLog.Info("handoff.restored", "sessions", len(st.Sessions))
	return nil
}

// StartRestoredReads 启动所有恢复会话的 PTY 读循环。
func (m *Manager) StartRestoredReads() {
	for _, s := range m.sessionsSnapshot() {
		s.startRestoredRead()
	}
}
