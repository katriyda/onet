package main

// logging.go — 全链路日志的唯一入口：JSONL 文件（带大小轮转）、日志级别、
// 浏览器回传通道、网页查看/下载所需的读取与脱敏辅助。
//
// 约定：所有事件用 appLog 记录，msg 为点分事件名（ws.open / pty.spawn …），
// 附加字段一律走 key-value。终端原始输入/输出默认不记录，由 log_terminal_io 显式开启。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// appLog 是全局日志句柄。setupLogging 之前默认只写 stderr。
var appLog = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// activeLogPath 保存当前生效的日志文件路径；空串表示未启用文件日志。
var activeLogPath atomic.Value

func init() {
	activeLogPath.Store("")
}

// defaultLogPath 默认日志文件与二进制同目录。
func defaultLogPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "onet.log"
	}
	return filepath.Join(filepath.Dir(exe), "onet.log")
}

// parseLogLevel 解析 debug/info/warn/error（大小写不敏感）。
func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("未知日志级别 %q", raw)
	}
}

// setupLogging 按配置初始化全局日志：文件（JSONL + 轮转）+ stderr 双写。
// 返回的 closeFn 只关闭日志文件；文件不可用时为空函数（不阻止启动）。
func setupLogging(cfg Config) (closeFn func()) {
	activeLogPath.Store("")

	lvl, err := parseLogLevel(cfg.LogLevel)
	if err != nil {
		lvl = slog.LevelInfo
	}

	var sink io.Writer = os.Stderr
	closer := func() {}

	if cfg.LogFile != "" {
		rw, openErr := openLogWriter(cfg.LogFile, cfg)
		resolved := cfg.LogFile
		if openErr != nil && !cfg.logFileGiven {
			// 默认路径（二进制目录）可能不可写 → 回退到当前工作目录再试一次。
			fallback := filepath.Join(".", "onet.log")
			if alt, aerr := openLogWriter(fallback, cfg); aerr == nil {
				rw, openErr, resolved = alt, nil, fallback
			}
		}
		if openErr != nil {
			fmt.Fprintf(os.Stderr, "日志文件不可用，仅输出到 stderr: %v\n", openErr)
		} else {
			activeLogPath.Store(resolved)
			sink = io.MultiWriter(rw, os.Stderr)
			closer = func() { _ = rw.Close() }
		}
	}

	handler := slog.NewJSONHandler(sink, &slog.HandlerOptions{
		Level:     lvl,
		AddSource: lvl == slog.LevelDebug,
	})
	appLog = slog.New(handler)
	slog.SetDefault(appLog)
	return closer
}

// openLogWriter 打开（必要时创建目录）日志文件写入器。
func openLogWriter(path string, cfg Config) (*rotatingWriter, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return newRotatingWriter(path, int64(cfg.LogMaxSizeMB)<<20, cfg.LogMaxBackups)
}

// ---------- rotatingWriter：按大小轮转的追加写入器 ----------

type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	size     int64
	maxBytes int64
	backups  int
}

func newRotatingWriter(path string, maxBytes int64, backups int) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w := &rotatingWriter{path: path, f: f, maxBytes: maxBytes, backups: backups}
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
	return w, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	// 单条记录本身超过上限时不轮转（否则会无限轮转），直接追加。
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate 关闭当前文件并滚动历史：path → path.1 → … → path.<backups>（最旧的删除）。
// backups == 0 时截断当前文件。rename 失败尽力而为，重新打开失败才返回错误。
func (w *rotatingWriter) rotate() error {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	if w.backups == 0 {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		w.f, w.size = f, 0
		return nil
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.backups))
	for i := w.backups - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	_ = os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.f, w.size = f, 0
	return nil
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// ---------- 小工具 ----------

