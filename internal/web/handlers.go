package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/cache"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"golang.org/x/crypto/bcrypt"
)

// ──── 会话管理（HTTP 层特有）────

const (
	sessionCookie = "tools115_session"
	sessionTTL    = 7 * 24 * time.Hour
)

type sessionStore struct {
	// sync.Map：token → time.Time（过期时刻）。登录写入低频、校验读取高频，
	// 正是 sync.Map 目标场景（读多写少、key 不相交），无需加锁。
	tokens sync.Map
}

func (s *sessionStore) create() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 几乎不会失败（内部自动兜底）；真失败时 token 退化为零值，
		// 生成的会话不可用即被拒绝，此处仅告警。
		logs.Error(logs.ModuleSystem, "生成会话令牌失败", "错误", err)
	}
	token := hex.EncodeToString(buf)
	now := time.Now()
	// 惰性清理过期会话（登录低频，Range 全扫成本可忽略）
	s.tokens.Range(func(k, v any) bool {
		if now.After(v.(time.Time)) {
			s.tokens.Delete(k)
		}
		return true
	})
	s.tokens.Store(token, now.Add(sessionTTL))
	return token
}

func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	v, ok := s.tokens.Load(token)
	if !ok {
		return false
	}
	if time.Now().After(v.(time.Time)) {
		s.tokens.Delete(token)
		return false
	}
	return true
}

func (s *sessionStore) remove(token string) {
	s.tokens.Delete(token)
}

