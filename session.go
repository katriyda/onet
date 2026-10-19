package main

// session.go — 会话核心：Pty 窄接口、Session（PTY 生命周期、广播、重放缓冲）、Manager。
// 业务代码只依赖本文件自定的 Pty 接口，不直接引用 ptygo，便于必要时换后端。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Pty 是本项目唯一的 PTY 抽象。ptygo.Connection 天然满足该接口。
type Pty interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Kill() error
	Pid() int
	Done() <-chan struct{}
	ExitCode() int
}

// ---------- ringBuf：固定 64KB 环形缓冲 ----------

const ringBufSize = 64 * 1024

type ringBuf struct {
	buf   []byte
	start int // 首个有效字节下标
	n     int // 有效字节数
}

func newRingBuf() *ringBuf {
	return &ringBuf{buf: make([]byte, ringBufSize)}
}

// Write 追加数据，溢出时丢弃最旧字节。
func (r *ringBuf) Write(p []byte) {
	if len(p) >= ringBufSize {
		r.n = copy(r.buf, p[len(p)-ringBufSize:])
		r.start = 0
		return
	}
	if r.n+len(p) > ringBufSize {
		drop := r.n + len(p) - ringBufSize
		r.start = (r.start + drop) % ringBufSize
		r.n -= drop
	}
	pos := (r.start + r.n) % ringBufSize
	if end := pos + len(p); end <= ringBufSize {
		copy(r.buf[pos:end], p)
	} else {
		first := ringBufSize - pos
		copy(r.buf[pos:], p[:first])
		copy(r.buf[:end-ringBufSize], p[first:])
	}
	r.n += len(p)
}

// Bytes 返回全部有效字节的副本。
func (r *ringBuf) Bytes() []byte {
	out := make([]byte, r.n)
	for i := range r.n {
		out[i] = r.buf[(r.start+i)%ringBufSize]
	}
	return out
}

// ---------- Client：WS 连接与发送队列 ----------

const clientSendCap = 256 // 慢客户端最多积压 256 帧，超出即丢帧

var clientSeq atomic.Int64 // 连接序号，日志里用 c<seq> 标识

type Client struct {
	id      int64
	conn    *websocket.Conn
	send    chan []byte
	done    chan struct{}
	dropped atomic.Int64 // 发送队列满导致的丢帧计数
}

func newClient(conn *websocket.Conn) *Client {
	return &Client{
		id:   clientSeq.Add(1),
		conn: conn,
		send: make(chan []byte, clientSendCap),
		done: make(chan struct{}),
	}
}

// logID 返回日志用的连接标识（c1、c2 …）。
func (c *Client) logID() string {
	return fmt.Sprintf("c%d", c.id)
}

// write 非阻塞：缓冲满直接丢弃该帧，防止慢客户端阻塞广播。
func (c *Client) write(v []byte) {
	select {
	case c.send <- v:
	default:
		// 只记首次与每 1000 次，避免长时间断网刷屏。
		if n := c.dropped.Add(1); n == 1 || n%1000 == 0 {
			appLog.Warn("ws.send_drop", "conn", c.logID(), "dropped", n)
		}
	}
}

