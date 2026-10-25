package main

// session.go — 会话核心：Pty 窄接口、Session（PTY 生命周期、广播、重放缓冲）、Manager。
// 业务代码只依赖本文件自定的 Pty 接口，不直接引用 ptygo，便于必要时换后端。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sort"
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
	// StopRead 只解除 Read 阻塞，不关闭 PTY、不终止子进程；用于热更新交接。
	StopRead() error
}

// ---------- outFrame：带序号的可续传回放缓冲 ----------

// outFrame 是一段 PTY 输出及其单调序号。客户端用 since_seq 续传时只补发缺失帧，
// 序号连续性是“字节级不丢不重”的唯一依据。
type outFrame struct {
	Seq  uint64
	Data []byte
}

const (
	// replayFrameMax 是单次 PTY Read 的上限；必须小于 replayMinBytes，
	// 否则单个读帧就会超出回放上限。当前 read 缓冲为 32KB。
	replayFrameMax = 32 * 1024
	replayMinBytes = 64 * 1024
	replayMaxBytes = 1024 * 1024
	ringFrameMax   = 2048 // 帧数上限，保证 attach 回放不会挤爆发送队列
)

// replayLimit 把 xterm scrollback（行）换算成回放字节上限：每行按 64B 估算，
// 夹在 64KB–1MB 之间。scrollback 非正时取最小值。
func replayLimit(scrollback int) int {
	if scrollback <= 0 {
		return replayMinBytes
	}
	n := scrollback * 64
	if n < replayMinBytes {
		return replayMinBytes
	}
	if n > replayMaxBytes {
		return replayMaxBytes
	}
	return n
}

// ---------- Client：WS 连接与发送队列 ----------

const (
	clientSendCap      = 4096 // 帧数上限；回放 ring ≤2048 帧，留出广播余量
	clientSendBytesMax = 8 << 20
)

var clientSeq atomic.Int64 // 连接序号，日志里用 c<seq> 标识

type Client struct {
	id        int64
	conn      *websocket.Conn
	send      chan []byte
	done      chan struct{}
	closeOne  sync.Once
	slowOne   sync.Once
	sendBytes atomic.Int64
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

// write 非阻塞：缓冲满说明客户端读得太慢。此时不断帧，而是异步断开连接，
// 让客户端重连后用 since_seq 从会话 ring 续传（会话 ring 会保留输出）。
func (c *Client) write(v []byte) {
	if v == nil {
		return
	}
	select {
	case <-c.done:
		return
	default:
	}
	n := int64(len(v))
	if c.sendBytes.Add(n) > clientSendBytesMax {
		c.sendBytes.Add(-n)
		c.markSlow()
		return
	}
	select {
	case c.send <- v:
	default:
		c.sendBytes.Add(-n)
		c.markSlow()
	}
}

// markSlow 只触发一次：关闭码 1013（Try Again Later）告诉客户端稍后重连续传。
// 对端完全不读时 close frame 可能排在大段积压数据后面，2s 后强制断开底层 TCP。
func (c *Client) markSlow() {
	c.slowOne.Do(func() {
		appLog.Warn("ws.slow_client", "conn", c.logID(), "pending", len(c.send),
			"queued_bytes", c.sendBytes.Load())
		go func() {
			closed := make(chan struct{})
			go func() {
				_ = c.conn.Close(websocket.StatusTryAgainLater, "slow client")
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				_ = c.conn.CloseNow()
			}
			c.close()
		}()
	})
}

// writeLoop 是每连接唯一的 writer：WebSocket 不允许并发 Write。
// 写失败后退出并关闭连接；正常关闭（close frame / net.ErrClosed）降为 DEBUG。
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
			c.sendBytes.Add(-int64(len(v)))
			if err != nil {
				st := websocket.CloseStatus(err)
				if pending, queued := len(c.send), c.sendBytes.Load(); pending > 0 || queued > 0 {
					// 单次 Write 15s 未完成且仍有积压：对端已停止读取。
					appLog.Warn("ws.slow_client", "conn", c.logID(), "pending", pending,
						"queued_bytes", queued, "reason", "write_stalled")
				}
				if errors.Is(err, net.ErrClosed) || st != -1 {
					appLog.Debug("ws.write_closed", "conn", c.logID(), "status", int(st), "err", err)
				} else {
					appLog.Warn("ws.write_error", "conn", c.logID(), "err", err)
				}
				_ = c.conn.CloseNow()
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *Client) close() {
	c.closeOne.Do(func() { close(c.done) })
}

// ---------- 协议消息（session 侧：output / exit） ----------

type outputMsg struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Seq  uint64 `json:"seq"`
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
	seq           uint64 // 最后一条 output 的序号；跨重启保持单调
	ring          []outFrame
	ringBytes     int
	ringDropped   bool // 任一头帧因上限被丢弃；直到会话重建
	replayLimit   int
	createdAt     int64 // Unix 毫秒，创建顺序的唯一依据
	dead          bool
	exitCode      int
	cols, rows    int
	logIO         bool // 是否记录 PTY 原始输入/输出（log_terminal_io）
	handoff       bool // true 时 Read 结束不关 PTY、不标记 dead（交给新进程）
	onStateChange func()

	spawn func(cols, rows int) (Pty, error)
}

