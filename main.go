package main

// main.go — 入口：加载配置、起 HTTP 服务、优雅退出。

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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

	srv := NewServer(cfg)
	mgr := NewManager(cfg, srv.broadcastSessions)
	srv.mgr = mgr

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("onet 已启动: http://%s（UI=%s，token 认证=%v）", addr, cfg.UI, cfg.Token != "")
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("正在关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	mgr.KillAll()
}