// writeLoop 常驻协程，串行消费发送队列。由 server 启动与终止。
// 写失败后退出：连接状态已不可用，继续写只会重复报错。
func (c *Client) writeLoop() {
	defer func() {
		if p := recover(); p != nil {
			appLog.Error("ws.write_panic", "conn", c.logID(),
				"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	for {
		select {
		case v := <-c.send:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := c.conn.Write(ctx, websocket.MessageBinary, v)
			cancel()
			if err != nil {
				appLog.Warn("ws.write_error", "conn", c.logID(), "err", err)
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *Client) close() {
	close(c.done)
}

// ---------- 协议消息（session 侧：output / exit） ----------

type outputMsg struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Data string `json:"data"` // base64 原始输出
}

type exitMsg struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Code int    `json:"code"`
}

// ---------- Session ----------

// livePTY 绑定某代 PTY 与其 read 循环；done 在该代循环清理完成后关闭。
type livePTY struct {
	pty     Pty
	done    chan struct{}
	started time.Time
}

type Session struct {
	ID   string
	Name string

	mu            sync.Mutex
	live          *livePTY // nil 表示当前无存活进程
	clients       map[*Client]struct{}
	buf           *ringBuf
	dead          bool
	exitCode      int
	cols, rows    int
	logIO         bool // 是否记录 PTY 原始输入/输出（log_terminal_io）
	onStateChange func()

	spawn func(cols, rows int) (Pty, error)
}

// run 常驻协程：读取 pty 输出 → 环形缓冲 + 广播；进程退出后清理并广播 exit。
// 各平台后端保证：进程退出后 Read 必然返回 EOF（Windows ConPTY 由后端在
// 检测到退出时先 ClosePseudoConsole 解除读阻塞）。
func (s *Session) run(l *livePTY) {
	defer close(l.done)
	code := -1
	defer func() {
		if p := recover(); p != nil {
			appLog.Error("session.panic", "session", s.ID, "pid", l.pty.Pid(),
				"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			code = -1
		}
		s.finishRun(l, code)
	}()

	p := l.pty
	rbuf := make([]byte, 32768)

	for {
		n, err := p.Read(rbuf)
		if n > 0 {
			s.broadcastOutput(l, rbuf[:n])
		}
		if err != nil {
			appLog.Debug("pty.read_end", "session", s.ID, "pid", p.Pid(), "err", err)
			break
		}
	}
	code = p.ExitCode()
}

// finishRun 收尾一代 PTY：关闭句柄、标记 dead、广播 exit。正常退出与 panic 共用。
func (s *Session) finishRun(l *livePTY, code int) {
	p := l.pty
	_ = p.Close()

	s.mu.Lock()
	current := s.live == l // 仅当仍是当前代时才标记 dead（Restart 换代后跳过）
	if current {
		s.dead = true
		s.exitCode = code
		s.live = nil
	}
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	if current {
		appLog.Info("session.exit", "session", s.ID, "pid", p.Pid(), "code", code,
			"uptime_ms", time.Since(l.started).Milliseconds())
	} else {
		appLog.Debug("session.exit_stale", "session", s.ID, "pid", p.Pid(), "code", code)
	}

	frame, _ := json.Marshal(exitMsg{Type: "exit", ID: s.ID, Code: code})
	for _, c := range clients {
		c.write(frame)
	}
	if s.onStateChange != nil {
		s.onStateChange()
	}
}

func (s *Session) broadcastOutput(l *livePTY, data []byte) {
	s.mu.Lock()
	// 仅当前代写入重放缓冲：重启换代后，旧循环的残余输出不污染新缓冲
	if s.live == l {
		s.buf.Write(data)
	}
	logIO := s.logIO
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	b64 := base64.StdEncoding.EncodeToString(data)
	if logIO {
		appLog.Info("pty.output", "session", s.ID, "bytes", len(data), "data_b64", b64)
	}
	frame, _ := json.Marshal(outputMsg{
		Type: "output",
		ID:   s.ID,
		Data: b64,
	})
	for _, c := range clients {
		c.write(frame)
	}
}

var errSessionDead = fmt.Errorf("会话已退出")

// Attach 先重放缓冲（解决 detach 期间丢首屏），再登记客户端。
// 已死会话拒绝 attach。
func (s *Session) Attach(c *Client) error {
	s.mu.Lock()
	if s.dead || s.live == nil {
		s.mu.Unlock()
		appLog.Warn("session.attach_rejected", "session", s.ID, "conn", c.logID(), "err", errSessionDead)
		return errSessionDead
	}
	replayBytes := 0
	if replay := s.buf.Bytes(); len(replay) > 0 {
		replayBytes = len(replay)
		frame, _ := json.Marshal(outputMsg{
			Type: "output",
			ID:   s.ID,
			Data: base64.StdEncoding.EncodeToString(replay),
		})
		c.write(frame)
	}
	s.clients[c] = struct{}{}
	count := len(s.clients)
	s.mu.Unlock()

	appLog.Info("session.attach", "session", s.ID, "conn", c.logID(),
		"replay_bytes", replayBytes, "clients", count)
	return nil
}

func (s *Session) Detach(c *Client) {
	s.mu.Lock()
	delete(s.clients, c)
	count := len(s.clients)
	s.mu.Unlock()
	appLog.Info("session.detach", "session", s.ID, "conn", c.logID(), "clients", count)
}

// Attached 返回当前连接的客户端数量。
func (s *Session) Attached() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// Dead 返回会话进程是否已退出。
func (s *Session) Dead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

// Input 把客户端原始输入字节转发给当前 shell（已退出则忽略）。
func (s *Session) Input(data []byte) {
	s.mu.Lock()
	l := s.live
	s.mu.Unlock()
	if l != nil {
		if _, err := l.pty.Write(data); err != nil {
			appLog.Warn("pty.write_error", "session", s.ID, "err", err)
		}
	}
}

// Resize 调用 pty.Resize 并记录当前尺寸（无存活进程时仅记录，供重启使用）。
func (s *Session) Resize(cols, rows int) {
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	p := s.live
	s.mu.Unlock()
	if p != nil {
		if err := p.pty.Resize(cols, rows); err != nil {
			appLog.Warn("pty.resize_error", "session", s.ID, "err", err)
		} else {
			appLog.Debug("session.resize", "session", s.ID, "cols", cols, "rows", rows, "pid", p.pty.Pid())
		}
	}
}

// Kill 终止进程并等待 read 循环清理完成；会话保持 dead 状态（可重启）。
func (s *Session) Kill() {
	s.mu.Lock()
	l := s.live
	s.mu.Unlock()
	if l == nil {
		return
	}
	appLog.Info("session.kill", "session", s.ID, "pid", l.pty.Pid())
	s.terminate(l)
}

// Restart 杀掉旧进程（若活）并以同 ID 重新拉起新 shell。
func (s *Session) Restart() error {
	s.mu.Lock()
	old := s.live
	cols, rows := s.cols, s.rows
	s.mu.Unlock()

	oldPid := 0
	if old != nil {
		oldPid = old.pty.Pid()
		s.terminate(old)
	}

	p, err := s.spawn(cols, rows)
	if err != nil {
		appLog.Error("session.restart_failed", "session", s.ID, "err", err)
		return fmt.Errorf("重启会话失败: %w", err)
	}

	l := &livePTY{pty: p, done: make(chan struct{}), started: time.Now()}
	s.mu.Lock()
	s.live = l
	s.dead = false
	s.exitCode = 0
	s.buf = newRingBuf() // 新进程，清空重放缓冲
	s.mu.Unlock()

	go s.run(l)
	appLog.Info("session.restart", "session", s.ID, "old_pid", oldPid, "new_pid", p.Pid())
	if s.onStateChange != nil {
		s.onStateChange()
	}
	return nil
}

// terminate 终止进程并返回（不阻塞）。进程退出后 run 循环自行清理；
// 若进程未在期限内退出，则强制 Close 兜底（同时解除 ConPTY 读阻塞）。
func (s *Session) terminate(l *livePTY) {
	_ = l.pty.Kill()
	go func() {
		select {
		case <-l.done:
		case <-time.After(3 * time.Second):
			appLog.Warn("pty.kill_timeout", "session", s.ID, "pid", l.pty.Pid())
			_ = l.pty.Close()
		}
	}()
}

// info 返回会话状态快照（调用方需持有 s.mu）。
func (s *Session) info() SessionInfo {
	info := SessionInfo{
		ID:       s.ID,
		Name:     s.Name,
		Dead:     s.dead,
		ExitCode: s.exitCode,
		Attached: len(s.clients),
	}
	if s.live != nil {
		info.PID = s.live.pty.Pid()
	}
	return info
}

// ---------- Manager ----------

type SessionInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	PID      int    `json:"pid"`
	Dead     bool   `json:"dead"`
	ExitCode int    `json:"exitCode"`
	Attached int    `json:"attached"`
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	order    []string // 创建顺序，稳定展示
	seq      int
	cfg      Config
	onChange func() // server 回调：广播 sessions 列表
}

func NewManager(cfg Config, onChange func()) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		cfg:      cfg,
		onChange: onChange,
	}
}

// Create 新建会话。name 为空 → "sess-<seq>"。
func (m *Manager) Create(name string) (*Session, error) {
	id := name
	if id == "" {
		m.mu.Lock()
		m.seq++
		id = fmt.Sprintf("sess-%d", m.seq)
		m.mu.Unlock()
	}

	m.mu.Lock()
	if _, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("会话 %q 已存在", id)
	}
	s := &Session{
		ID:            id,
		Name:          name,
		clients:       make(map[*Client]struct{}),
		buf:           newRingBuf(),
		cols:          80,
		rows:          24,
		logIO:         m.cfg.LogTerminalIO,
		onStateChange: m.onChange,
		spawn: func(cols, rows int) (Pty, error) {
			return spawnShell(m.cfg, id, cols, rows)
		},
	}
	m.sessions[id] = s
	m.order = append(m.order, id)
	m.mu.Unlock()

	p, err := s.spawn(80, 24)
	if err != nil {
		// 启动失败：回滚创建。
		m.mu.Lock()
		delete(m.sessions, id)
		m.order = m.order[:len(m.order)-1]
		m.mu.Unlock()
		appLog.Error("session.create_failed", "session", id, "name", name, "err", err)
		return nil, fmt.Errorf("启动会话失败: %w", err)
	}
	l := &livePTY{pty: p, done: make(chan struct{}), started: time.Now()}
	s.mu.Lock()
	s.live = l
	s.mu.Unlock()
	go s.run(l)

	appLog.Info("session.create", "session", id, "name", name, "pid", p.Pid())
	if m.onChange != nil {
		m.onChange()
	}
	return s, nil
}

