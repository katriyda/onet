package main

// session_replay_test.go — 可续传回放的序号语义、会话排序与启动安全校验。

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

type testFrame struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Seq       uint64 `json:"seq"`
	Resumed   bool   `json:"resumed"`
	Truncated bool   `json:"truncated"`
	Capped    bool   `json:"capped"`
	FromSeq   uint64 `json:"from_seq"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

func newTestClient() *Client {
	return &Client{id: 1, send: make(chan []byte, clientSendCap), done: make(chan struct{})}
}

func drainFrames(t *testing.T, c *Client) []testFrame {
	t.Helper()
	var out []testFrame
	for {
		select {
		case raw := <-c.send:
			var f testFrame
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("解析帧失败: %v (%s)", err, raw)
			}
			out = append(out, f)
		default:
			return out
		}
	}
}

func newReplaySession(limit int) (*Session, *livePTY) {
	s := &Session{
		ID:          "s1",
		clients:     make(map[*Client]struct{}),
		replayLimit: limit,
		createdAt:   1,
	}
	l := &livePTY{pty: nil, done: make(chan struct{})}
	s.live = l
	return s, l
}

func TestReplaySinceSeq(t *testing.T) {
	s, l := newReplaySession(1024 * 1024)
	for i := range 10 {
		s.broadcastOutput(l, []byte{byte('a' + i)})
	}
	c := newTestClient()
	since := uint64(4)
	if err := s.Attach(c, &since, 0); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	frames := drainFrames(t, c)
	if len(frames) != 7 {
		t.Fatalf("帧数 = %d, want 7 (attached + 6 output)", len(frames))
	}
	att := frames[0]
	if att.Type != "attached" || !att.Resumed || att.Truncated || att.Seq != 10 || att.FromSeq != 5 {
		t.Fatalf("attached = %+v, want resumed=true truncated=false seq=10 from=5", att)
	}
	for i, f := range frames[1:] {
		if f.Type != "output" || f.Seq != uint64(5+i) {
			t.Fatalf("output[%d] = %+v, want seq=%d", i, f, 5+i)
		}
	}
}

func TestReplayTruncated(t *testing.T) {
	s, l := newReplaySession(replayMinBytes)
	chunk := make([]byte, 8*1024)
	for range 20 {
		s.broadcastOutput(l, chunk)
	}
	if !s.ringDropped {
		t.Fatal("超过上限后 ringDropped 应为 true")
	}
	firstKept := s.ring[0].Seq
	c := newTestClient()
	since := uint64(1)
	if err := s.Attach(c, &since, 0); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	frames := drainFrames(t, c)
	if len(frames) == 0 {
		t.Fatal("没有收到帧")
	}
	att := frames[0]
	if att.Type != "attached" || att.Resumed || !att.Truncated || att.Capped || att.FromSeq != firstKept {
		t.Fatalf("attached = %+v, want truncated=true from=%d", att, firstKept)
	}
	var notice *testFrame
	var firstOutput uint64
	for i := 1; i < len(frames); i++ {
		f := frames[i]
		if f.Type == "notice" {
			notice = &frames[i]
			continue
		}
		if f.Type == "output" && firstOutput == 0 {
			firstOutput = f.Seq
		}
	}
	if notice == nil || notice.Level != "warn" {
		t.Fatalf("缺少 warning notice: %+v", frames)
	}
	if firstOutput != firstKept {
		t.Fatalf("首帧 seq = %d, want %d", firstOutput, firstKept)
	}
}

func TestReplayMaxBytesKeepsTail(t *testing.T) {
	s, l := newReplaySession(1024 * 1024)
	for i := range 10 {
		s.broadcastOutput(l, bytes.Repeat([]byte{byte('a' + i)}, 1024))
	}
	c := newTestClient()
	if err := s.Attach(c, nil, 3*1024); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	frames := drainFrames(t, c)
	att := frames[0]
	if att.Type != "attached" || att.Resumed || !att.Truncated || !att.Capped || att.Seq != 10 || att.FromSeq != 8 {
		t.Fatalf("attached = %+v, want capped+truncated seq=10 from=8", att)
	}
	var outputs []uint64
	for _, f := range frames[1:] {
		if f.Type == "notice" {
			t.Fatalf("capped replay 不应再发断线 notice: %+v", f)
		}
		if f.Type == "output" {
			outputs = append(outputs, f.Seq)
		}
	}
	if len(outputs) != 3 || outputs[0] != 8 || outputs[2] != 10 {
		t.Fatalf("output seqs = %v, want [8 9 10]", outputs)
	}
}

func TestReplayMaxBytesKeepsOneOversizeFrame(t *testing.T) {
	s, l := newReplaySession(1024 * 1024)
	s.broadcastOutput(l, []byte("small"))
	s.broadcastOutput(l, bytes.Repeat([]byte("x"), 8*1024))
	c := newTestClient()
	if err := s.Attach(c, nil, 1024); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	frames := drainFrames(t, c)
	att := frames[0]
	if !att.Truncated || !att.Capped || att.FromSeq != 2 || att.Seq != 2 {
		t.Fatalf("attached = %+v, want single oversize frame seq=2", att)
	}
}

func TestSessionListStableOrder(t *testing.T) {
	m := &Manager{sessions: make(map[string]*Session), cfg: DefaultConfig()}
	mk := func(id string, createdAt int64) {
		s := m.newSession(id, "", 80, 24)
		s.createdAt = createdAt
		m.sessions[id] = s
		m.order = append(m.order, id)
	}
	mk("b", 3000)
	mk("a", 1000)
	mk("c", 2000)

	for i := 0; i < 20; i++ {
		list := m.List()
		if len(list) != 3 {
			t.Fatalf("会话数 = %d", len(list))
		}
		want := []string{"a", "c", "b"}
		for j, id := range want {
			if list[j].ID != id {
				t.Fatalf("第 %d 次 List = %v, want %v", i, list, want)
			}
			if j > 0 && list[j-1].CreatedAt > list[j].CreatedAt {
				t.Fatalf("createdAt 未升序: %v", list)
			}
		}
	}
}

func TestNonLoopbackRequiresToken(t *testing.T) {
	if err := validateConfig(Config{Host: "127.0.0.1"}); err != nil {
		t.Fatalf("回环空 token 应允许启动: %v", err)
	}
	if err := validateConfig(Config{Host: "0.0.0.0"}); err == nil {
		t.Fatal("非回环空 token 且 allow_insecure=false 应拒绝")
	}
	if err := validateConfig(Config{Host: "0.0.0.0", AllowInsecure: true}); err != nil {
		t.Fatalf("allow_insecure=true 应允许: %v", err)
	}
	if err := validateConfig(Config{Host: "0.0.0.0", Token: "x"}); err != nil {
		t.Fatalf("有 token 应允许: %v", err)
	}
}

func TestPingCommand(t *testing.T) {
	srv := NewServer(DefaultConfig())
	c := newTestClient()
	var attached *Session
	resp := srv.handleCommand(c, &attached, []byte(`{"type":"ping"}`))
	var f struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(resp, &f); err != nil {
		t.Fatalf("解析 pong: %v", err)
	}
	if f.Type != "pong" {
		t.Fatalf("ping 响应 = %q, want pong", f.Type)
	}
}

func TestHealthzLoopbackOnly(t *testing.T) {
	srv := NewServer(DefaultConfig())
	cases := []struct {
		remote string
		want   int
	}{
		{"127.0.0.1:1234", 200},
		{"[::1]:1234", 200},
		{"10.0.0.1:1234", 404},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/healthz", nil)
		req.RemoteAddr = c.remote
		rec := httptest.NewRecorder()
		srv.handleHealthz(rec, req)
		if rec.Code != c.want {
			t.Errorf("RemoteAddr=%s 状态 = %d, want %d", c.remote, rec.Code, c.want)
		}
	}
}
