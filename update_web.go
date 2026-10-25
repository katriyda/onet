package main

// update_web.go — 网页触发的更新：GET /update 检查版本，POST /update 下载安装并热切换。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

type updateCheckResp struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Asset     string `json:"asset,omitempty"`
	Available bool   `json:"available"`
	Updating  bool   `json:"updating"`
}

type updateInstallResp struct {
	Updated    bool   `json:"updated"`
	From       string `json:"from"`
	To         string `json:"to,omitempty"`
	Restarting bool   `json:"restarting,omitempty"`
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleUpdateCheck(w, r)
	case http.MethodPost:
		s.handleUpdateInstall(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if s.updateInProgress() {
		writeJSON(w, updateCheckResp{Current: version, Updating: true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}

	cand, err := findCandidate(ctx, client)
	if err != nil {
		appLog.Warn("update.check_failed", "err", err)
		writeJSONError(w, http.StatusBadGateway, "检查更新失败: "+err.Error())
		return
	}
	resp := updateCheckResp{
		Current:   version,
		Latest:    normalizeVersion(cand.Rel.Tag),
		Asset:     cand.Asset.Name,
		Available: !cand.upToDate(false),
	}
	appLog.Info("update.check", "current", version, "latest", cand.Rel.Tag, "available", resp.Available)
	writeJSON(w, resp)
}

// updateInProgress 返回是否有安装/切换正在执行（互斥读写 updating）。
func (s *Server) updateInProgress() bool {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	return s.updating
}

func (s *Server) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	// 自定义头迫使跨站请求进入 CORS 预检；配合 SameSite cookie 阻止网页 CSRF。
	if r.Header.Get("X-Onet-Update") != "1" {
		http.Error(w, "missing X-Onet-Update header", http.StatusForbidden)
		return
	}
	if s.mgr.HandoffActive() {
		writeJSONError(w, http.StatusConflict, "正在更新，请稍候")
		return
	}

	s.updateMu.Lock()
	if s.updating {
		s.updateMu.Unlock()
		writeJSONError(w, http.StatusConflict, "已有更新正在进行")
		return
	}
	s.updating = true
	s.updateMu.Unlock()
	clearUpdating := func() {
		s.updateMu.Lock()
		s.updating = false
		s.updateMu.Unlock()
	}

	// 下载可能超过浏览器请求生命周期；用户关页也继续完成安装。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Minute}

	cand, err := findCandidate(ctx, client)
	if err != nil {
		clearUpdating()
		appLog.Warn("update.install_check_failed", "err", err)
		writeJSONError(w, http.StatusBadGateway, "检查更新失败: "+err.Error())
		return
	}
	if cand.upToDate(false) {
		clearUpdating()
		appLog.Info("update.already_latest", "current", version, "latest", cand.Rel.Tag)
		writeJSON(w, updateInstallResp{Updated: false, From: version, To: normalizeVersion(cand.Rel.Tag)})
		return
	}

	appLog.Info("update.install_begin", "current", version, "latest", cand.Rel.Tag, "asset", cand.Asset.Name)
	exe, err := installCandidate(ctx, client, cand)
	if err != nil {
		clearUpdating()
		appLog.Error("update.install_failed", "err", err)
		writeJSONError(w, http.StatusBadGateway, "安装更新失败: "+err.Error())
		return
	}
	appLog.Info("update.install_done", "exe", exe, "to", cand.Rel.Tag)

	writeJSON(w, updateInstallResp{
		Updated:    true,
		From:       version,
		To:         normalizeVersion(cand.Rel.Tag),
		Restarting: true,
	})
	// 先把响应刷给页面，再切换进程；页面收到后显示“正在重启”。
	_ = http.NewResponseController(w).Flush()

	go func() {
		time.Sleep(400 * time.Millisecond)
		if err := s.beginHandoff(cand.Rel.Tag, exe); err != nil {
			clearUpdating()
			appLog.Error("app.handoff_failed", "err", err)
			s.broadcastNotice("error", "更新失败: "+err.Error())
		}
		// 成功时进程即将退出；保持 updating=true 阻止并发更新请求。
	}()
}

// beginHandoff 冻结会话、生成快照并交给平台实现执行切换。
func (s *Server) beginHandoff(to, exe string) error {
	s.handoffGuard.Lock()
	defer s.handoffGuard.Unlock()

	if !s.mgr.BeginHandoff() {
		return fmt.Errorf("已有更新正在进行")
	}
	st, sessions := snapshotHandoff(s.mgr)
	statePath, err := createHandoffStatePath()
	if err != nil {
		s.mgr.EndHandoff()
		return fmt.Errorf("创建交接状态文件失败: %w", err)
	}
	appLog.Info("app.handoff_begin", "from", version, "to", to, "sessions", len(sessions), "exe", exe)
	if err := startHandoffPlatform(s, st, sessions, statePath, exe); err != nil {
		_ = os.Remove(statePath)
		s.mgr.EndHandoff()
		return err
	}
	return nil
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": msg})
}