func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Find 按 ID 或 Name 匹配（深链 ?s= 用）。
func (m *Manager) Find(ident string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[ident]; ok {
		return s
	}
	for _, id := range m.order {
		if s := m.sessions[id]; s != nil && s.Name == ident {
			return s
		}
	}
	return nil
}

// List 返回按创建顺序排列的会话状态快照。
func (m *Manager) List() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(m.order))
	for _, id := range m.order {
		if s := m.sessions[id]; s != nil {
			s.mu.Lock()
			out = append(out, s.info())
			s.mu.Unlock()
		}
	}
	return out
}

// Remove 杀掉进程并从列表删除。
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	existed := s != nil
	if s != nil {
		delete(m.sessions, id)
		for i, oid := range m.order {
			if oid == id {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
	m.mu.Unlock()
	appLog.Info("session.remove", "session", id, "existed", existed)
	if s != nil {
		s.Kill()
		if m.onChange != nil {
			m.onChange()
		}
	}
}

// Restart 重启指定会话（存活则先杀）。
func (m *Manager) Restart(id string) error {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return fmt.Errorf("会话不存在")
	}
	return s.Restart()
}

// KillAll 终止全部会话进程（优雅退出时调用）。
func (m *Manager) KillAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Kill()
	}
	appLog.Info("app.kill_all", "count", len(sessions))
}
