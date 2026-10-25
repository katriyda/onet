package main

// main.go — 入口：加载配置、初始化日志、起 HTTP 服务、接收更新交接、优雅退出。

import (
	"context"
	"flag"
	"fmt"
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

// version 由发布构建通过 -ldflags "-X main.version=<tag>" 注入；源码构建为 dev。
var version = "dev"

// defaultConfigPath 默认配置文件与二进制同目录。
func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func main() {
	// 顺手清掉上一次更新留下的 *.old；子命令不加载配置、不初始化日志。
	cleanupStaleBinary()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "update":
			os.Exit(runUpdate(os.Args[2:]))
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		}
	}

	configPath := flag.String("config", defaultConfigPath(), "配置文件路径（默认与二进制同目录的 config.json）")
	flag.Parse()

	os.Exit(runServer(*configPath))
}

func runServer(configPath string) int {
	cfg, err := Load(configPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	closeLog := setupLogging(cfg)
	defer closeLog()

	if cfg.generated {
		fmt.Fprintf(os.Stderr, "已生成默认配置 %s：监听 %s:%d，随机访问令牌已写入该文件\n",
			configPath, cfg.Host, cfg.Port)
		appLog.Info("config.generated", "path", configPath, "host", cfg.Host, "port", cfg.Port,
			"detail", "已写入随机 token；从配置文件读取后登录")
	}

	if err := validateConfig(cfg); err != nil {
		appLog.Error("app.start_refused", "err", err)
		return 1
	}

	srv := NewServer(cfg)
	srv.configPath = configPath
	mgr := NewManager(cfg, srv.broadcastSessions)
	srv.mgr = mgr

	// 热更新交接：Unix 为 exec，Windows 为子进程 + 握手。
	resumePath := os.Getenv(handoffStateEnv)
	_ = os.Unsetenv(handoffStateEnv)
	var restored *handoffState
	if resumePath != "" {
		initial, err := loadHandoffState(resumePath)
		if err != nil {
			appLog.Error("app.resume_load_failed", "path", resumePath, "err", err)
			return 1
		}
		restored, err = handoffAwaitFinalState(resumePath, initial)
		if err != nil {
			appLog.Error("app.resume_handshake_failed", "path", resumePath, "err", err)
			return 1
		}
		// 最终状态已读入内存，立刻删除含终端缓冲的临时文件。
		_ = os.Remove(resumePath)
		if err := mgr.RestoreHandoff(restored); err != nil {
			appLog.Error("app.resume_restore_failed", "err", err)
			return 1
		}
	}

	srv.addr = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	ln, err := srv.listen(restored)
	if err != nil {
		appLog.Error("app.listen_failed", "addr", srv.addr, "err", err)
		return 1
	}
	srv.ln = ln
	httpSrv := &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(appLog.Handler(), slog.LevelError),
	}
	srv.httpSrv = httpSrv

	logPath, _ := activeLogPath.Load().(string)
	appLog.Info("app.start",
		"addr", srv.addr,
		"config", configPath,
		"version", version,
		"resumed", restored != nil,
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

	if restored != nil {
		mgr.StartRestoredReads()
	}
	srv.startServe(ln)
	if restored != nil {
		// Windows：告知父进程监听已就绪；Unix 为空操作。
		handoffSignalServing()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-stop:
		appLog.Info("app.shutdown")
		_ = httpSrv.Shutdown(ctx)
		mgr.KillAll()
	case <-srv.handoffDone:
		// 新进程已经接管 PTY 与端口；这里不能 KillAll。
		appLog.Info("app.handoff_exit")
		_ = httpSrv.Shutdown(ctx)
	}
	return 0
}

// startServe 启动一代 Serve；重新监听后旧 Serve 的返回错误会被忽略。
func (s *Server) startServe(ln net.Listener) {
	gen := s.listenerGen.Add(1)
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			if s.handoffClosing.Load() || gen != s.listenerGen.Load() {
				appLog.Info("app.http_handoff_close")
				return
			}
			appLog.Error("app.http_error", "err", err)
			os.Exit(1)
		}
	}()
}

// listen 优先复用交接过来的监听 socket；Unix 上失败可重新 bind。
func (s *Server) listen(st *handoffState) (net.Listener, error) {
	if st != nil && handoffInheritsListener() && !st.Rebind {
		if ln, err := listenerFromHandoff(st); err == nil {
			appLog.Info("app.listen_inherited", "addr", s.addr)
			return ln, nil
		} else {
			appLog.Warn("app.listen_inherit_failed", "addr", s.addr, "err", err)
		}
	}
	if st != nil && st.Rebind {
		appLog.Info("app.listen_rebind", "addr", s.addr)
	}
	return net.Listen("tcp", s.addr)
}

// reopenListener 交接失败后让旧进程重新接管端口。
func (s *Server) reopenListener() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.handoffClosing.Store(false)
	appLog.Warn("app.http_relisten", "addr", s.addr)
	s.startServe(ln)
	return nil
}