func (s *Server) authRequired() bool {
	return s.App.AuthRequired()
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

	user, passHash := s.App.GetAuth()
	if user == "" {
		writeOK(w, http.StatusOK)
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
	writeOK(w, http.StatusOK)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.remove(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	writeOK(w, http.StatusOK)
}

// ──── SSE 写器 ────

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
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

// sseConnect 完成 SSE 响应头设置并发送首条 connected 注释帧；不支持流式或写入首帧失败时
// 返回 (nil, false)，由调用方写错误响应。serveSSE 与 handleLogsCounts 共用，避免重复样板。
func sseConnect(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sw := &sseWriter{w: w, flusher: flusher}
	if !sw.writeComment("connected") {
		return nil, false
	}
	return sw, true
}

// serveSSE 把 events 流实时推给单个订阅者，并在连接建立时先回放 replay。
// match 可选：非 nil 时回放与实时事件都先经它过滤（分类日志 SSE 用）；nil 表示不过滤。
func serveSSE(w http.ResponseWriter, r *http.Request, appCtx context.Context, events <-chan logs.Entry, replay []logs.Entry, match func(logs.Entry) bool) {
	sw, ok := sseConnect(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	pass := func(v logs.Entry) bool { return match == nil || match(v) }
	// 回放打包为单个数据帧（JSON 数组），避免 1000 条逐条写+Flush 拖慢首屏。
	if len(replay) > 0 {
		if match != nil {
			// 用 slices.DeleteFunc 在克隆上过滤，避免改动调用方持有的 replay 底层数组
			//（RecentFiltered 返回的切片可能被上层复用，原地写入会污染回放缓冲）。
			replay = slices.DeleteFunc(slices.Clone(replay), func(v logs.Entry) bool {
				return !pass(v)
			})
		}
		if len(replay) > 0 {
			data, err := json.Marshal(replay)
			if err == nil && !sw.writeData(string(data)) {
				return
			}
		}
	}
	writeFrame := func(v logs.Entry) bool {
		if !pass(v) {
			return true
		}
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
	if err := s.App.StartTask(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request) {
	s.App.StopTask(r.PathValue("name"))
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// ──── 配置 ────

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.App.ConfigSnapshot())
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req config.Editable
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if err := s.App.ApplyConfig(r.Context(), req); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存配置失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ──── 日志 SSE ────

// logReplayLimit 切换分类时 SSE 回放的最近条数：只够首屏一屏，更早的由 /api/logs/history 滚动翻页按需取。
const logReplayLimit = 300

// handleLogsCounts 分类日志计数流：事件驱动，仅在有新日志写入（计数可能变化）时推送，
// 空闲不推送——替代原 300ms 定时轮询（无日志时仍高频空推的浪费）。计数直接扫描 ring
// （与回放/翻页同一数据源），保证「chip 显示有日志 ⇔ 点进去能看到日志」严格一致；
// 早期日志被 ring 淘汰后计数同步回落，不会出现计数有、内容无的矛盾。
// 与 handleLogs（按 cat 过滤的日志流）分离：计数全局、日志流按分类过滤，故需独立流；此处订阅日志流，
// 有日志即推计数，与「日志推送即计数更新」语义一致。
func (s *Server) handleLogsCounts(w http.ResponseWriter, r *http.Request) {
	sw, ok := sseConnect(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sub := s.App.Subscribe()
	defer s.App.Unsubscribe(sub)

	send := func() bool {
		data, err := json.Marshal(map[string]any{"counts": s.App.LogCounts()})
		if err != nil {
			return true
		}
		return sw.writeData(string(data))
	}
	if !send() { // 连接即推送当前计数，保证 chip 立即可见
		return
	}

	// 事件驱动：订阅日志流，任意日志写入即标记脏，150ms 内合并推送一次（突发日志不洪泛）；
	// 空闲时不推送。与 serveSSE 对齐保留 15s 心跳，避免空闲连接被反向代理掐断。
	// 每次推送重新扫描 ring 取最新可见计数，即使订阅丢帧也不影响准确性（只把 entry 当脏信号）。
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	select {
	case <-debounce.C:
	default:
	}
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	var dirty bool
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.AppCtx.Done():
			return
		case <-hb.C:
			if !sw.writeComment("ping") {
				return
			}
		case _, ok := <-sub:
			if !ok {
				return
			}
			if !dirty {
				dirty = true
				debounce.Reset(150 * time.Millisecond)
			}
		case <-debounce.C:
			if !send() {
				return
			}
			dirty = false
		}
	}
}

// handleLogs 单一日志通道：支持 ?cat= 分类参数。后端按分类过滤回放历史并实时推送，
// 前端切换分类时断开重建本连接即可，无需再走独立的历史查询接口。
// cat=all|warn|error|模块名；缺省 all。状态帧（Module="status"）始终推送，不参与分类过滤。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	sub := s.App.Subscribe()
	defer s.App.Unsubscribe(sub)

	cat := r.URL.Query().Get("cat")
	if cat == "" {
		cat = "all"
	}
	filter := logs.LogFilter(cat)
	// 状态帧始终放行；其余按分类过滤（回放与实时共用）。
	match := func(e logs.Entry) bool {
		if e.Status != nil {
			return true
		}
		return filter.Matches(e)
	}

	// 状态快照作为首条事件回放（直接复用 App.Snapshot 单一类型）
	snap := s.App.Snapshot()
	// 回放该分类最近 logReplayLimit 条（而非 ring 全部）：首屏只需一屏，更早的由
	// /api/logs/history 滚动翻页按需取（ring 仍有全量）。避免一次性推送 5000 条、前端只渲染 300 条的浪费。
	replay := s.App.RecentFiltered(cat, logReplayLimit)
	replay = slices.Concat([]logs.Entry{{
		Time: time.Now(), Level: "INFO", Module: "status", Msg: "状态快照",
		Status: snap,
	}}, replay)

	serveSSE(w, r, s.AppCtx, sub, replay, match)
}

func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.App.ClearLogs()
	writeOK(w, http.StatusOK)
}

// handleLogsHistory 向前翻页：返回某分类中 Seq<before 的最近最多 limit 条日志（升序），
// 供前端向上滚动加载更早历史。before 为当前视图顶部日志的 seq（缺失/0 表示取最新 limit 条）。
func (s *Server) handleLogsHistory(w http.ResponseWriter, r *http.Request) {
	cat := r.URL.Query().Get("cat")
	if cat == "" {
		cat = "all"
	}
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil {
		before = 0 // 缺失/非法 → 取最新 limit 条
	}
	limit, err := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if err != nil {
		limit = 0 // 缺失/非法 → 走下方默认 200
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	writeJSON(w, http.StatusOK, s.App.LogHistory(cat, before, limit))
}

// ──── 离线下载 ────

const _torrentMaxSize = 10 << 20

func (s *Server) handleOfflineTasks(w http.ResponseWriter, r *http.Request) {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil {
		page = 0 // 缺失/非法 → 第 0 页
	}
	list, err := s.App.OfflineTaskList(r.Context(), page)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取任务列表失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleOfflineQuota(w http.ResponseWriter, r *http.Request) {
	quota, err := s.App.OfflineQuotaInfo(r.Context())
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
		if norm := normalizeMagnetURL(line); norm != "" {
			urls = append(urls, norm)
		}
	}
	if len(urls) == 0 {
		writeErr(w, http.StatusBadRequest, "请至少提供一条下载链接")
		return
	}

	savePath := strings.TrimSpace(req.SavePath)
	if savePath == "" {
		savePath = strings.TrimSpace(s.App.ConfigSnapshot().StrmPath)
	}
	dirID, err := s.App.ResolveCloudDir(r.Context(), savePath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "保存目录无效: %v", err)
		return
	}

	results, err := s.App.AddOfflineTasks(r.Context(), urls, dirID, savePath)
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
	logs.Info(logs.ModuleSystem, "添加离线任务", "提交", len(urls), "成功", added, "保存路径", savePath, "耗时", time.Since(t0))
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "results": results})
}

