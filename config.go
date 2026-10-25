package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Config 是 onet 的可选 JSON 配置文件结构。
// 文件缺失时全部使用默认值；文件存在时仅覆盖显式给出的字段。
type Config struct {
	Host       string   `json:"host"`       // 监听地址，默认 "127.0.0.1"
	Port       int      `json:"port"`       // 监听端口，默认 7681
	Token      string   `json:"token"`      // 认证 token，空 = 不认证（仅限本机监听时使用）
	Shell      string   `json:"shell"`      // 要启动的 shell，空 = 平台默认（pwsh/bash）
	Cwd        string   `json:"cwd"`        // 会话工作目录，空 = 用户主目录
	Scrollback int      `json:"scrollback"` // xterm 回滚行数，默认 5000
	FontSize   int      `json:"font_size"`  // 终端字号，默认 15
	UI         string   `json:"ui"`         // "auto"|"mobile"|"desktop"，默认 "auto"
	ExtraKeys  []string `json:"extra_keys"` // 触屏额外按键栏，默认 agent 常用 12 键

	LogFile       string `json:"log_file"`        // 空 = 仅 stderr；默认 <二进制目录>/onet.log
	LogLevel      string `json:"log_level"`       // debug|info|warn|error，默认 info
	LogMaxSizeMB  int    `json:"log_max_size_mb"` // 单文件上限（MB），默认 20，0 = 不轮转
	LogMaxBackups int    `json:"log_max_backups"` // 保留历史份数，默认 5，0 = 轮转时截断
	LogTerminalIO bool   `json:"log_terminal_io"` // 默认 false；true 时记录 PTY 原始输入/输出

	// AllowInsecure 允许非回环监听且 token 为空（局域网手机直连等场景）。
	// 默认 false：非回环 + 空 token 会拒绝启动。
	AllowInsecure bool `json:"allow_insecure"`

	logFileGiven bool // 非 JSON：配置文件是否显式给出 log_file
}

// DefaultConfig 返回全部字段取默认值的 Config。
func DefaultConfig() Config {
	return Config{
		Host:       "127.0.0.1",
		Port:       7681,
		Token:      "",
		Shell:      "",
		Cwd:        "",
		Scrollback: 5000,
		FontSize:   15,
		UI:         "auto",
		ExtraKeys: []string{
			"ESC", "TAB", "CTRL", "ALT", "^C", "ENTER",
			"←", "↑", "↓", "→", "PGUP", "PGDN",
		},
		LogFile:       defaultLogPath(),
		LogLevel:      "info",
		LogMaxSizeMB:  20,
		LogMaxBackups: 5,
		LogTerminalIO: false,
	}
}

// Load 读取配置文件。文件不存在 → 默认值；存在 → 解析并逐字段回填默认值后校验。
func Load(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}

	// 先解析到 map 以区分"显式给出的字段"，再回填。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}

	// 用临时结构接收显式字段（零值即未给出）。
	var given Config
	if err := json.Unmarshal(data, &given); err != nil {
		return Config{}, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}

	if _, ok := raw["host"]; ok {
		cfg.Host = given.Host
	}
	if _, ok := raw["port"]; ok {
		cfg.Port = given.Port
	}
	if _, ok := raw["token"]; ok {
		cfg.Token = given.Token
	}
	if _, ok := raw["shell"]; ok {
		cfg.Shell = given.Shell
	}
	if _, ok := raw["cwd"]; ok {
		cfg.Cwd = given.Cwd
	}
	if _, ok := raw["scrollback"]; ok {
		cfg.Scrollback = given.Scrollback
	}
	if _, ok := raw["font_size"]; ok {
		cfg.FontSize = given.FontSize
	}
	if _, ok := raw["ui"]; ok {
		cfg.UI = given.UI
	}
	if _, ok := raw["extra_keys"]; ok {
		cfg.ExtraKeys = given.ExtraKeys
	}
	if _, ok := raw["log_file"]; ok {
		cfg.LogFile = given.LogFile
		cfg.logFileGiven = true
	}
	if _, ok := raw["log_level"]; ok {
		cfg.LogLevel = given.LogLevel
	}
	if _, ok := raw["log_max_size_mb"]; ok {
		cfg.LogMaxSizeMB = given.LogMaxSizeMB
	}
	if _, ok := raw["log_max_backups"]; ok {
		cfg.LogMaxBackups = given.LogMaxBackups
	}
	if _, ok := raw["log_terminal_io"]; ok {
		cfg.LogTerminalIO = given.LogTerminalIO
	}
	if _, ok := raw["allow_insecure"]; ok {
		cfg.AllowInsecure = given.AllowInsecure
	}

	// 相对路径按配置文件所在目录解析（与二进制同目录的 config.json 行为一致）。
	if cfg.LogFile != "" && !filepath.IsAbs(cfg.LogFile) {
		cfg.LogFile = filepath.Join(filepath.Dir(path), cfg.LogFile)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// isLoopbackHost 判断监听地址是否只对本机可见。
func isLoopbackHost(h string) bool {
	h = strings.TrimSpace(h)
	switch strings.ToLower(h) {
	case "", "localhost":
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// validateConfig 在监听前检查安全边界：非回环 + 空 token 默认拒绝启动。
// allow_insecure=true 时显式放行，并记录一条 WARN 供审计。
func validateConfig(cfg Config) error {
	if isLoopbackHost(cfg.Host) || cfg.Token != "" {
		return nil
	}
	if !cfg.AllowInsecure {
		appLog.Error("app.insecure_refused", "host", cfg.Host,
			"reason", "非回环监听且 token 为空；设置 token 或 allow_insecure=true")
		return fmt.Errorf("配置错误: host=%q 非回环监听且 token 为空（设置 token，或显式 allow_insecure=true）", cfg.Host)
	}
	appLog.Warn("app.insecure_allowed", "host", cfg.Host,
		"detail", "非回环监听且 token 为空：同一网络内任何人都能访问完整 shell")
	return nil
}

// Validate 校验配置取值。
func (c Config) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("配置错误: host 不能为空")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("配置错误: port %d 超出 1-65535", c.Port)
	}
	if c.Scrollback <= 0 {
		return fmt.Errorf("配置错误: scrollback %d 必须大于 0", c.Scrollback)
	}
	if c.FontSize <= 0 {
		return fmt.Errorf("配置错误: font_size %d 必须大于 0", c.FontSize)
	}
	switch c.UI {
	case "auto", "mobile", "desktop":
	default:
		return fmt.Errorf("配置错误: ui %q 必须是 auto/mobile/desktop 之一", c.UI)
	}
	if _, err := parseLogLevel(c.LogLevel); err != nil {
		return fmt.Errorf("配置错误: log_level %q 必须是 debug/info/warn/error 之一", c.LogLevel)
	}
	if c.LogMaxSizeMB < 0 || c.LogMaxSizeMB > 1024 {
		return fmt.Errorf("配置错误: log_max_size_mb 必须在 0-1024 之间")
	}
	if c.LogMaxBackups < 0 || c.LogMaxBackups > 100 {
		return fmt.Errorf("配置错误: log_max_backups 必须在 0-100 之间")
	}
	// extra_keys 任意字符串均接受：未知符号按字面量文本发送。
	return nil
}