// truncateUTF8 按 rune 边界截断超长字符串，避免切断多字节字符。
func truncateUTF8(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// redactedQuery 返回脱敏后的查询串：key 含 token 的值替换为 ***（日志绝不落 token）。
func redactedQuery(raw string) string {
	if raw == "" {
		return ""
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "<invalid>"
	}
	for k, vs := range q {
		if !strings.Contains(strings.ToLower(k), "token") {
			continue
		}
		for i := range vs {
			vs[i] = "***"
		}
		q[k] = vs
	}
	return q.Encode()
}

// tailFile 读取文件末尾最多 maxLines 行（最多 maxBytes 字节）。
// 从中间开始时丢掉第一个不完整行；该块内没有换行（单行超大）时返回空。
func tailFile(path string, maxLines int, maxBytes int64) ([]byte, error) {
	const maxTailBytes = 2 << 20
	if maxBytes <= 0 || maxBytes > maxTailBytes {
		maxBytes = maxTailBytes
	}
	if maxLines <= 0 {
		maxLines = 1
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	if start > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil, nil
		}
		data = data[i+1:]
	}
	if len(data) == 0 {
		return nil, nil
	}

	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out := make([]byte, 0, len(data)+1)
	out = append(out, bytes.Join(lines, []byte("\n"))...)
	out = append(out, '\n')
	return out, nil
}

// logParts 按从旧到新的顺序返回日志文件与历史份数：path.<backups> … path.1, path。
func logParts(path string, backups int) []string {
	parts := make([]string, 0, backups+1)
	for i := backups; i >= 1; i-- {
		parts = append(parts, fmt.Sprintf("%s.%d", path, i))
	}
	return append(parts, path)
}

// logFilesExist 报告任一日志分片是否存在。
func logFilesExist(path string, backups int) bool {
	for _, p := range logParts(path, backups) {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// streamLogFiles 按从旧到新的顺序把全部分片写入 w。单分片失败继续下一个并返回首个错误。
func streamLogFiles(w io.Writer, path string, backups int) (written int64, found bool, err error) {
	found = logFilesExist(path, backups)
	if !found {
		return 0, false, nil
	}
	for _, p := range logParts(path, backups) {
		f, openErr := os.Open(p)
		if openErr != nil {
			if !os.IsNotExist(openErr) && err == nil {
				err = openErr
			}
			continue
		}
		n, copyErr := io.Copy(w, f)
		_ = f.Close()
		written += n
		if copyErr != nil {
			if err == nil {
				err = copyErr
			}
			continue
		}
	}
	return written, true, err
}

// ---------- 浏览器回传日志 ----------

// clientLogEntry 是一条浏览器日志（客户端结构，字段截断后才落盘）。
type clientLogEntry struct {
	Level string `json:"level"` // debug|info|warn|error，其他按 info
	Event string `json:"event"`
	Msg   string `json:"msg"`
	Stack string `json:"stack"`
	TS    int64  `json:"ts"` // 浏览器 Date.now() 毫秒
}

const (
	maxClientLogEntries = 50
	maxClientLogBytes   = 4096
)

func clientLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// handleClientLogs 把浏览器批量回传的日志落盘，每条一个 client.log 事件。
func handleClientLogs(c *Client, clientID string, entries []clientLogEntry) {
	conn := ""
	if c != nil {
		conn = c.logID()
	}
	if extra := len(entries) - maxClientLogEntries; extra > 0 {
		appLog.Warn("client.log_dropped", "conn", conn, "dropped", extra)
		entries = entries[:maxClientLogEntries]
	}
	clientID = truncateUTF8(clientID, 64)
	for _, e := range entries {
		attrs := []any{
			"conn", conn,
			"client", clientID,
			"event", truncateUTF8(e.Event, 128),
			"text", truncateUTF8(e.Msg, maxClientLogBytes),
			"stack", truncateUTF8(e.Stack, maxClientLogBytes),
		}
		if e.TS > 0 {
			t := time.UnixMilli(e.TS)
			attrs = append(attrs,
				"client_ts", t.UTC().Format(time.RFC3339Nano),
				"client_age_ms", time.Since(t).Milliseconds(),
			)
		}
		appLog.Log(context.Background(), clientLogLevel(e.Level), "client.log", attrs...)
	}
}