// normalizeMagnetURL 规范化单个磁链/下载链接输入（web 层输入校验的一部分，
// 不在 drive 侧做）：用户漏写 magnet:?xt=urn:btih: 前缀、只贴 info_hash
// （40 位 hex 或 32 位 base32）时自动补全；已是完整链接则原样返回；空行返回空串。
func normalizeMagnetURL(line string) string {
	s := strings.TrimSpace(line)
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "magnet:") || strings.Contains(s, "://") {
		return s
	}
	hash := strings.TrimPrefix(s, "btih:")
	hash = strings.TrimPrefix(hash, "urn:btih:")
	if isBTHash(hash) {
		return "magnet:?xt=urn:btih:" + hash
	}
	return s
}

// isBTHash 判断字符串是否为合法的 BT info_hash：40 位 hex 或 32 位 base32（大小写不敏感）。
func isBTHash(s string) bool {
	if len(s) == 40 {
		for _, c := range s {
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
		return true
	}
	if len(s) == 32 {
		for _, c := range s {
			if !strings.ContainsRune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ234567", c) {
				return false
			}
		}
		return true
	}
	return false
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
	defer func() {
		if cerr := file.Close(); cerr != nil {
			logs.Debug(logs.ModuleSystem, "关闭种子文件失败", "错误", cerr)
		}
	}()

	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "读取种子文件失败")
		return
	}

	cfg := s.App.ConfigSnapshot()

	savePath := strings.TrimSpace(r.FormValue("save_path"))
	if savePath == "" {
		savePath = strings.TrimSpace(cfg.StrmPath)
	}
	savePath = strings.Trim(savePath, "/")
	if savePath == "" {
		savePath = "/"
	}

	result, err := s.App.AddTorrentTask(r.Context(), data, hdr.Filename, savePath)
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
	if err := s.App.DeleteOfflineTask(r.Context(), req.InfoHash, req.DeleteFiles); err != nil {
		writeErr(w, http.StatusBadGateway, "删除任务失败: %v", err)
		return
	}
	logs.Info(logs.ModuleSystem, "删除离线任务", "info_hash", req.InfoHash, "删除源文件", req.DeleteFiles, "耗时", time.Since(t0))
	writeOK(w, http.StatusOK)
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
	if err := s.App.ClearOfflineTasks(r.Context(), req.Flag); err != nil {
		writeErr(w, http.StatusBadGateway, "清除任务失败: %v", err)
		return
	}
	logs.Info(logs.ModuleSystem, "批量清除任务", "flag", req.Flag, "耗时", time.Since(t0))
	writeOK(w, http.StatusOK)
}

// ──── 透传本地缓存管理 ────

// handleCacheList 返回全部缓存条目（文件名升序）+ 汇总（条目数、总占用）。
// Cache 为 nil（未启用本地缓存）时返回空列表，前端照常渲染空态。
func (s *Server) handleCacheList(w http.ResponseWriter, r *http.Request) {
	items := []cache.Item{}
	total := int64(0)
	if s.Cache != nil {
		items = s.Cache.List()
		for _, it := range items {
			total += it.Size
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"count":      len(items),
		"total_size": total,
	})
}

// handleCacheDelete 批量删除指定 pickcode 的缓存项，返回实际删除数。
func (s *Server) handleCacheDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PickCodes []string `json:"pickcodes"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if len(req.PickCodes) == 0 {
		writeErr(w, http.StatusBadRequest, "未指定要删除的缓存项")
		return
	}
	deleted := 0
	if s.Cache != nil {
		deleted = s.Cache.Delete(req.PickCodes)
	}
	logs.Info(logs.ModuleSystem, "手动删除缓存完成", "请求", len(req.PickCodes), "删除", deleted)
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}
