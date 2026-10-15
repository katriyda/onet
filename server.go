package main

// server.go — HTTP 路由、token 认证、WebSocket 协议处理。

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
	mux.HandleFunc("/ws", s.handleWS)
	return mux
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
		return tokenOK(strings.TrimPrefix(h, "Bearer "), s.cfg.Token)
	}
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
		http.Error(w, "未启用 token 认证", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req loginReq
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !tokenOK(req.Token, s.cfg.Token) {
		http.Error(w, "token 错误", http.StatusUnauthorized)
		return
	}
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
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	c.SetReadLimit(1 << 20) // 大段粘贴输入

	client := newClient(c)
	go client.writeLoop()

	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()

	var attached *Session
	defer func() {
		client.close()
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
		// 断连 → detach，会话存活（tmux detach 语义）
		if attached != nil {
			attached.Detach(client)
		}
	}()

	for {
		typ, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageText:
			// 文本帧 = 原始输入，转发给当前 attach 的会话
			if attached != nil {
				attached.Input(data)
			}
		case websocket.MessageBinary:
			if resp := s.handleCommand(client, &attached, data); resp != nil {
				client.write(resp)
			}
		}
	}
}

// handleCommand 处理一条二进制 JSON 命令，返回需要回复的帧（可为 nil）。
func (s *Server) handleCommand(client *Client, attached **Session, data []byte) []byte {
	var cmd struct {
		Type string `json:"type"`
		Name string `json:"name"`
		ID   string `json:"id"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if err := json.Unmarshal(data, &cmd); err != nil {
		return mustJSON(errorMsg{Type: "error", Message: "无法解析命令"})
	}

	switch cmd.Type {
	case "list":
		return mustJSON(sessionsMsg{Type: "sessions", Sessions: s.mgr.List()})

	case "create":
		sess, err := s.mgr.Create(cmd.Name)
		if err != nil {
			return mustJSON(errorMsg{Type: "error", Message: err.Error()})
		}
		return mustJSON(createdMsg{Type: "created", ID: sess.ID, Name: sess.Name})

	case "attach":
		sess := s.mgr.Get(cmd.ID)
		if sess == nil {
			return mustJSON(errorMsg{Type: "error", Message: "会话不存在"})
		}
		if sess.Dead() {
			return mustJSON(errorMsg{Type: "error", Message: "会话已退出"})
		}
		// 同一连接同一时刻只 attach 一个会话：切换 = 先 detach 再 attach
		if *attached != nil && *attached != sess {
			(*attached).Detach(client)
		}
		if err := sess.Attach(client); err != nil {
			return mustJSON(errorMsg{Type: "error", Message: "会话已退出"})
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
		if sess := s.mgr.Get(cmd.ID); sess != nil && cmd.Cols > 0 && cmd.Rows > 0 {
			sess.Resize(cmd.Cols, cmd.Rows)
		}
		return nil

	case "restart":
		if err := s.mgr.Restart(cmd.ID); err != nil {
			return mustJSON(errorMsg{Type: "error", Message: err.Error()})
		}
		return nil

	case "kill":
		if sess := s.mgr.Get(cmd.ID); sess != nil {
			sess.Kill()
		}
		return nil

	case "remove":
		s.mgr.Remove(cmd.ID)
		return nil

	default:
		return mustJSON(errorMsg{Type: "error", Message: fmt.Sprintf("未知命令 %q", cmd.Type)})
	}
}

// broadcastSessions 向所有连接广播会话列表（Manager 增删改回调）。
func (s *Server) broadcastSessions() {
	frame := mustJSON(sessionsMsg{Type: "sessions", Sessions: s.mgr.List()})
	s.mu.Lock()
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.write(frame)
	}
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

type errorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("序列化失败: %v", err)
		return []byte(`{"type":"error","message":"内部错误"}`)
	}
	return b
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
