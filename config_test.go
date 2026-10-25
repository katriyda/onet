package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newConfigTestServer(t *testing.T) (*Server, string, Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	srv := NewServer(cfg)
	srv.configPath = path
	srv.mgr = NewManager(cfg, func() {})
	return srv, path, cfg
}

func configRequest(t *testing.T, srv *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/config", nil)
	} else {
		req = httptest.NewRequest(method, "/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Onet-Config", "1")
	}
	if srv.cfg.Token != "" {
		req.AddCookie(&http.Cookie{Name: "onet_token", Value: srv.cfg.Token})
	}
	rec := httptest.NewRecorder()
	srv.handleConfig(rec, req)
	return rec
}

func fullConfigPayload(cfg Config, overrides map[string]any) map[string]any {
	p := map[string]any{
		"host":            cfg.Host,
		"port":            cfg.Port,
		"token":           "",
		"clear_token":     false,
		"shell":           cfg.Shell,
		"cwd":             cfg.Cwd,
		"scrollback":      cfg.Scrollback,
		"font_size":       cfg.FontSize,
		"ui":              cfg.UI,
		"extra_keys":      cfg.ExtraKeys,
		"log_file":        cfg.LogFile,
		"log_level":       cfg.LogLevel,
		"log_max_size_mb": cfg.LogMaxSizeMB,
		"log_max_backups": cfg.LogMaxBackups,
		"log_terminal_io": cfg.LogTerminalIO,
		"allow_insecure":  cfg.AllowInsecure,
		"restart":         false,
	}
	for k, v := range overrides {
		p[k] = v
	}
	return p
}

func mustPayloadJSON(t *testing.T, p map[string]any) string {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLoadGeneratesSecureConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.generated {
		t.Fatal("首次加载应标记 generated")
	}
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("host = %q, want 127.0.0.1", cfg.Host)
	}
	if cfg.AllowInsecure {
		t.Fatal("默认 allow_insecure 应为 false")
	}
	if len(cfg.Token) < 40 {
		t.Fatalf("token 太短: %q", cfg.Token)
	}

	raw := readConfigMap(t, path)
	if raw["token"] != cfg.Token {
		t.Fatalf("落盘 token 与内存不一致")
	}
	if raw["host"] != "127.0.0.1" {
		t.Fatalf("落盘 host = %v", raw["host"])
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("config 权限 = %o, want 600", fi.Mode().Perm())
		}
	}

	again, err := Load(path)
	if err != nil {
		t.Fatalf("再次 Load: %v", err)
	}
	if again.Token != cfg.Token || again.generated {
		t.Fatal("已有配置不应重新生成 token")
	}
}

func TestLoadRandomizesTokens(t *testing.T) {
	first, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token {
		t.Fatal("两个新配置生成了相同 token")
	}
}

func TestSaveWritesRelativeLogPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := DefaultConfig()
	cfg.Token = "test-token"
	cfg.LogFile = filepath.Join(dir, "logs", "onet.log")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw := readConfigMap(t, path)
	if raw["log_file"] != "logs/onet.log" {
		t.Fatalf("log_file = %v, want logs/onet.log", raw["log_file"])
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LogFile != cfg.LogFile {
		t.Fatalf("Load(LogFile) = %q, want %q", loaded.LogFile, cfg.LogFile)
	}
}

func TestConfigViewHidesToken(t *testing.T) {
	srv, _, cfg := newConfigTestServer(t)
	rec := configRequest(t, srv, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), cfg.Token) {
		t.Fatal("GET /config 泄露了 token")
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["token"] != "" {
		t.Fatalf("token 字段 = %v, want 空串", got["token"])
	}
	if got["token_set"] != true {
		t.Fatalf("token_set = %v, want true", got["token_set"])
	}
}

func TestConfigUpdatePersistsAndKeepsToken(t *testing.T) {
	srv, path, cfg := newConfigTestServer(t)
	payload := fullConfigPayload(cfg, map[string]any{"port": 7777, "font_size": 18})
	rec := configRequest(t, srv, http.MethodPut, mustPayloadJSON(t, payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 7777 || got.FontSize != 18 {
		t.Fatalf("落盘配置 = port %d font %d", got.Port, got.FontSize)
	}
	if got.Token != cfg.Token {
		t.Fatal("空 token 应保持原 token")
	}
}

func TestConfigUpdateRejectsInsecureClearToken(t *testing.T) {
	srv, path, cfg := newConfigTestServer(t)
	before := readConfigMap(t, path)
	payload := fullConfigPayload(cfg, map[string]any{
		"host":        "0.0.0.0",
		"clear_token": true,
	})
	rec := configRequest(t, srv, http.MethodPut, mustPayloadJSON(t, payload))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	after := readConfigMap(t, path)
	if before["port"] != after["port"] || before["token"] != after["token"] {
		t.Fatal("非法配置不应写入文件")
	}
}

func TestConfigUpdateClearTokenLoopback(t *testing.T) {
	srv, path, cfg := newConfigTestServer(t)
	payload := fullConfigPayload(cfg, map[string]any{"clear_token": true})
	rec := configRequest(t, srv, http.MethodPut, mustPayloadJSON(t, payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "" {
		t.Fatalf("token = %q, want 空", got.Token)
	}
}

func TestConfigUpdateRestartConflict(t *testing.T) {
	srv, path, cfg := newConfigTestServer(t)
	before := readConfigMap(t, path)
	srv.updating = true
	payload := fullConfigPayload(cfg, map[string]any{"port": 7778, "restart": true})
	rec := configRequest(t, srv, http.MethodPut, mustPayloadJSON(t, payload))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	after := readConfigMap(t, path)
	if before["port"] != after["port"] {
		t.Fatal("冲突时不应写入配置")
	}
}

func TestConfigUpdateRequiresCSRFHeader(t *testing.T) {
	srv, _, cfg := newConfigTestServer(t)
	payload := mustPayloadJSON(t, fullConfigPayload(cfg, nil))
	req := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(payload))
	req.AddCookie(&http.Cookie{Name: "onet_token", Value: cfg.Token})
	rec := httptest.NewRecorder()
	srv.handleConfig(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func readConfigMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