// trimRingLocked 从头部丢整帧直到不超过回放上限；单帧自身超限时保留整帧。
func (s *Session) trimRingLocked() {
	for len(s.ring) > 1 && (s.ringBytes > s.replayLimit || len(s.ring) > ringFrameMax) {
		head := s.ring[0]
		s.ring[0] = outFrame{}
		s.ring = s.ring[1:]
		s.ringBytes -= len(head.Data)
		s.ringDropped = true
	}
	if s.ringBytes > s.replayLimit {
		appLog.Warn("session.replay_frame_too_large", "session", s.ID,
			"bytes", s.ringBytes, "limit", s.replayLimit)
	}
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
	// ConPTY 经常按行返回很小的 Read，如果一帧一个 seq，回放会变成上万帧。
	// 这里按 16KB / 6ms 聚合，既降低帧数与前端开销，又保留完整字节。
	type readResult struct {
		data []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	readStop := make(chan struct{})
	defer close(readStop)
	go func() {
		buf := make([]byte, replayFrameMax)
		for {
			n, err := p.Read(buf)
			var data []byte
			if n > 0 {
				data = append([]byte(nil), buf[:n]...)
			}
			select {
			case readCh <- readResult{data: data, err: err}:
			case <-readStop:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	const flushBytes = 16 * 1024
	const flushEvery = 6 * time.Millisecond
	pending := make([]byte, 0, flushBytes+replayFrameMax)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	flush := func() {
		if len(pending) == 0 {
			return
		}
		s.broadcastOutput(l, pending)
		pending = pending[:0]
	}

	for {
		select {
		case r := <-readCh:
			if len(r.data) > 0 {
				pending = append(pending, r.data...)
				if len(pending) >= flushBytes {
					flush()
				}
			}
			if r.err != nil {
				flush()
				appLog.Debug("pty.read_end", "session", s.ID, "pid", p.Pid(), "err", r.err)
				code = p.ExitCode()
				return
			}
		case <-ticker.C:
			flush()
		}
	}
}

// finishRun 收尾一代 PTY：关闭句柄、标记 dead、广播 exit。正常退出与 panic 共用。
// 热更新交接期间 Read 被主动停止：保留 PTY 句柄供新进程接管，不标记 dead。
func (s *Session) finishRun(l *livePTY, code int) {
	p := l.pty

	s.mu.Lock()
	handoff := s.live == l && s.handoff
	s.mu.Unlock()
	if handoff {
		appLog.Info("session.handoff_pause", "session", s.ID, "pid", p.Pid())
		return
	}

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
	b64 := base64.StdEncoding.EncodeToString(data)

	s.mu.Lock()
	// 仅当前代写入 ring 与广播：重启换代后，旧循环的残余输出不能污染新 seq。
	var frame []byte
	if s.live == l {
		s.seq++
		f := outFrame{Seq: s.seq, Data: append([]byte(nil), data...)}
		s.ring = append(s.ring, f)
		s.ringBytes += len(f.Data)
		s.trimRingLocked()
		frame = mustJSON(outputMsg{Type: "output", ID: s.ID, Seq: f.Seq, Data: b64})
	}
	for c := range s.clients {
		c.write(frame)
	}
	logIO := s.logIO
	s.mu.Unlock()

	if logIO && frame != nil {
		appLog.Info("pty.output", "session", s.ID, "bytes", len(data), "data_b64", b64)
	}
}

var errSessionDead = fmt.Errorf("会话已退出")

// Attach 先发 attached（带 seq/resumed/truncated），再发回放帧，最后登记客户端。
// sinceSeq 非空且对应帧仍在 ring 内时只补发缺失帧；否则全量重放。
// 已死会话拒绝 attach。
func (s *Session) Attach(c *Client, sinceSeq *uint64) error {
	s.mu.Lock()
	if s.dead || s.live == nil || s.handoff {
		s.mu.Unlock()
		appLog.Warn("session.attach_rejected", "session", s.ID, "conn", c.logID(), "err", errSessionDead)
		return errSessionDead
	}

	cur := s.seq
	resumed := false
	truncated := false
	fromSeq := cur + 1
	var replay []outFrame
	if n := len(s.ring); n > 0 {
		first := s.ring[0].Seq
		if sinceSeq != nil && *sinceSeq >= first-1 {
			// 客户端要的下一条仍在 ring 内：只发缺失部分。
			resumed = true
			for _, f := range s.ring {
				if f.Seq > *sinceSeq {
					if len(replay) == 0 {
						fromSeq = f.Seq
					}
					replay = append(replay, f)
				}
			}
		} else {
			fromSeq = first
			truncated = s.ringDropped
			replay = append(replay, s.ring...)
		}
	} else {
		// 空 ring：服务端没有可补的历史；序号相等即视为已同步。
		resumed = sinceSeq != nil && *sinceSeq >= cur
	}

	replayBytes := 0
	for _, f := range replay {
		replayBytes += len(f.Data)
	}
	// 早于所有回放帧发送 attached，客户端据此决定是否清屏 / 丢弃 early 缓冲。
	c.write(mustJSON(attachedMsg{
		Type:      "attached",
		ID:        s.ID,
		Seq:       cur,
		Resumed:   resumed,
		Truncated: truncated,
		FromSeq:   fromSeq,
	}))
	for _, f := range replay {
		c.write(mustJSON(outputMsg{
			Type: "output",
			ID:   s.ID,
			Seq:  f.Seq,
			Data: base64.StdEncoding.EncodeToString(f.Data),
		}))
	}
	if truncated {
		c.write(mustJSON(noticeMsg{Type: "notice", Level: "warn", Message: "历史已截断"}))
	}
	s.clients[c] = struct{}{}
	count := len(s.clients)
	s.mu.Unlock()

	var since any
	if sinceSeq != nil {
		since = *sinceSeq
	}
	appLog.Info("session.attach", "session", s.ID, "conn", c.logID(),
		"replay_bytes", replayBytes, "clients", count,
		"since_seq", since, "resumed", resumed, "truncated", truncated,
		"seq", cur, "from_seq", fromSeq)
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

// Size 返回当前记录的终端尺寸（无存活进程时也是最后尺寸）。
func (s *Session) Size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
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
	// 新进程清空回放缓冲，但 seq 继续递增：已连接的客户端不会看到序号回退，
	// 新客户端首帧无需历史重放。
	s.ring = nil
	s.ringBytes = 0
	s.ringDropped = false
	s.mu.Unlock()

	go s.run(l)
	appLog.Info("session.restart", "session", s.ID, "old_pid", oldPid, "new_pid", p.Pid())
	if s.onStateChange != nil {
		s.onStateChange()
	}
	return nil
}

// suspendForHandoff 停止读取但保留 PTY 与 shell 进程，等待新进程接管。
// 必须在 Manager.BeginHandoff 之后调用，避免与 Restart/Remove 竞争。
func (s *Session) suspendForHandoff() {
	s.mu.Lock()
	l := s.live
	if l == nil {
		s.mu.Unlock()
		return
	}
	s.handoff = true
	s.mu.Unlock()

	if err := l.pty.StopRead(); err != nil {
		appLog.Warn("session.handoff_stop_read", "session", s.ID, "pid", l.pty.Pid(), "err", err)
	}
	select {
	case <-l.done:
	case <-time.After(3 * time.Second):
		appLog.Warn("session.handoff_stop_timeout", "session", s.ID, "pid", l.pty.Pid())
	}
}

// startRestoredRead 在恢复 PTY 后启动读循环（仅由恢复流程调用一次）。
func (s *Session) startRestoredRead() {
	s.mu.Lock()
	l := s.live
	s.mu.Unlock()
	if l != nil {
		go s.run(l)
	}
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
		ID:        s.ID,
		Name:      s.Name,
		Dead:      s.dead,
		ExitCode:  s.exitCode,
		Attached:  len(s.clients),
		CreatedAt: s.createdAt,
	}
	if s.live != nil {
		info.PID = s.live.pty.Pid()
	}
	return info
}

// ---------- Manager ----------

type SessionInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PID       int    `json:"pid"`
	Dead      bool   `json:"dead"`
	ExitCode  int    `json:"exitCode"`
	Attached  int    `json:"attached"`
	CreatedAt int64  `json:"createdAt"`
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	order    []string // 创建顺序，稳定展示
	seq      int
	cfg      Config
	onChange func() // server 回调：广播 sessions 列表
	handoff  bool   // 热更新交接期间冻结会话增删/重启
}

func NewManager(cfg Config, onChange func()) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		cfg:      cfg,
		onChange: onChange,
	}
}

