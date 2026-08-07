package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"golang.org/x/crypto/bcrypt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ──── 会话管理（HTTP 层特有，不归入 init.Broker）────

const (
	sessionCookie = "tools115_session"
	sessionTTL    = 7 * 24 * time.Hour
)

type sessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

func newSessionStore() sessionStore {
	return sessionStore{tokens: make(map[string]time.Time)}
}

func (s *sessionStore) create() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	maps.DeleteFunc(s.tokens, func(_ string, exp time.Time) bool { return now.After(exp) })
	s.tokens[token] = now.Add(sessionTTL)
	return token
}

func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.tokens, token)
		return false
	}
	return true
}

func (s *sessionStore) remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

func (s *Server) authRequired() bool {
	return s.Broker.AuthRequired()
}

func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.sessions.valid(c.Value)
}

func (s *Server) protect(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authRequired() && !s.loggedIn(r) {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next(w, r)
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"auth_required": s.authRequired(),
		"logged_in":     !s.authRequired() || s.loggedIn(r),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	user, passHash := s.Broker.GetAuth()
	if user == "" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(user)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(passHash), []byte(req.Password)) == nil
	if !userOK || !passOK {
		time.Sleep(500 * time.Millisecond)
		logs.Warn(logs.ModuleSystem, "登录失败", "用户名", req.Username, "来源", clientIP(r))
		writeErr(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.sessions.create(), Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	logs.Info(logs.ModuleSystem, "登录成功", "用户名", req.Username, "来源", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.remove(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ──── SSE 写器 ────

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	return &sseWriter{w: w, flusher: flusher}, true
}

func (s *sseWriter) writeData(payload string) bool {
	if _, err := s.w.Write([]byte("data: " + payload + "\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}

func (s *sseWriter) writeComment(msg string) bool {
	if _, err := s.w.Write([]byte(":" + msg + "\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}

func serveSSE[T any](w http.ResponseWriter, r *http.Request, appCtx context.Context, events <-chan T, replay []T) {
	sw, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if !sw.writeComment("connected") {
		return
	}
	// 回放打包为单个数据帧（JSON 数组），避免 1000 条逐条写+Flush 拖慢首屏。
	if len(replay) > 0 {
		data, err := json.Marshal(replay)
		if err == nil && !sw.writeData(string(data)) {
			return
		}
	}
	writeFrame := func(v T) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return true
		}
		return sw.writeData(string(data))
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-appCtx.Done():
			return
		case <-heartbeat.C:
			if !sw.writeComment("ping") {
				return
			}
		case ev, ok := <-events:
			if !ok || !writeFrame(ev) {
				return
			}
		}
	}
}

// ──── 任务启停 ────

func (s *Server) handleTaskStart(w http.ResponseWriter, r *http.Request) {
	if err := s.Broker.StartTask(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request) {
	s.Broker.StopTask(r.PathValue("name"))
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// ──── 配置 ────

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Broker.ConfigSnapshot())
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req config.Editable
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if err := s.Broker.ApplyConfig(r.Context(), req); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存配置失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ──── 日志 SSE ────

const logReplayLimit = 1000

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	sub := s.Broker.Subscribe()
	defer s.Broker.Unsubscribe(sub)

	snap := s.Broker.Snapshot()
	replay := s.Broker.Recent(logReplayLimit)
	replay = append([]logs.Entry{{
		Time: time.Now(), Level: "INFO", Module: "status", Msg: "状态快照",
		Status: &logs.StatusData{
			Ready:       snap.Ready,
			ConfigReady: snap.ConfigReady,
			Missing:     snap.Missing,
			InitError:   snap.InitError,
			Sync:        snap.Sync,
			Strm:        snap.Strm,
		},
	}}, replay...)

	serveSSE(w, r, s.AppCtx, sub, replay)
}

func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.Broker.Clear()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ──── 离线下载 ────

const _torrentMaxSize = 10 << 20

func (s *Server) handleOfflineTasks(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	list, err := s.Broker.OfflineTaskList(r.Context(), page)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取任务列表失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleOfflineQuota(w http.ResponseWriter, r *http.Request) {
	quota, err := s.Broker.OfflineQuotaInfo(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取配额失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

func (s *Server) handleOfflineAdd(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	var req struct {
		Urls     string `json:"urls"`
		SavePath string `json:"save_path"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	var urls []string
	for line := range strings.Lines(req.Urls) {
		if line = strings.TrimSpace(line); line != "" {
			urls = append(urls, line)
		}
	}
	if len(urls) == 0 {
		writeErr(w, http.StatusBadRequest, "请至少提供一条下载链接")
		return
	}

	dirID, err := s.Broker.ResolveCloudDir(r.Context(), req.SavePath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "保存目录无效: %v", err)
		return
	}

	results, err := s.Broker.AddOfflineTasks(r.Context(), urls, dirID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "添加离线任务失败: %v", err)
		return
	}
	added := 0
	for _, res := range results {
		if res.State {
			added++
		}
	}
	logs.Info(logs.ModuleSystem, "添加离线任务", "提交", len(urls), "成功", added, "目标目录", dirID, "耗时", time.Since(t0))
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "results": results})
}

func (s *Server) handleOfflineTorrent(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	if err := r.ParseMultipartForm(_torrentMaxSize); err != nil {
		writeErr(w, http.StatusBadRequest, "解析上传数据失败: %v", err)
		return
	}
	file, hdr, err := r.FormFile("torrent")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "未收到种子文件")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "读取种子文件失败")
		return
	}

	cfg := s.Broker.ConfigSnapshot()

	savePath := strings.TrimSpace(r.FormValue("save_path"))
	if savePath == "" {
		savePath = strings.TrimSpace(cfg.StrmPath)
	}
	savePath = strings.Trim(savePath, "/")
	if savePath == "" {
		savePath = "/"
	}

	torrentPath := strings.TrimSpace(cfg.TorrentPath)
	if torrentPath == "" {
		torrentPath = strings.TrimSpace(cfg.TempPath)
	}
	if torrentPath == "" {
		torrentPath = "/"
	}
	torrentCID, err := s.Broker.ResolveCloudDir(r.Context(), torrentPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "种子临时目录无效: %v", err)
		return
	}
	result, err := s.Broker.AddTorrentTask(r.Context(), data, hdr.Filename, torrentCID, savePath)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "添加种子任务失败: %v", err)
		return
	}

	logs.Info(logs.ModuleSystem, "添加种子任务",
		"文件名", hdr.Filename, "大小", len(data),
		"info_hash", result.InfoHash, "保存路径", savePath, "成功", result.State, "耗时", time.Since(t0))
	writeJSON(w, http.StatusOK, map[string]any{
		"added":   boolToInt(result.State),
		"results": []drive.OfflineAddResult{*result},
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Server) handleOfflineDelete(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	var req struct {
		InfoHash    string `json:"info_hash"`
		DeleteFiles bool   `json:"delete_files"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if req.InfoHash == "" {
		writeErr(w, http.StatusBadRequest, "缺少 info_hash")
		return
	}
	if err := s.Broker.DeleteOfflineTask(r.Context(), req.InfoHash, req.DeleteFiles); err != nil {
		writeErr(w, http.StatusBadGateway, "删除任务失败: %v", err)
		return
	}
	logs.Info(logs.ModuleSystem, "删除离线任务", "info_hash", req.InfoHash, "删除源文件", req.DeleteFiles, "耗时", time.Since(t0))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleOfflineClear(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	var req struct {
		Flag int `json:"flag"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if req.Flag < 0 || req.Flag > 5 {
		writeErr(w, http.StatusBadRequest, "flag 取值范围 0-5")
		return
	}
	if err := s.Broker.ClearOfflineTasks(r.Context(), req.Flag); err != nil {
		writeErr(w, http.StatusBadGateway, "清除任务失败: %v", err)
		return
	}
	logs.Info(logs.ModuleSystem, "批量清除任务", "flag", req.Flag, "耗时", time.Since(t0))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
