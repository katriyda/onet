package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 是 onet 的可选 JSON 配置文件结构。
// 文件缺失时全部使用默认值；文件存在时仅覆盖显式给出的字段。
type Config struct {
	Host       string   `json:"host"`        // 监听地址，默认 "127.0.0.1"
	Port       int      `json:"port"`        // 监听端口，默认 7681
	Token      string   `json:"token"`       // 认证 token，空 = 不认证（仅限本机监听时使用）
	Shell      string   `json:"shell"`       // 要启动的 shell，空 = 平台默认（pwsh/bash）
	Cwd        string   `json:"cwd"`         // 会话工作目录，空 = 用户主目录
	Scrollback int      `json:"scrollback"`  // xterm 回滚行数，默认 5000
	FontSize   int      `json:"font_size"`   // 终端字号，默认 15
	UI         string   `json:"ui"`          // "auto"|"mobile"|"desktop"，默认 "auto"
	ExtraKeys  []string `json:"extra_keys"`  // 触屏额外按键栏，默认 agent 常用 12 键
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

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
	// extra_keys 任意字符串均接受：未知符号按字面量文本发送。
	return nil
}