// BeginHandoff 冻结会话增删/重启；返回 false 表示已有交接在进行。
func (m *Manager) BeginHandoff() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handoff {
		return false
	}
	m.handoff = true
	return true
}

// EndHandoff 解除冻结（交接失败时回退）。
func (m *Manager) EndHandoff() {
	m.mu.Lock()
	m.handoff = false
	m.mu.Unlock()
}

// HandoffActive 返回是否正在做热更新交接。
func (m *Manager) HandoffActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.handoff
}

// sessionsSnapshot 按创建顺序返回会话指针副本。
func (m *Manager) sessionsSnapshot() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.order))
	for _, id := range m.order {
		if s := m.sessions[id]; s != nil {
			out = append(out, s)
		}
	}
	return out
}

// newSession 构造会话对象（不启动 PTY）。id/name 的语义与 Create 一致。
func (m *Manager) newSession(id, name string, cols, rows int) *Session {
	return &Session{
		ID:            id,
		Name:          name,
		clients:       make(map[*Client]struct{}),
		replayLimit:   replayLimit(m.cfg.Scrollback),
		createdAt:     time.Now().UnixMilli(),
		cols:          cols,
		rows:          rows,
		logIO:         m.cfg.LogTerminalIO,
		onStateChange: m.onChange,
		spawn: func(cols, rows int) (Pty, error) {
			return spawnShell(m.cfg, id, cols, rows)
		},
	}
}

