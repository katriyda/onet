package main

// config_web.go — 网页读写配置：GET 返回脱敏视图；PUT 校验、持久化，可选交接重启。

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

type configView struct {
	Config
	Version      string       `json:"version"`
	Capabilities capabilities `json:"capabilities"`
	TokenSet     bool         `json:"token_set"`
	ConfigPath   string       `json:"config_path,omitempty"`
}

func (s *Server) configView() configView {
	cfg := s.cfg
	tokenSet := cfg.Token != ""
	cfg.Token = ""
	return configView{
		Config:       cfg,
		Version:      version,
		Capabilities: serverCapabilities(),
		TokenSet:     tokenSet,
		ConfigPath:   s.configPath,
	}
}

type configUpdateReq struct {
	Host          string   `json:"host"`
	Port          int      `json:"port"`
	Token         string   `json:"token"`
	ClearToken    bool     `json:"clear_token"`
	Shell         string   `json:"shell"`
	Cwd           string   `json:"cwd"`
	Scrollback    int      `json:"scrollback"`
	FontSize      int      `json:"font_size"`
	UI            string   `json:"ui"`
	ExtraKeys     []string `json:"extra_keys"`
	LogFile       string   `json:"log_file"`
	LogLevel      string   `json:"log_level"`
	LogMaxSizeMB  int      `json:"log_max_size_mb"`
	LogMaxBackups int      `json:"log_max_backups"`
	LogTerminalIO bool     `json:"log_terminal_io"`
	AllowInsecure bool     `json:"allow_insecure"`
	Restart       bool     `json:"restart"`
}

type configUpdateResp struct {
	Saved        bool   `json:"saved"`
	Restarting   bool   `json:"restarting"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	TokenChanged bool   `json:"token_changed"`
	ConfigPath   string `json:"config_path,omitempty"`
}

func (s *Server) handleConfigUpdate(w http.ResponseWriter, r *http.Request) {
	// 自定义头迫使跨站请求进入 CORS 预检；配合 SameSite cookie 阻止网页 CSRF。
	if r.Header.Get("X-Onet-Config") != "1" {
		http.Error(w, "missing X-Onet-Config header", http.StatusForbidden)
		return
	}
	if s.configPath == "" {
		writeJSONError(w, http.StatusInternalServerError, "服务端未保存配置文件路径")
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.mgr != nil && s.mgr.HandoffActive() {
		writeJSONError(w, http.StatusConflict, "正在更新，请稍后再试")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req configUpdateReq
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "配置格式错误")
		return
	}

	token := s.cfg.Token
	tokenChanged := false
	switch {
	case req.ClearToken && req.Token != "":
		writeJSONError(w, http.StatusBadRequest, "不能同时设置新令牌和清除令牌")
		return
	case req.ClearToken:
		tokenChanged = s.cfg.Token != ""
		token = ""
	case req.Token != "":
		tokenChanged = req.Token != s.cfg.Token
		token = req.Token
	}

	cfg := Config{
		Host:          strings.TrimSpace(req.Host),
		Port:          req.Port,
		Token:         token,
		Shell:         req.Shell,
		Cwd:           req.Cwd,
		Scrollback:    req.Scrollback,
		FontSize:      req.FontSize,
		UI:            strings.TrimSpace(req.UI),
		ExtraKeys:     req.ExtraKeys,
		LogFile:       strings.TrimSpace(req.LogFile),
		LogLevel:      strings.TrimSpace(req.LogLevel),
		LogMaxSizeMB:  req.LogMaxSizeMB,
		LogMaxBackups: req.LogMaxBackups,
		LogTerminalIO: req.LogTerminalIO,
		AllowInsecure: req.AllowInsecure,
		logFileGiven:  true,
	}
	if err := cfg.Validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateConfig(cfg); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 先检查切换冲突，成功保存后把 updating 占住直到进程退出。
	reserved := false
	launched := false
	if req.Restart {
		s.updateMu.Lock()
		if s.updating {
			s.updateMu.Unlock()
			writeJSONError(w, http.StatusConflict, "已有更新或重启正在进行")
			return
		}
		s.updating = true
		s.updateMu.Unlock()
		reserved = true
		defer func() {
			if reserved && !launched {
				s.clearUpdating()
			}
		}()
	}

	if err := Save(s.configPath, cfg); err != nil {
		appLog.Error("config.save_failed", "path", s.configPath, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	appLog.Info("config.saved",
		"path", s.configPath,
		"host", cfg.Host,
		"port", cfg.Port,
		"restart", req.Restart,
		"token_changed", tokenChanged,
	)

	if tokenChanged && req.Restart {
		setOnetTokenCookie(w, token)
	}

	resp := configUpdateResp{
		Saved:        true,
		Restarting:   req.Restart,
		Host:         cfg.Host,
		Port:         cfg.Port,
		TokenChanged: tokenChanged,
		ConfigPath:   s.configPath,
	}
	writeJSON(w, resp)
	_ = http.NewResponseController(w).Flush()

	if !req.Restart {
		return
	}
	launched = true
	rebind := cfg.Host != s.cfg.Host || cfg.Port != s.cfg.Port
	go func() {
		time.Sleep(400 * time.Millisecond)
		exe, err := os.Executable()
		if err != nil {
			s.clearUpdating()
			appLog.Error("config.restart_exe_failed", "err", err)
			s.broadcastNotice("error", "重启失败: 无法定位可执行文件")
			return
		}
		appLog.Info("config.restart_begin", "exe", exe, "rebind", rebind)
		if err := s.beginHandoff(version, exe, rebind); err != nil {
			s.clearUpdating()
			appLog.Error("config.restart_failed", "err", err)
			s.broadcastNotice("error", "重启失败: "+err.Error())
		}
	}()
}

func (s *Server) clearUpdating() {
	s.updateMu.Lock()
	s.updating = false
	s.updateMu.Unlock()
}

func setOnetTokenCookie(w http.ResponseWriter, token string) {
	cookie := &http.Cookie{
		Name:     "onet_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if token == "" {
		cookie.MaxAge = -1
	} else {
		cookie.MaxAge = 365 * 24 * 3600
	}
	http.SetCookie(w, cookie)
}
