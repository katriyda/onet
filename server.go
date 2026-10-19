package main

// server.go — HTTP 路由、token 认证、WebSocket 协议处理。

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

//go:embed all:web
var webFS embed.FS

type Server struct {
	cfg     Config
	mgr     *Manager
	mu      sync.Mutex
	clients map[*Client]struct{}
}

func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg, clients: make(map[*Client]struct{})}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/static/", s.handleStatic)
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logs", s.handleLogs)
	mux.HandleFunc("/ws", s.handleWS)
	return s.withAccessLog(mux)
}

// ---------- 访问日志 ----------

// statusRecorder 记录状态码与响应字节数；Unwrap 让 websocket 找到 Hijacker。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status != 0 {
		return
	}
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// withAccessLog 为每个请求记录一条 http.request（panic 记为 http.panic）。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				appLog.Error("http.panic",
					"method", r.Method,
					"path", r.URL.Path,
					"query", redactedQuery(r.URL.RawQuery),
					"remote", r.RemoteAddr,
					"ua", r.UserAgent(),
					"panic", fmt.Sprint(p),
					"stack", string(debug.Stack()),
				)
				if rec.status == 0 {
					http.Error(rec, "internal server error", http.StatusInternalServerError)
				}
				return
			}
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			lvl := slog.LevelInfo
			switch {
			case status >= 500:
				lvl = slog.LevelError
			case status >= 400:
				lvl = slog.LevelWarn
			case strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/ws":
				lvl = slog.LevelDebug
			}
			appLog.Log(context.Background(), lvl, "http.request",
				"method", r.Method,
				"path", r.URL.Path,
				"query", redactedQuery(r.URL.RawQuery),
				"status", status,
				"bytes", rec.bytes,
				"dur_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
				"ua", r.UserAgent(),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

// ---------- 认证 ----------

// authorized 恒时比较。cookie / ?token= / Authorization: Bearer 三选一通过即可。
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	if c, err := r.Cookie("onet_token"); err == nil && tokenOK(c.Value, s.cfg.Token) {
		return true
	}
	if t := r.URL.Query().Get("token"); t != "" && tokenOK(t, s.cfg.Token) {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if tokenOK(strings.TrimPrefix(h, "Bearer "), s.cfg.Token) {
			return true
		}
	}
	// 只记录请求元数据：token / cookie / Authorization 一律不落盘。
	appLog.Warn("auth.fail", "path", r.URL.Path, "remote", r.RemoteAddr, "ua", r.UserAgent())
	return false
}

func tokenOK(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ---------- HTTP 处理 ----------

// handleIndex 始终返回前端页面（token 登录页内嵌其中；数据接口另行认证）。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "index.html 缺失", 500)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// handleStatic 提供前端静态资源（无敏感数据，公开访问以便登录页渲染）。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// handleConfig 返回当前配置（token 不回传）。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.cfg
	cfg.Token = ""
	writeJSON(w, cfg)
}

type loginReq struct {
	Token string `json:"token"`
}

