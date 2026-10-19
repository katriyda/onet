package main

// main.go — 入口：加载配置、初始化日志、起 HTTP 服务、优雅退出。

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// defaultConfigPath 默认配置文件与二进制同目录。
func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func main() {
	configPath := flag.String("config", defaultConfigPath(), "配置文件路径（默认与二进制同目录的 config.json）")
	flag.Parse()

	cfg, err := Load(*configPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	closeLog := setupLogging(cfg)
	defer closeLog()

	srv := NewServer(cfg)
	mgr := NewManager(cfg, srv.broadcastSessions)
	srv.mgr = mgr

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(appLog.Handler(), slog.LevelError),
	}

	logPath, _ := activeLogPath.Load().(string)
	appLog.Info("app.start",
		"addr", addr,
		"ui", cfg.UI,
		"auth", cfg.Token != "",
		"shell", cfg.Shell,
		"cwd", cfg.Cwd,
		"log_file", logPath,
		"log_level", cfg.LogLevel,
		"terminal_io", cfg.LogTerminalIO,
		"go", runtime.Version(),
		"os", runtime.GOOS,
		"arch", runtime.GOARCH,
		"pid", os.Getpid(),
	)
	if cfg.LogTerminalIO {
		appLog.Warn("app.insecure_log", "detail", "log_terminal_io 已开启：日志包含终端原始输入/输出，可能泄露密码或密钥")
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// 文件写入无缓冲，记录后即可退出。
			appLog.Error("app.http_error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	appLog.Info("app.shutdown")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	mgr.KillAll()
}