// normalizeSize 把客户端未提供/非法的尺寸回落到 80×24。
func normalizeSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return cols, rows
}

// Create 新建会话。name 为空 → "sess-<seq>"。cols/rows 非 0 时作为 PTY 初始尺寸。
func (m *Manager) Create(name string, cols, rows int) (*Session, error) {
	cols, rows = normalizeSize(cols, rows)
	m.mu.Lock()
	if m.handoff {
		m.mu.Unlock()
		return nil, fmt.Errorf("正在更新，请稍后再试")
	}
	id := name
	if id == "" {
		m.seq++
		id = fmt.Sprintf("sess-%d", m.seq)
	}
	if _, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("会话 %q 已存在", id)
	}
	s := m.newSession(id, name, cols, rows)
	m.sessions[id] = s
	m.order = append(m.order, id)
	m.mu.Unlock()

	p, err := s.spawn(cols, rows)
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

	appLog.Info("session.create", "session", id, "name", name, "pid", p.Pid(),
		"cols", cols, "rows", rows)
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

// List 返回按创建时间升序（同值按 ID）排列的会话状态快照。
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
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Remove 杀掉进程并从列表删除。
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	if m.handoff {
		m.mu.Unlock()
		appLog.Warn("session.remove_blocked", "session", id, "reason", "handoff")
		return
	}
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
	if m.handoff {
		m.mu.Unlock()
		return fmt.Errorf("正在更新，请稍后再试")
	}
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