// handleLogin 校验 token 并种下 onet_token cookie。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.Token == "" {
		appLog.Warn("auth.login", "ok", false, "reason", "token_disabled", "remote", r.RemoteAddr)
		http.Error(w, "未启用 token 认证", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
	if err != nil {
		appLog.Warn("auth.login", "ok", false, "reason", "bad_request", "remote", r.RemoteAddr)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req loginReq
	if err := json.Unmarshal(body, &req); err != nil {
		appLog.Warn("auth.login", "ok", false, "reason", "bad_request", "remote", r.RemoteAddr)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !tokenOK(req.Token, s.cfg.Token) {
		appLog.Warn("auth.login", "ok", false, "reason", "bad_token", "remote", r.RemoteAddr)
		http.Error(w, "token 错误", http.StatusUnauthorized)
		return
	}
	appLog.Info("auth.login", "ok", true, "remote", r.RemoteAddr)
	http.SetCookie(w, &http.Cookie{
		Name:     "onet_token",
		Value:    s.cfg.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   365 * 24 * 3600,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 日志查看 / 下载 ----------

// handleLogs 提供日志读取（?tail=N）与合并下载（?download=1）。需认证。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path, _ := activeLogPath.Load().(string)
	if path == "" {
		http.Error(w, "日志文件未启用", http.StatusNotFound)
		return
	}

	if r.URL.Query().Get("download") == "1" {
		if !logFilesExist(path, s.cfg.LogMaxBackups) {
			http.Error(w, "日志文件不存在", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition",
			`attachment; filename="onet-logs-`+time.Now().Format("20060102-150405")+`.jsonl"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		n, _, err := streamLogFiles(w, path, s.cfg.LogMaxBackups)
		if err != nil {
			appLog.Warn("logs.download_part_failed", "path", path, "err", err)
		}
		appLog.Info("logs.download", "path", path, "bytes", n)
		return
	}

	tail := 1000
	if v := r.URL.Query().Get("tail"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			tail = parsed
		}
	}
	if tail < 1 {
		tail = 1
	} else if tail > 5000 {
		tail = 5000
	}
	data, err := tailFile(path, tail, 2<<20)
	if err != nil {
		appLog.Warn("logs.read_failed", "path", path, "err", err)
		http.Error(w, "读取日志失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
	appLog.Debug("logs.read", "tail", tail, "bytes", len(data))
}

// ---------- WebSocket ----------

// handleWS 处理协议连接。二进制帧 = JSON 命令；文本帧 = 原始输入字节。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // 任意 Origin 可连（局域网移动端场景）
	})
	if err != nil {
		appLog.Warn("ws.accept_failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	c.SetReadLimit(1 << 20) // 大段粘贴输入

	client := newClient(c)
	go client.writeLoop()

	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()

	start := time.Now()
	appLog.Info("ws.open", "conn", client.logID(), "remote", r.RemoteAddr, "ua", r.UserAgent())
	// 先告知客户端连接标识，浏览器日志据此关联到同一连接。
	client.write(mustJSON(helloMsg{Type: "hello", Conn: client.logID()}))

	var attached *Session
	var closeErr error
	defer func() {
		client.close()
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
		// 断连 → detach，会话存活（tmux detach 语义）
		sessID := ""
		if attached != nil {
			sessID = attached.ID
			attached.Detach(client)
		}
		attrs := []any{
			"conn", client.logID(),
			"session", sessID,
			"dur_ms", time.Since(start).Milliseconds(),
			"status", int(websocket.CloseStatus(closeErr)),
		}
		// 正常关闭的 "received close frame" 不算错误，避免噪音。
		if st := websocket.CloseStatus(closeErr); closeErr != nil &&
			st != websocket.StatusNormalClosure && st != websocket.StatusGoingAway {
			attrs = append(attrs, "err", closeErr)
		}
		appLog.Info("ws.close", attrs...)
	}()

	for {
		typ, data, err := c.Read(r.Context())
		if err != nil {
			closeErr = err
			return
		}
		switch typ {
		case websocket.MessageText:
			// 文本帧 = 原始输入，转发给当前 attach 的会话
			if attached != nil {
				if s.cfg.LogTerminalIO {
					appLog.Info("pty.input", "session", attached.ID, "conn", client.logID(),
						"bytes", len(data), "data_b64", base64.StdEncoding.EncodeToString(data))
				}
				attached.Input(data)
			}
		case websocket.MessageBinary:
			if resp := s.handleCommand(client, &attached, data); resp != nil {
				client.write(resp)
			}
		}
	}
}

// cmdErr 统一记录命令错误并返回 error 帧（错误文案保持原样）。
func cmdErr(client *Client, cmd, msg string) []byte {
	appLog.Warn("ws.command_error", "conn", client.logID(), "cmd", cmd, "err", msg)
	return mustJSON(errorMsg{Type: "error", Message: msg})
}

// handleCommand 处理一条二进制 JSON 命令，返回需要回复的帧（可为 nil）。
func (s *Server) handleCommand(client *Client, attached **Session, data []byte) []byte {
	var cmd struct {
		Type    string           `json:"type"`
		Name    string           `json:"name"`
		ID      string           `json:"id"`
		Cols    int              `json:"cols"`
		Rows    int              `json:"rows"`
		Client  string           `json:"client"`
		Entries []clientLogEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &cmd); err != nil {
		appLog.Warn("ws.bad_command", "conn", client.logID(), "bytes", len(data), "err", err)
		return mustJSON(errorMsg{Type: "error", Message: "无法解析命令"})
	}

	// 浏览器日志回传：不记录 ws.command，避免每批两条日志。
	if cmd.Type == "log" {
		handleClientLogs(client, cmd.Client, cmd.Entries)
		return nil
	}

	// list / resize 高频且信息量低 → debug。
	lvl := slog.LevelInfo
	if cmd.Type == "list" || cmd.Type == "resize" {
		lvl = slog.LevelDebug
	}
	appLog.Log(context.Background(), lvl, "ws.command",
		"conn", client.logID(), "cmd", cmd.Type,
		"session", cmd.ID, "name", cmd.Name, "cols", cmd.Cols, "rows", cmd.Rows)

	switch cmd.Type {
	case "list":
		return mustJSON(sessionsMsg{Type: "sessions", Sessions: s.mgr.List()})

	case "create":
		sess, err := s.mgr.Create(cmd.Name)
		if err != nil {
			return cmdErr(client, cmd.Type, err.Error())
		}
		return mustJSON(createdMsg{Type: "created", ID: sess.ID, Name: sess.Name})

	case "attach":
		sess := s.mgr.Get(cmd.ID)
		if sess == nil {
			return cmdErr(client, cmd.Type, "会话不存在")
		}
		if sess.Dead() {
			return cmdErr(client, cmd.Type, "会话已退出")
		}
		// 同一连接同一时刻只 attach 一个会话：切换 = 先 detach 再 attach
		if *attached != nil && *attached != sess {
			(*attached).Detach(client)
		}
		if err := sess.Attach(client); err != nil {
			return cmdErr(client, cmd.Type, "会话已退出")
		}
		*attached = sess
		return mustJSON(attachedMsg{Type: "attached", ID: sess.ID})

	case "detach":
		if *attached != nil {
			(*attached).Detach(client)
			*attached = nil
		}
		return mustJSON(detachedMsg{Type: "detached"})

	case "resize":
		sess := s.mgr.Get(cmd.ID)
		if sess == nil || cmd.Cols <= 0 || cmd.Rows <= 0 {
			appLog.Debug("ws.resize_ignored", "conn", client.logID(), "session", cmd.ID,
				"cols", cmd.Cols, "rows", cmd.Rows)
			return nil
		}
		sess.Resize(cmd.Cols, cmd.Rows)
		return nil

	case "restart":
		if err := s.mgr.Restart(cmd.ID); err != nil {
			return cmdErr(client, cmd.Type, err.Error())
		}
		return nil

	case "kill":
		if sess := s.mgr.Get(cmd.ID); sess != nil {
			sess.Kill()
		} else {
			appLog.Debug("ws.session_missing", "conn", client.logID(), "cmd", cmd.Type, "session", cmd.ID)
		}
		return nil

	case "remove":
		if s.mgr.Get(cmd.ID) == nil {
			appLog.Debug("ws.session_missing", "conn", client.logID(), "cmd", cmd.Type, "session", cmd.ID)
		}
		s.mgr.Remove(cmd.ID)
		return nil

	default:
		return cmdErr(client, cmd.Type, fmt.Sprintf("未知命令 %q", cmd.Type))
	}
}

// broadcastSessions 向所有连接广播会话列表（Manager 增删改回调）。
func (s *Server) broadcastSessions() {
	list := s.mgr.List()
	frame := mustJSON(sessionsMsg{Type: "sessions", Sessions: list})
	s.mu.Lock()
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.write(frame)
	}
	appLog.Debug("ws.broadcast_sessions", "clients", len(clients), "sessions", len(list))
}

// ---------- 协议消息（server 侧） ----------

type sessionsMsg struct {
	Type     string        `json:"type"`
	Sessions []SessionInfo `json:"sessions"`
}

type createdMsg struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type attachedMsg struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type detachedMsg struct {
	Type string `json:"type"`
}

// helloMsg 连接建立后的第一帧：告知客户端日志标识。
type helloMsg struct {
	Type string `json:"type"` // "hello"
	Conn string `json:"conn"` // "c1"
}

type errorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		appLog.Error("json.marshal_failed", "err", err)
		return []byte(`{"type":"error","message":"内部错误"}`)
	}
	return b
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
