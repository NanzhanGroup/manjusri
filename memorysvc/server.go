// Package memorysvc — 记忆服务（独占 SQLite，Unix Socket RPC）
//
// 与 sessionsvc 相同架构，统一管理 memory.db。
// chat、api-server、微信网关都通过本服务读写记忆，消除并发锁冲突。
package memorysvc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ── 默认清理配置（可通过环境变量覆盖） ──
const (
	defaultCleanupMaxAge   = 90 * 24 * time.Hour // 默认保留90天
	defaultCleanupInterval = 1 * time.Hour        // 每小时检查一次
)

// Server 记忆服务端
type Server struct {
	dataDir    string
	socketPath string
	db         *sql.DB
	mu         sync.RWMutex
}

// NewServer 创建记忆服务端
func NewServer(dataDir, socketPath string) *Server {
	return &Server{
		dataDir:    dataDir,
		socketPath: socketPath,
	}
}

// Start 启动服务（阻塞）
func (s *Server) Start() error {
	// 初始化 SQLite
	os.MkdirAll(s.dataDir, 0755)
	dbPath := filepath.Join(s.dataDir, "memory.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer db.Close()
	s.db = db
	db.SetMaxOpenConns(1)

	// 建表
	if err := s.initSchema(); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}

	// 确保 BASIC.MD 和 SECURITY.MD 文件存在
	s.ensureMDFile("BASIC.MD")
	s.ensureMDFile("SECURITY.MD")

	// 删除旧的 socket 文件
	os.Remove(s.socketPath)
	os.MkdirAll(filepath.Dir(s.socketPath), 0755)

	// 监听 Unix socket
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("监听 socket 失败 %s: %w", s.socketPath, err)
	}
	defer listener.Close()

	// 权限：所有用户可读写
	os.Chmod(s.socketPath, 0666)

	// 启动自动清理（后台 goroutine）
	s.startAutoCleanup()

	fmt.Printf("[memory-service] 已就绪: %s (data: %s)\n", s.socketPath, s.dataDir)

	// 注册路由
	mux := http.NewServeMux()
	mux.HandleFunc("/append", s.handleAppend)
	mux.HandleFunc("/append-with-opts", s.handleAppendWithOpts)
	mux.HandleFunc("/search-layers", s.handleSearchLayers)
	mux.HandleFunc("/search-all", s.handleSearchAll)
	mux.HandleFunc("/search-by-tag", s.handleSearchByTag)
	mux.HandleFunc("/count", s.handleCount)
	mux.HandleFunc("/cleanup", s.handleCleanup)
	mux.HandleFunc("/update-topic", s.handleUpdateTopic)
	mux.HandleFunc("/basic", s.handleBasic)
	mux.HandleFunc("/security", s.handleSecurity)

	// 会话管理路由
	mux.HandleFunc("/sessions/create", s.handleCreateSession)
	mux.HandleFunc("/sessions/update", s.handleUpdateSession)
	mux.HandleFunc("/sessions/get", s.handleGetSession)
	mux.HandleFunc("/sessions/list", s.handleListSessions)
	mux.HandleFunc("/sessions/delete", s.handleDeleteSession)
	mux.HandleFunc("/sessions/find-by-user", s.handleFindSessionByUser)
	mux.HandleFunc("/sessions/update-activity", s.handleUpdateSessionActivity)

	// 消息管理路由
	mux.HandleFunc("/messages/append", s.handleAppendMessage)
	mux.HandleFunc("/messages/list", s.handleListMessages)
	mux.HandleFunc("/messages/trim", s.handleTrimMessages)

	// 上下文拼装
	mux.HandleFunc("/context/build", s.handleBuildContext)

	// 智能评分
	mux.HandleFunc("/auto-score", s.handleAutoScore)

	// 记忆合并/总结
	mux.HandleFunc("/memories/merge-by-tag", s.handleMergeByTag)
	mux.HandleFunc("/memories/summarize", s.handleSummarizeMemories)

	// 线程管理路由
	mux.HandleFunc("/threads/close", s.handleThreadClose)
	mux.HandleFunc("/threads/list", s.handleThreadList)
	mux.HandleFunc("/threads/get", s.handleThreadGet)
	mux.HandleFunc("/threads/reactivate", s.handleThreadReactivate)
	mux.HandleFunc("/discard-epoch", s.handleDiscardEpoch)
	mux.HandleFunc("/discarded-msgids", s.handleDiscardedMsgIDs)

	return http.Serve(listener, mux)
}

func (s *Server) initSchema() error {
	createTable := `CREATE TABLE IF NOT EXISTS memories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		content TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
		tag TEXT DEFAULT '',
		importance INTEGER DEFAULT 0
	)`
	if _, err := s.db.Exec(createTable); err != nil {
		return err
	}

	// 会话元数据表
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL UNIQUE,
		title TEXT DEFAULT '',
		summary TEXT DEFAULT '',
		message_count INTEGER DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'active',
		created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
		tags TEXT DEFAULT ''
	)`)
	if err != nil {
		return err
	}

	// 消息流水表
	_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		role TEXT NOT NULL DEFAULT 'user',
		content TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now','localtime')),
		tokens INTEGER DEFAULT 0
	)`)
	if err != nil {
		return err
	}

	// ── 线程管理 ──
	_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS session_threads (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id  TEXT NOT NULL,
		title       TEXT DEFAULT '',
		status      TEXT NOT NULL DEFAULT 'active',
		start_msg_id  INTEGER DEFAULT NULL,
		end_msg_id    INTEGER DEFAULT NULL,
		superseded_by INTEGER DEFAULT NULL,
		summary     TEXT DEFAULT '',
		created_at  TEXT NOT NULL DEFAULT (datetime('now','localtime')),
		closed_at   TEXT DEFAULT NULL
	)`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_threads_session ON session_threads(session_id, status)`)
	if err != nil {
		return err
	}

	// 已作废消息记录表（用于翻篇作废）
	_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS discarded_messages (
		session_key TEXT NOT NULL,
		msg_id      TEXT NOT NULL,
		epoch_id    INTEGER NOT NULL,
		discarded_at TEXT NOT NULL,
		PRIMARY KEY (session_key, msg_id)
	)`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_discarded_session ON discarded_messages(session_key)`)
	if err != nil {
		return err
	}

	// messages 表增加 thread_id 字段（兼容已有数据库）
	_, err = s.db.Exec(`ALTER TABLE messages ADD COLUMN thread_id INTEGER DEFAULT NULL`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		// SQLite 的 ADD COLUMN 在列已存在时会报错，忽略
		return err
	}

	// messages 表增加 metadata 字段（兼容已有数据库）
	_, err = s.db.Exec(`ALTER TABLE messages ADD COLUMN metadata TEXT DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}

	// sessions 表增加 gateway_session 兼容字段（兼容已有数据库）
	alterColumns := []string{
		"ALTER TABLE sessions ADD COLUMN user_id TEXT DEFAULT ''",
		"ALTER TABLE sessions ADD COLUMN nick_name TEXT DEFAULT ''",
		"ALTER TABLE sessions ADD COLUMN platform TEXT DEFAULT ''",
		"ALTER TABLE sessions ADD COLUMN status TEXT NOT NULL DEFAULT 'active'",
		"ALTER TABLE sessions ADD COLUMN processed INTEGER DEFAULT 0",
		"ALTER TABLE sessions ADD COLUMN processed_at TEXT DEFAULT ''",
	}
	for _, col := range alterColumns {
		_, err = s.db.Exec(col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}

	// 索引
	indexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_memories_created ON memories(created_at)",
		"CREATE INDEX IF NOT EXISTS idx_tag ON memories(tag)",
		"CREATE INDEX IF NOT EXISTS idx_importance ON memories(importance)",
		"CREATE INDEX IF NOT EXISTS idx_sessions_session_id ON sessions(session_id)",
		"CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at)",
		"CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id)",
		"CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(created_at)",
	}
	for _, idx := range indexes {
		if _, err := s.db.Exec(idx); err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) ensureMDFile(name string) {
	path := filepath.Join(s.dataDir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.WriteFile(path, []byte(""), 0644)
	}
}

// ── HTTP 处理 ──

// handleAppend POST /append
// 写入记忆：过滤无意义 → 语义更新检测 → 去重 → AutoDetectTag → AutoScoreImportance → 入库
func (s *Server) handleAppend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.Content == "" {
		writeError(w, "content 不能为空", 400)
		return
	}

	// 过滤无意义内容
	if IsMeaningless(req.Content) {
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"skipped": true,
			"reason":  "无意义内容已跳过",
		})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// ═══ 步骤 1：语义更新检测 ═══
	// 检测新内容是否是对已有记忆的更新（如搬家换地址、换工作等）
	if shouldUpdate, oldID, _ := s.autoDetectUpdate(req.Content); shouldUpdate {
		tag := AutoDetectTag(req.Content)
		importance := AutoScoreImportance(req.Content)
		now := time.Now().Format("2006-01-02 15:04:05")

		_, err := s.db.Exec(
			"UPDATE memories SET content = ?, tag = ?, importance = ?, created_at = ? WHERE id = ?",
			req.Content, tag, importance, now, oldID,
		)
		if err != nil {
			writeError(w, "更新失败: "+err.Error(), 500)
			return
		}

		writeJSON(w, map[string]interface{}{
			"ok":         true,
			"skipped":    false,
			"updated":    true,
			"old_id":     oldID,
			"tag":        tag,
			"importance": importance,
			"reason":     "检测到「" + extractEntityType(req.Content) + "」信息更新",
		})
		return
	}

	// ═══ 步骤 2：全局去重 ═══
	if s.isDuplicate(req.Content) {
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"skipped": true,
			"reason":  "重复内容已跳过",
		})
		return
	}

	// ═══ 步骤 3：新增记忆 ═══
	tag := AutoDetectTag(req.Content)
	importance := AutoScoreImportance(req.Content)

	_, err := s.db.Exec(
		"INSERT INTO memories (content, tag, importance, created_at) VALUES (?, ?, ?, ?)",
		req.Content, tag, importance, time.Now().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		writeError(w, "写入失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":         true,
		"skipped":    false,
		"updated":    false,
		"tag":        tag,
		"importance": importance,
	})
}

// handleAppendWithOpts POST /append-with-opts
// 直接写入（跳过自动分类），支持指定 tag 和 importance
func (s *Server) handleAppendWithOpts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		Content    string `json:"content"`
		Tag        string `json:"tag"`
		Importance int    `json:"importance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.Content == "" {
		writeError(w, "content 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO memories (content, tag, importance, created_at) VALUES (?, ?, ?, ?)",
		req.Content, req.Tag, req.Importance, time.Now().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		writeError(w, "写入失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok": true,
	})
}

// handleSearchLayers GET /search-layers?query=xxx
// 分层搜索：今天 → 近7天 → 近30天 → 近90天，找到即止
func (s *Server) handleSearchLayers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, "query 参数不能为空", 400)
		return
	}

	s.mu.RLock()
	found, source, content := s.SearchLayers(query)
	s.mu.RUnlock()

	writeJSON(w, map[string]interface{}{
		"found":   found,
		"source":  source,
		"content": content,
	})
}

// handleSearchAll GET /search-all?query=xxx
// 搜索全部层级，返回所有层级的结果
func (s *Server) handleSearchAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, "query 参数不能为空", 400)
		return
	}

	s.mu.RLock()
	results := s.SearchAllLayers(query)
	s.mu.RUnlock()

	writeJSON(w, map[string]interface{}{
		"found":   len(results) > 0,
		"results": results,
	})
}

// handleSearchByTag GET /search-by-tag?tag=xxx&limit=20
// 按标签搜索记忆
func (s *Server) handleSearchByTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		writeError(w, "tag 参数不能为空", 400)
		return
	}
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		"SELECT content FROM memories WHERE tag = ? ORDER BY created_at DESC LIMIT ?",
		tag, limit,
	)
	if err != nil {
		writeError(w, "搜索失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			continue
		}
		results = append(results, content)
	}

	writeJSON(w, map[string]interface{}{
		"found":   len(results) > 0,
		"results": results,
	})
}

// handleCount GET /count
// 返回记忆总数
func (s *Server) handleCount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&count)
	if err != nil {
		writeError(w, "查询失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]int{"count": count})
}

// handleCleanup POST /cleanup
// 清理超过 90 天的记忆
func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -90).Format("2006-01-02 15:04:05")
	result, err := s.db.Exec("DELETE FROM memories WHERE created_at < ?", cutoff)
	if err != nil {
		writeError(w, "清理失败: "+err.Error(), 500)
		return
	}
	n, _ := result.RowsAffected()

	writeJSON(w, map[string]interface{}{
		"ok":     true,
		"deleted": n,
	})
}

// handleUpdateTopic POST /update-topic
// {keyword, new_content} 按关键字更新记忆内容
func (s *Server) handleUpdateTopic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		Keyword    string `json:"keyword"`
		NewContent string `json:"new_content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.Keyword == "" || req.NewContent == "" {
		writeError(w, "keyword 和 new_content 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	like := "%" + req.Keyword + "%"
	result, err := s.db.Exec(
		"UPDATE memories SET content = ? WHERE content LIKE ?",
		req.NewContent, like,
	)
	if err != nil {
		writeError(w, "更新失败: "+err.Error(), 500)
		return
	}
	n, _ := result.RowsAffected()

	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"updated": n,
	})
}

// handleBasic GET/POST /basic
// GET → 读取 BASIC.MD；POST → 写入 BASIC.MD
func (s *Server) handleBasic(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		defer s.mu.RUnlock()
		content := s.readFile(filepath.Join(s.dataDir, "BASIC.MD"))
		writeJSON(w, map[string]string{"content": content})

	case http.MethodPost:
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "请求体解析失败: "+err.Error(), 400)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := os.WriteFile(filepath.Join(s.dataDir, "BASIC.MD"), []byte(req.Content), 0644); err != nil {
			writeError(w, "写入失败: "+err.Error(), 500)
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})

	default:
		writeError(w, "仅支持 GET/POST", 405)
	}
}

// handleSecurity GET /security
// 读取 SECURITY.MD（只读，由系统管理）
func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	content := s.readFile(filepath.Join(s.dataDir, "SECURITY.MD"))
	writeJSON(w, map[string]string{"content": content})
}

// readFile 读取文件内容，文件不存在返回空字符串
func (s *Server) readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ── 会话管理 ──

// handleCreateSession POST /sessions/create
// 创建会话，返回 session_id
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Title     string `json:"title"`
		UserID    string `json:"user_id"`
		NickName  string `json:"nick_name"`
		Platform  string `json:"platform"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.SessionID == "" {
		writeError(w, "session_id 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Format("2006-01-02 15:04:05")
	_, err := s.db.Exec(
		"INSERT INTO sessions (session_id, title, user_id, nick_name, platform, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		req.SessionID, req.Title, req.UserID, req.NickName, req.Platform, now, now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeJSON(w, map[string]interface{}{
				"ok":         false,
				"error":      "session_id 已存在",
				"session_id": req.SessionID,
			})
			return
		}
		writeError(w, "创建会话失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":         true,
		"session_id": req.SessionID,
	})
}

// handleUpdateSession POST /sessions/update
// 更新会话标题/摘要/标签
func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		SessionID   string `json:"session_id"`
		Title       string `json:"title"`
		Summary     string `json:"summary"`
		Tags        string `json:"tags"`
		UserID      string `json:"user_id"`
		NickName    string `json:"nick_name"`
		Platform    string `json:"platform"`
		Processed   *bool  `json:"processed"`   // pointer to detect "not set"
		ProcessedAt string `json:"processed_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.SessionID == "" {
		writeError(w, "session_id 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// processed 字段处理：*bool → 整数值（0 或 1），nil 表示不更新
	var processedVal interface{}
	if req.Processed != nil {
		if *req.Processed {
			processedVal = 1
		} else {
			processedVal = 0
		}
	} else {
		processedVal = nil
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	result, err := s.db.Exec(
		`UPDATE sessions SET 
			title = CASE WHEN ? != '' THEN ? ELSE title END,
			summary = CASE WHEN ? != '' THEN ? ELSE summary END,
			tags = CASE WHEN ? != '' THEN ? ELSE tags END,
			user_id = CASE WHEN ? != '' THEN ? ELSE user_id END,
			nick_name = CASE WHEN ? != '' THEN ? ELSE nick_name END,
			platform = CASE WHEN ? != '' THEN ? ELSE platform END,
			processed = CASE WHEN ? IS NOT NULL THEN ? ELSE processed END,
			processed_at = CASE WHEN ? != '' THEN ? ELSE processed_at END,
			updated_at = ?
		WHERE session_id = ?`,
		req.Title, req.Title,
		req.Summary, req.Summary,
		req.Tags, req.Tags,
		req.UserID, req.UserID,
		req.NickName, req.NickName,
		req.Platform, req.Platform,
		processedVal, processedVal,
		req.ProcessedAt, req.ProcessedAt,
		now, req.SessionID,
	)
	if err != nil {
		writeError(w, "更新会话失败: "+err.Error(), 500)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeJSON(w, map[string]interface{}{
			"ok":    false,
			"error": "session not found",
		})
		return
	}

	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleGetSession GET /sessions/get?session_id=xxx
// 获取会话元数据
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, "session_id 参数不能为空", 400)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var sess Session
	var processedInt int
	err := s.db.QueryRow(
		"SELECT session_id, title, summary, message_count, COALESCE(status,'active'), created_at, updated_at, tags, COALESCE(user_id,''), COALESCE(nick_name,''), COALESCE(platform,''), COALESCE(processed,0), COALESCE(processed_at,'') FROM sessions WHERE session_id = ?",
		sessionID,
	).Scan(&sess.SessionID, &sess.Title, &sess.Summary, &sess.MessageCount, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt, &sess.Tags, &sess.UserID, &sess.NickName, &sess.Platform, &processedInt, &sess.ProcessedAt)
	sess.Processed = processedInt == 1
	if err == sql.ErrNoRows {
		writeJSON(w, map[string]interface{}{
			"ok":    false,
			"error": "session not found",
		})
		return
	}
	if err != nil {
		writeError(w, "查询会话失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"session": sess,
	})
}

// handleListSessions GET /sessions/list?limit=20&offset=0
// 列出最近会话（按 updated_at DESC）
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}

	limit := 20
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		fmt.Sscanf(o, "%d", &offset)
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		"SELECT session_id, title, summary, message_count, COALESCE(status,'active'), created_at, updated_at, tags, COALESCE(user_id,''), COALESCE(nick_name,''), COALESCE(platform,''), COALESCE(processed,0), COALESCE(processed_at,'') FROM sessions ORDER BY updated_at DESC LIMIT ? OFFSET ?",
		limit, offset,
	)
	if err != nil {
		writeError(w, "查询会话列表失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	sessions := make([]Session, 0)
	for rows.Next() {
		var sess Session
		var processedInt int
		if err := rows.Scan(&sess.SessionID, &sess.Title, &sess.Summary, &sess.MessageCount, &sess.Status, &sess.CreatedAt, &sess.UpdatedAt, &sess.Tags, &sess.UserID, &sess.NickName, &sess.Platform, &processedInt, &sess.ProcessedAt); err != nil {
			continue
		}
		sess.Processed = processedInt == 1
		sessions = append(sessions, sess)
	}

	writeJSON(w, map[string]interface{}{
		"ok":       true,
		"sessions": sessions,
	})
}

// handleDeleteSession DELETE /sessions/delete?session_id=xxx
// 删除会话及其所有消息（级联删除）
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, "仅支持 DELETE", 405)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, "session_id 参数不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 先删除会话的所有消息
	if _, err := s.db.Exec("DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
		writeError(w, "删除消息失败: "+err.Error(), 500)
		return
	}

	// 再删除会话
	result, err := s.db.Exec("DELETE FROM sessions WHERE session_id = ?", sessionID)
	if err != nil {
		writeError(w, "删除会话失败: "+err.Error(), 500)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeJSON(w, map[string]interface{}{
			"ok":    false,
			"error": "session not found",
		})
		return
	}

	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleFindSessionByUser GET /sessions/find-by-user?platform=xxx&user_id=xxx
// 按平台+用户查找活跃会话，返回最近的活跃会话
func (s *Server) handleFindSessionByUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}
	userID := r.URL.Query().Get("user_id")
	platform := r.URL.Query().Get("platform")
	if userID == "" || platform == "" {
		writeError(w, "user_id 和 platform 不能为空", 400)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// 查找该用户最近的活跃会话
	var sess Session
	var processedInt int
	err := s.db.QueryRow(
		`SELECT session_id, COALESCE(title,''), COALESCE(summary,''), message_count, 
		        COALESCE(status,'active'), created_at, updated_at, COALESCE(tags,''), 
		        COALESCE(user_id,''), COALESCE(nick_name,''), COALESCE(platform,''),
		        COALESCE(processed,0), COALESCE(processed_at,'')
		 FROM sessions 
		 WHERE user_id=? AND platform=? AND status='active' 
		 ORDER BY updated_at DESC LIMIT 1`,
		userID, platform,
	).Scan(&sess.SessionID, &sess.Title, &sess.Summary, &sess.MessageCount,
		&sess.Status, &sess.CreatedAt, &sess.UpdatedAt, &sess.Tags,
		&sess.UserID, &sess.NickName, &sess.Platform,
		&processedInt, &sess.ProcessedAt)
	sess.Processed = processedInt == 1

	if err == sql.ErrNoRows {
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"found":   false,
			"session": nil,
		})
		return
	}
	if err != nil {
		writeError(w, "查询失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"found":   true,
		"session": sess,
	})
}

// handleUpdateSessionActivity POST /sessions/update-activity
// 更新会话的活动时间和状态
func (s *Server) handleUpdateSessionActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Status    string `json:"status,omitempty"` // 可选：更新状态
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.SessionID == "" {
		writeError(w, "session_id 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Format("2006-01-02 15:04:05")
	var result sql.Result
	var err error
	if req.Status != "" {
		result, err = s.db.Exec(
			"UPDATE sessions SET updated_at=?, status=? WHERE session_id=?",
			now, req.Status, req.SessionID,
		)
	} else {
		result, err = s.db.Exec(
			"UPDATE sessions SET updated_at=? WHERE session_id=?",
			now, req.SessionID,
		)
	}
	if err != nil {
		writeError(w, "更新失败: "+err.Error(), 500)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeJSON(w, map[string]interface{}{
			"ok":    false,
			"error": "session not found",
		})
		return
	}

	writeJSON(w, map[string]interface{}{"ok": true})
}

// ── 消息管理 ──

// handleAppendMessage POST /messages/append
// 追加消息到会话，自动递增 session 的 message_count
func (s *Server) handleAppendMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Role      string `json:"role"`
		Content   string `json:"content"`
		Tokens    int    `json:"tokens"`
		Metadata  string `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.SessionID == "" || req.Content == "" {
		writeError(w, "session_id 和 content 不能为空", 400)
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 检查会话是否存在
	var exists int
	err := s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE session_id = ?", req.SessionID).Scan(&exists)
	if err != nil || exists == 0 {
		writeJSON(w, map[string]interface{}{
			"ok":    false,
			"error": "session not found",
		})
		return
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	result, err := s.db.Exec(
		"INSERT INTO messages (session_id, role, content, tokens, metadata, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		req.SessionID, req.Role, req.Content, req.Tokens, req.Metadata, now,
	)
	if err != nil {
		writeError(w, "追加消息失败: "+err.Error(), 500)
		return
	}

	msgID, _ := result.LastInsertId()

	// 确保会话有 active 线程，并将消息绑定到该线程
	activeThreadID, err := s.ensureSessionHasActiveThread(req.SessionID)
	if err == nil && activeThreadID > 0 {
		s.updateMessageThreadID(msgID, activeThreadID)
	}

	// 作废指令检测：在话题边界检测之前执行
	obsoleteDetected := false
	obsoleteAction := ""
	obsoleteReason := ""

	// 话题边界检测：检测用户消息是否触发话题切换
	threadSwitched := false
	newThreadID := int64(0)
	if req.Role == "user" {
		// 先检测作废指令
		matched, action, keyword := matchObsoleteCommand(req.Content)
		if matched {
			obsoleteDetected = true
			obsoleteAction = action
			switch action {
			case "close_current":
				obsoleteReason = keyword
				newThreadID, threadSwitched, err = s.autoSwitchThread(req.SessionID, req.Content)
				if err == nil && threadSwitched && newThreadID > 0 {
					s.updateMessageThreadID(msgID, newThreadID)
					activeThreadID = newThreadID
				}
			case "close_topic":
				obsoleteReason = keyword
				n, _ := s.closeThreadsByKeyword(req.SessionID, keyword)
				if n > 0 {
					// 关闭后创建新线程
					newThreadID, threadSwitched, err = s.autoSwitchThread(req.SessionID, keyword+" 已关闭")
					if err == nil && threadSwitched && newThreadID > 0 {
						s.updateMessageThreadID(msgID, newThreadID)
						activeThreadID = newThreadID
					}
				}
			}
		}

		// 未匹配作废指令时，执行话题边界检测
		if !matched {
			isNewTopic, _ := detectTopicBoundary(req.Content)
			if isNewTopic {
				newThreadID, threadSwitched, err = s.autoSwitchThread(req.SessionID, req.Content)
				if err == nil && threadSwitched && newThreadID > 0 {
					s.updateMessageThreadID(msgID, newThreadID)
					activeThreadID = newThreadID
				}
			}
		}
	}

	// 递增会话消息计数 + 更新 updated_at
	s.db.Exec(
		"UPDATE sessions SET message_count = message_count + 1, updated_at = ? WHERE session_id = ?",
		now, req.SessionID,
	)

	writeJSON(w, map[string]interface{}{
		"ok":                true,
		"id":                msgID,
		"thread_id":         activeThreadID,
		"thread_switched":   threadSwitched,
		"new_thread_id":     newThreadID,
		"obsolete_detected": obsoleteDetected,
		"obsolete_action":   obsoleteAction,
		"obsolete_reason":   obsoleteReason,
	})
}

// handleListMessages GET /messages/list?session_id=xxx&limit=50&before_id=999
// 列出会话的消息（按 created_at DESC，支持 before_id 游标分页）
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, "session_id 参数不能为空", 400)
		return
	}

	limit := 50
	beforeID := int64(0)
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	if b := r.URL.Query().Get("before_id"); b != "" {
		fmt.Sscanf(b, "%d", &beforeID)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = s.db.Query(
			"SELECT id, session_id, role, content, created_at, tokens, COALESCE(metadata,'') FROM messages WHERE session_id = ? AND id < ? ORDER BY id DESC LIMIT ?",
			sessionID, beforeID, limit,
		)
	} else {
		rows, err = s.db.Query(
			"SELECT id, session_id, role, content, created_at, tokens, COALESCE(metadata,'') FROM messages WHERE session_id = ? ORDER BY id DESC LIMIT ?",
			sessionID, limit,
		)
	}
	if err != nil {
		writeError(w, "查询消息失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	messages := make([]Message, 0)
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Role, &msg.Content, &msg.CreatedAt, &msg.Tokens, &msg.Metadata); err != nil {
			continue
		}
		messages = append(messages, msg)
	}

	writeJSON(w, map[string]interface{}{
		"ok":       true,
		"messages": messages,
	})
}

// handleTrimMessages POST /messages/trim
// 按 token 预算截断早期消息，自动生成摘要
func (s *Server) handleTrimMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.SessionID == "" {
		writeError(w, "session_id 不能为空", 400)
		return
	}
	if req.MaxTokens <= 0 || req.MaxTokens > 128000 {
		req.MaxTokens = 4096
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 查询所有消息（按 id 正序）
	rows, err := s.db.Query(
		"SELECT id, role, content, tokens FROM messages WHERE session_id = ? ORDER BY id ASC",
		req.SessionID,
	)
	if err != nil {
		writeError(w, "查询消息失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	type msg struct {
		id      int64
		role    string
		content string
		tokens  int
	}
	var allMsgs []msg
	var totalTokens int
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.id, &m.role, &m.content, &m.tokens); err != nil {
			continue
		}
		allMsgs = append(allMsgs, m)
		totalTokens += m.tokens
	}

	if totalTokens <= req.MaxTokens || len(allMsgs) <= 2 {
		writeJSON(w, map[string]interface{}{
			"ok":            true,
			"trimmed_count": 0,
			"summary":       "",
			"reason":        "未超出预算或消息过少，跳过截断",
		})
		return
	}

	// 从前往后删除，直到总 token 数 ≤ max_tokens（至少保留 2 条消息）
	var trimmedMsgs []msg
	var trimmedTokens int
	for len(allMsgs) > 2 && totalTokens-trimmedTokens > req.MaxTokens {
		trimmedMsgs = append(trimmedMsgs, allMsgs[0])
		trimmedTokens += allMsgs[0].tokens
		allMsgs = allMsgs[1:]
	}

	// 生成摘要：取被截断消息的 role+content 前 80 字拼接
	var summaryParts []string
	for _, m := range trimmedMsgs {
		label := "用户"
		if m.role == "assistant" {
			label = "AI"
		} else if m.role == "system" {
			label = "系统"
		}
		text := m.content
		runes := []rune(text)
		if len(runes) > 80 {
			text = string(runes[:80])
		}
		summaryParts = append(summaryParts, label+": "+text)
	}
	summary := strings.Join(summaryParts, "\n")

	// 删除被截断的消息
	for _, m := range trimmedMsgs {
		s.db.Exec("DELETE FROM messages WHERE id = ?", m.id)
	}

	// 更新 session 的 summary
	s.db.Exec(
		"UPDATE sessions SET summary = ?, updated_at = datetime('now','localtime') WHERE session_id = ?",
		summary, req.SessionID,
	)

	writeJSON(w, map[string]interface{}{
		"ok":            true,
		"trimmed_count": len(trimmedMsgs),
		"summary":       summary,
		"remaining":     len(allMsgs),
	})
}

// ── 上下文拼装 ──

// handleBuildContext POST /context/build
// 拼装三段信息：[系统身份] + [长期记忆] + [会话历史]
func (s *Server) handleBuildContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		SessionID       string `json:"session_id"`
		Query           string `json:"query"`
		MaxMessages     int    `json:"max_messages"`
		MaxMemories     int    `json:"max_memories"`
		IncludeObsolete bool   `json:"include_obsolete"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.SessionID == "" {
		writeError(w, "session_id 不能为空", 400)
		return
	}
	if req.MaxMessages <= 0 || req.MaxMessages > 100 {
		req.MaxMessages = 20
	}
	if req.MaxMemories <= 0 || req.MaxMemories > 50 {
		req.MaxMemories = 5
	}

	// 1. 读取 BASIC.MD + SECURITY.MD 合成 system_prompt
	basicContent := s.readFile(filepath.Join(s.dataDir, "BASIC.MD"))
	securityContent := s.readFile(filepath.Join(s.dataDir, "SECURITY.MD"))
	systemPrompt := ""
	if basicContent != "" {
		systemPrompt += basicContent + "\n"
	}
	if securityContent != "" {
		systemPrompt += securityContent
	}
	systemPrompt = strings.TrimSpace(systemPrompt)

	// 2. 按 query 搜索长期记忆
	var memoriesText string
	var memorySource string
	memoryCount := 0
	threadActiveCount := 0
	threadObsoleteCount := 0
	if req.Query != "" {
		s.mu.RLock()
		found, source, content := s.SearchLayers(req.Query)
		s.mu.RUnlock()
		if found {
			memorySource = source
			// 按行切割，每行一条记忆
			lines := strings.Split(content, "\n")
			var items []string
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line != "" {
					items = append(items, "- "+line)
				}
			}
			memoryCount = len(items)
			if memoryCount > req.MaxMemories {
				items = items[:req.MaxMemories]
				memoryCount = len(items)
			}
			memoriesText = "📌 长期记忆（来自 " + source + "）：\n" + strings.Join(items, "\n")
		}
	}

	// 3. 查询线程统计信息
	s.mu.RLock()
	s.db.QueryRow("SELECT COUNT(*) FROM session_threads WHERE session_id=? AND status='active'", req.SessionID).Scan(&threadActiveCount)
	s.db.QueryRow("SELECT COUNT(*) FROM session_threads WHERE session_id=? AND status='obsolete'", req.SessionID).Scan(&threadObsoleteCount)
	s.mu.RUnlock()

	// 4. 里程碑摘要：将已作废线程的摘要拼接到 system_prompt
	if !req.IncludeObsolete {
		s.mu.RLock()
		milestonesText, _ := s.getObsoleteSummaries(req.SessionID)
		s.mu.RUnlock()
		if milestonesText != "" {
			systemPrompt += "\n\n" + milestonesText
		}
	}

	// 5. 从 messages 表取最近 N 条消息
	var historyText string
	historyCount := 0
	s.mu.RLock()
	var rows *sql.Rows
	var err error
	if !req.IncludeObsolete {
		// 仅加载 active 线程的消息（兼容 thread_id IS NULL 的老消息）
		rows, err = s.db.Query(
			`SELECT m.role, m.content, m.created_at FROM messages m
			 WHERE m.session_id = ? AND (
			   m.thread_id IN (SELECT id FROM session_threads WHERE session_id=? AND status='active')
			   OR m.thread_id IS NULL
			 )
			 ORDER BY m.id DESC LIMIT ?`,
			req.SessionID, req.SessionID, req.MaxMessages,
		)
	} else {
		// 加载所有消息（原逻辑）
		rows, err = s.db.Query(
			"SELECT role, content, created_at FROM messages WHERE session_id = ? ORDER BY id DESC LIMIT ?",
			req.SessionID, req.MaxMessages,
		)
	}
	s.mu.RUnlock()

	if err == nil {
		defer rows.Close()
		type msg struct {
			role      string
			content   string
			createdAt string
		}
		var msgs []msg
		for rows.Next() {
			var m msg
			if err := rows.Scan(&m.role, &m.content, &m.createdAt); err != nil {
				continue
			}
			msgs = append(msgs, m)
		}
		// 反转为时间正序
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
		historyCount = len(msgs)
		var parts []string
		for _, m := range msgs {
			roleLabel := "用户"
			if m.role == "assistant" {
				roleLabel = "AI"
			} else if m.role == "system" {
				roleLabel = "系统"
			}
			// 截取时间中的 HH:MM
			timeStr := m.createdAt
			if len(timeStr) >= 16 {
				timeStr = timeStr[11:16]
			}
			parts = append(parts, fmt.Sprintf("%s (%s): %s", roleLabel, timeStr, m.content))
		}
		historyText = strings.Join(parts, "\n")
	}

	writeJSON(w, map[string]interface{}{
		"ok": true,
		"context": map[string]interface{}{
			"system_prompt": systemPrompt,
			"memories":      memoriesText,
			"history":       historyText,
		},
		"meta": map[string]interface{}{
			"memory_count":        memoryCount,
			"history_count":       historyCount,
			"memory_source":       memorySource,
			"thread_active_count": threadActiveCount,
			"thread_obsolete_count": threadObsoleteCount,
		},
	})
}

// ── 智能评分 ──

// handleAutoScore POST /auto-score
// 评估一段文本的标签、重要性、置信度
func (s *Server) handleAutoScore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.Content == "" {
		writeError(w, "content 不能为空", 400)
		return
	}

	// 计算该内容在 memories 表中出现的次数（用于置信度评分）
	s.mu.RLock()
	var occurrenceCount int
	s.db.QueryRow("SELECT COUNT(*) FROM memories WHERE content = ?", req.Content).Scan(&occurrenceCount)
	s.mu.RUnlock()

	tag := AutoDetectTag(req.Content)
	importance := AutoScoreImportance(req.Content)
	confidence := AutoScoreConfidence(req.Content, occurrenceCount)

	writeJSON(w, map[string]interface{}{
		"ok":         true,
		"tag":        tag,
		"importance": importance,
		"confidence": confidence,
	})
}

// ── 记忆合并/总结 ──

// handleMergeByTag POST /memories/merge-by-tag
// 按标签合并同主题记忆
func (s *Server) handleMergeByTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		Tag        string `json:"tag"`
		MaxAgeDays int    `json:"max_age_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.Tag == "" {
		writeError(w, "tag 不能为空", 400)
		return
	}
	if req.MaxAgeDays <= 0 || req.MaxAgeDays > 365 {
		req.MaxAgeDays = 30
	}

	cutoff := time.Now().AddDate(0, 0, -req.MaxAgeDays).Format("2006-01-02 15:04:05")

	s.mu.Lock()
	defer s.mu.Unlock()

	// 查询该标签下所有记忆
	rows, err := s.db.Query(
		"SELECT id, content FROM memories WHERE tag = ? AND created_at >= ? ORDER BY id DESC",
		req.Tag, cutoff,
	)
	if err != nil {
		writeError(w, "查询记忆失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	type rec struct {
		id      int64
		content string
	}
	var records []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.content); err != nil {
			continue
		}
		records = append(records, r)
	}

	if len(records) < 3 {
		writeJSON(w, map[string]interface{}{
			"ok":           true,
			"merged_count": 0,
			"result":       "",
			"reason":       fmt.Sprintf("记忆数 %d < 3，跳过合并", len(records)),
		})
		return
	}

	// 去重合并内容
	seen := make(map[string]bool)
	var uniqueLines []string
	maxImportance := 0
	for _, rec := range records {
		line := strings.TrimSpace(rec.content)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		uniqueLines = append(uniqueLines, line)
		// 取重要性最大值
		imp := AutoScoreImportance(line)
		if imp > maxImportance {
			maxImportance = imp
		}
	}

	mergedText := strings.Join(uniqueLines, "\n")

	// 删除旧碎片
	for _, rec := range records {
		s.db.Exec("DELETE FROM memories WHERE id = ?", rec.id)
	}

	// 写入合并结果
	s.db.Exec(
		"INSERT INTO memories (content, tag, importance, created_at) VALUES (?, ?, ?, ?)",
		mergedText, req.Tag, maxImportance, time.Now().Format("2006-01-02 15:04:05"),
	)

	writeJSON(w, map[string]interface{}{
		"ok":           true,
		"merged_count": len(records),
		"result":       mergedText,
	})
}

// handleSummarizeMemories POST /memories/summarize
// 对旧记忆做精简
func (s *Server) handleSummarizeMemories(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		MaxDays     int `json:"max_days"`
		MaxMemories int `json:"max_memories"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.MaxDays <= 0 || req.MaxDays > 365 {
		req.MaxDays = 60
	}
	if req.MaxMemories <= 0 || req.MaxMemories > 1000 {
		req.MaxMemories = 100
	}

	cutoff := time.Now().AddDate(0, 0, -req.MaxDays).Format("2006-01-02 15:04:05")

	s.mu.Lock()
	defer s.mu.Unlock()

	// 查询超过截止时间的记忆，按 tag 分组
	rows, err := s.db.Query(
		"SELECT id, tag, content, importance FROM memories WHERE created_at < ? ORDER BY tag, created_at DESC",
		cutoff,
	)
	if err != nil {
		writeError(w, "查询记忆失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	type rec struct {
		id         int64
		tag        string
		content    string
		importance int
	}
	var allRecs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.tag, &r.content, &r.importance); err != nil {
			continue
		}
		allRecs = append(allRecs, r)
	}

	// 按 tag 分组，每组保留最新的 3 条
	tagGroups := make(map[string][]rec)
	for _, r := range allRecs {
		tagGroups[r.tag] = append(tagGroups[r.tag], r)
	}

	totalDeleted := 0
	totalKept := 0
	for tag, group := range tagGroups {
		if len(group) <= 3 {
			totalKept += len(group)
			continue
		}
		// 保留前 3 条（已按 created_at DESC 排序）
		keep := group[:3]
		delete := group[3:]

		for _, d := range delete {
			s.db.Exec("DELETE FROM memories WHERE id = ?", d.id)
			totalDeleted++
		}
		totalKept += len(keep)
		_ = tag // tag 用于分组标识
	}

	writeJSON(w, map[string]interface{}{
		"ok":            true,
		"deleted_count": totalDeleted,
		"kept_count":    totalKept,
	})
}

// ── 业务逻辑函数 ──

// AutoDetectTag 自动分类记忆标签
// 分类：emotion（情感/情绪）、growth（成长/觉醒）、insight（洞见/领悟）
//        profile（个人设定）、preference（偏好）、note（笔记）、knowledge（知识）
// 让记忆系统从"记事实"变成"记人心"——优先捕捉情绪、成长和洞见
func AutoDetectTag(content string) string {
	content = strings.ToLower(content)

	// emotion — 情感/情绪（优先级最高，因为记的是人心）
	emotionKeywords := []string{
		"难过", "伤心", "开心", "快乐", "痛苦", "焦虑", "害怕", "孤独",
		"感动", "温暖", "失落", "迷茫", "困惑", "空虚", "满足", "平静",
		"愤怒", "委屈", "不安", "期待", "释然", "感恩", "幸福",
		"心里", "心情", "感觉", "感受", "情绪",
		"苦", "累", "痛", "怕",
	}
	for _, kw := range emotionKeywords {
		if strings.Contains(content, kw) {
			return "emotion"
		}
	}

	// growth — 成长/觉醒（第二优先级）
	growthKeywords := []string{
		"悟了", "明白了", "懂了", "觉醒", "成长", "改变", "放下",
		"接受", "释怀", "看开", "想通", "领悟", "觉悟",
		"自在", "自省", "觉察", "接纳", "修行",
	}
	for _, kw := range growthKeywords {
		if strings.Contains(content, kw) {
			return "growth"
		}
	}

	// insight — 洞见/领悟
	insightKeywords := []string{
		"发现", "意识到", "认识到", "原来", "其实",
		"对我来说", "我的理解", "我的体会",
	}
	for _, kw := range insightKeywords {
		if strings.Contains(content, kw) {
			return "insight"
		}
	}

	// profile — 个人身份/设定
	profileKeywords := []string{
		"我叫", "我是", "我的名字", "我来自", "我住在", "我今年", "我的职业",
		"我是个", "我是名", "我的生日", "我的性别", "我的家乡",
	}
	for _, kw := range profileKeywords {
		if strings.Contains(content, kw) {
			return "profile"
		}
	}

	// preference — 偏好/喜好
	prefKeywords := []string{
		"我喜欢", "我不喜欢", "我爱", "我讨厌", "我最爱", "我偏爱",
		"我习惯", "我的爱好", "我的兴趣", "我想吃", "我想去",
	}
	for _, kw := range prefKeywords {
		if strings.Contains(content, kw) {
			return "preference"
		}
	}

	// config — 配置/设置
	configKeywords := []string{
		"设置", "配置", "开启", "关闭", "启用", "禁用",
		"设为", "改成", "调整为", "默认",
	}
	for _, kw := range configKeywords {
		if strings.Contains(content, kw) {
			return "config"
		}
	}

	// knowledge — 知识/信息（含"是"的陈述句、含专业术语）
	knowledgePatterns := []string{
		"是指", "指的是", "意思是", "定义为", "又称",
		"是一种", "是一个", "属于", "位于", "成立于",
	}
	for _, kw := range knowledgePatterns {
		if strings.Contains(content, kw) {
			return "knowledge"
		}
	}

	return "note"
}

// AutoScoreImportance 自动评分记忆重要性（0/1/2）
// 把"记人心"放在第一位：情绪表达、成长感悟、个人经历 → 高重要性
func AutoScoreImportance(content string) int {
	content = strings.ToLower(content)

	// 高分（2）：强烈情感、生命感悟、重要人生变化
	highScore := []string{
		"非常重要", "极其重要", "绝对", "必须", "务必",
		"永远", "始终", "核心", "关键", "根本",
		"最爱", "最讨厌", "不能", "禁止", "重要",
		"必须记住", "重中之重",
		// 情绪高分
		"痛苦", "绝望", "崩溃", "害怕", "恐惧",
		"感动", "幸福", "温暖", "感恩", "释然",
		"觉醒", "悟了", "放下",
	}
	for _, kw := range highScore {
		if strings.Contains(content, kw) {
			return 2
		}
	}

	// 中分（1）：情绪表达、个人成长、一般信息
	mediumScore := []string{
		// 情绪类
		"难过", "开心", "快乐", "焦虑", "孤独", "失落", "迷茫", "困惑",
		"委屈", "不安", "期待", "平静", "空虚", "满足",
		"感觉", "感受", "心情", "心里", "情绪",
		// 成长类
		"明白了", "懂了", "改变", "接受", "释怀", "看开", "想通",
		"领悟", "觉悟", "成长", "自省", "觉察", "接纳", "修行",
		"自在", "意识到", "认识到", "发现",
		// 一般重要
		"是", "有", "在", "可以", "需要",
		"觉得", "认为", "想", "要", "会",
		"请记住", "别忘了", "记住", "重点",
	}

	// 个人信息关键词 → 重要性+1
	profilePatterns := []string{"我叫", "我是", "我的", "我住在", "我今年", "电话", "邮箱"}
	for _, p := range profilePatterns {
		if strings.Contains(content, p) {
			return 1
		}
	}

	for _, kw := range mediumScore {
		if strings.Contains(content, kw) {
			return 1
		}
	}

	// 低分（0）：默认
	return 0
}

// AutoScoreConfidence 评估一条记忆的置信度（0~5）
// 场景：从对话中自动提炼的记忆，置信度取决于来源、重复次数等
func AutoScoreConfidence(content string, occurrenceCount int) int {
	score := 0

	// 用户明确要求记住 → 高置信度
	if strings.Contains(content, "请记住") || strings.Contains(content, "务必记住") {
		score += 2
	}

	// 包含具体事实（数字、日期、专有名词）→ 较高置信度
	if containsConcreteInfo(content) {
		score += 1
	}

	// 多次提及（occurrenceCount >= 2）→ 加置信度
	if occurrenceCount >= 2 {
		score += 1
	}
	if occurrenceCount >= 5 {
		score += 1
	}

	// 上限 5
	if score > 5 {
		score = 5
	}
	return score
}

// containsConcreteInfo 判断是否包含具体信息：数字、日期、专有名词特征
func containsConcreteInfo(s string) bool {
	hasNumber := regexp.MustCompile(`\d+`).MatchString(s)
	hasDate := regexp.MustCompile(`\d{4}[-/年]\d{1,2}[-/月]\d{1,2}`).MatchString(s)
	return hasNumber || hasDate
}

// IsMeaningless 判断内容是否无意义（返回 true 表示无意义，应跳过）
func IsMeaningless(content string) bool {
	if len(strings.TrimSpace(content)) < 3 {
		return true
	}

	// 纯标点符号或特殊字符
	trimmed := strings.TrimSpace(content)
	re := regexp.MustCompile(`^[\s\p{P}\p{S}]+$`)
	if re.MatchString(trimmed) {
		return true
	}

	// 纯数字
	reDigits := regexp.MustCompile(`^\d+$`)
	if reDigits.MatchString(trimmed) {
		return true
	}

	// 单字重复（如：哈哈哈、啊啊啊）
	if len(trimmed) >= 3 {
		allSame := true
		for i := 1; i < len(trimmed); i++ {
			if trimmed[i] != trimmed[0] {
				allSame = false
				break
			}
		}
		if allSame {
			return true
		}
	}

	// 常见的无意义填充词（保留单字回应如"嗯""好""对"等简短互动）
	meaninglessPhrases := []string{
		"test", "测试", "121", "asdf",
	}
	for _, phrase := range meaninglessPhrases {
		if strings.TrimSpace(content) == phrase {
			return true
		}
	}

	return false
}

// isDuplicate 检查 content 是否已存在于数据库中
func (s *Server) isDuplicate(content string) bool {
	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM memories WHERE content = ?", content,
	).Scan(&count)
	if err != nil {
		return false
	}
	return count > 0
}

// ── 语义更新检测 ──

// entityPatterns 可更新的属性实体模式
// key=实体类型, values=触发关键词（新内容中包含即认为涉及该实体）
var entityPatterns = map[string][]string{
	"地址":   {"搬到", "搬家", "住址", "地址", "家住", "住在", "迁到", "新家", "家庭地址", "家庭住址", "小区", "住宅"},
	"工作":   {"换工作", "跳槽", "新公司", "现在在", "入职", "离职", "上班", "工作单位", "职位", "就职", "工作在", "任职", "开发", "工程师", "后端", "前端", "全栈", "产品经理", "设计师", "运维", "算法", "测试"},
	"电话":   {"换号", "新手机", "新电话", "手机号", "电话号码", "联系方式", "电话", "手机", "联系"},
	"邮箱":   {"新邮箱", "换邮箱", "邮箱地址", "电子邮件", "邮箱", "邮件"},
	"姓名":   {"改名为", "现在叫", "新名字", "改名", "姓名", "名字", "称呼", "昵称"},
	"学校":   {"转学", "新学校", "考上", "入学", "就读", "学校", "毕业", "大学", "学院", "专业"},
	"生日":   {"生日", "出生日期", "出生", "岁数", "年龄"},
	"关系":   {"分手", "结婚", "离婚", "新对象", "女朋友", "男朋友", "配偶", "伴侣", "妻子", "丈夫", "老婆", "老公"},
	"宠物":   {"新宠物", "养了", "买了只", "领养", "宠物", "狗", "猫"},
	"爱好":   {"新爱好", "开始喜欢", "最近迷上", "兴趣", "爱好", "喜欢", "爱玩"},
	"车辆":   {"新车", "换车", "买了车", "车牌", "座驾", "车子", "汽车"},
	"账户":   {"新账号", "换账号", "用户名", "账户", "账号", "密码"},
}

// extractEntityType 从内容中提取涉及的属性实体类型
// 返回实体类型字符串，未匹配返回 ""
func extractEntityType(content string) string {
	contentLower := strings.ToLower(content)
	for entityType, patterns := range entityPatterns {
		for _, p := range patterns {
			if strings.Contains(contentLower, p) {
				return entityType
			}
		}
	}
	return ""
}

// extractSearchKeywords 从内容中提取用于搜索旧记忆的关键词
// 优先提取实体关键词，再补充其他显著名词
func extractSearchKeywords(content string) []string {
	keywords := make(map[string]bool)

	// 1. 实体模式关键词
	for _, patterns := range entityPatterns {
		for _, p := range patterns {
			if strings.Contains(content, p) {
				keywords[p] = true
			}
		}
	}

	// 2. 如果没匹配到实体，提取内容前 10 个字作为搜索词
	if len(keywords) == 0 {
		runes := []rune(strings.TrimSpace(content))
		if len(runes) > 10 {
			keywords[string(runes[:10])] = true
		} else if len(runes) > 3 {
			keywords[string(runes)] = true
		}
	}

	result := make([]string, 0, len(keywords))
	for k := range keywords {
		result = append(result, k)
	}
	return result
}

// autoDetectUpdate 检测新内容是否是对已有记忆的更新
// 返回：(是否更新, 旧记忆ID, 新内容)
// 如果检测到更新，调用方应 UPDATE 旧记录而非 INSERT 新记录
func (s *Server) autoDetectUpdate(newContent string) (shouldUpdate bool, oldID int64, newContentOut string) {
	// 1. 提取新内容的实体类型
	entityType := extractEntityType(newContent)
	if entityType == "" {
		return false, 0, ""
	}

	// 2. 用该实体类型的**全部模式关键词**搜索已有记忆
	//    例如新内容有"搬到"→实体类型"地址"→搜索"搬到/搬家/地址/住址/家住…"所有模式
	//    这样旧内容的"地址"也能被命中
	patterns := entityPatterns[entityType]
	if len(patterns) == 0 {
		return false, 0, ""
	}

	whereClause := ""
	args := []interface{}{}
	for i, p := range patterns {
		if i > 0 {
			whereClause += " OR "
		}
		whereClause += "content LIKE ?"
		args = append(args, "%"+p+"%")
	}

	query := "SELECT id, content FROM memories WHERE " + whereClause + " ORDER BY created_at DESC LIMIT 5"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return false, 0, ""
	}
	defer rows.Close()

	type candidate struct {
		id      int64
		content string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.content); err != nil {
			continue
		}
		candidates = append(candidates, c)
	}

	if len(candidates) == 0 {
		return false, 0, ""
	}

	// 3. 检查候选记忆是否与新内容涉及同一实体类型
	for _, c := range candidates {
		oldEntityType := extractEntityType(c.content)
		if oldEntityType == "" {
			continue
		}
		// 同一实体类型 → 这是更新！（如旧"地址"+新"地址"，内容不同但实体一致）
		if oldEntityType == entityType {
			// 但如果新旧内容完全相同 → 是重复，不是更新（由上游 isDuplicate 处理）
			if c.content == newContent {
				continue
			}
			return true, c.id, newContent
		}
	}

	// 4. 没找到匹配的实体对 → 不是更新
	return false, 0, ""
}

// ── 分层搜索 ──

// SearchLayers 按层级搜索记忆
// 搜索顺序：今天 → 近7天 → 近30天 → 近90天，找到即止
func (s *Server) SearchLayers(query string) (found bool, source string, content string) {
	if query == "" {
		return false, "", ""
	}

	now := time.Now()
	today := now.Format("2006-01-02")
	weekAgo := now.AddDate(0, 0, -7).Format("2006-01-02 15:04:05")
	monthAgo := now.AddDate(0, 0, -30).Format("2006-01-02 15:04:05")
	quarterAgo := now.AddDate(0, 0, -90).Format("2006-01-02 15:04:05")

	like := "%" + query + "%"
	limit := 10

	// 1. 今天
	if c := s.searchDB(like, today+" 00:00:00", today+" 23:59:59", limit); c != "" {
		return true, "今天", c
	}
	// 2. 近7天（不含今天）
	if c := s.searchDB(like, weekAgo, today+" 00:00:00", limit); c != "" {
		return true, "近7天", c
	}
	// 3. 近30天（不含近7天）
	if c := s.searchDB(like, monthAgo, weekAgo, limit); c != "" {
		return true, "近30天", c
	}
	// 4. 近90天（不含近30天）
	if c := s.searchDB(like, quarterAgo, monthAgo, limit); c != "" {
		return true, "近90天", c
	}

	return false, "", ""
}

// SearchAllLayers 搜索所有层级，返回全部结果
func (s *Server) SearchAllLayers(query string) map[string]string {
	results := make(map[string]string)
	if query == "" {
		return results
	}

	now := time.Now()
	today := now.Format("2006-01-02")
	weekAgo := now.AddDate(0, 0, -7).Format("2006-01-02 15:04:05")
	monthAgo := now.AddDate(0, 0, -30).Format("2006-01-02 15:04:05")
	quarterAgo := now.AddDate(0, 0, -90).Format("2006-01-02 15:04:05")
	like := "%" + query + "%"
	limit := 10

	if c := s.searchDB(like, today+" 00:00:00", today+" 23:59:59", limit); c != "" {
		results["今天"] = c
	}
	if c := s.searchDB(like, weekAgo, today+" 00:00:00", limit); c != "" {
		results["近7天"] = c
	}
	if c := s.searchDB(like, monthAgo, weekAgo, limit); c != "" {
		results["近30天"] = c
	}
	if c := s.searchDB(like, quarterAgo, monthAgo, limit); c != "" {
		results["近90天"] = c
	}

	return results
}

// searchDB 在时间范围内搜索记忆
func (s *Server) searchDB(like, since, until string, limit int) string {
	rows, err := s.db.Query(
		`SELECT content FROM memories
		 WHERE content LIKE ? AND created_at >= ? AND created_at <= ?
		 ORDER BY created_at DESC LIMIT ?`,
		like, since, until, limit,
	)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			continue
		}
		results = append(results, content)
	}
	if len(results) == 0 {
		return ""
	}
	return strings.Join(results, "\n")
}

// ── 自动清理 ──

// startAutoCleanup 启动后台定时清理 goroutine
func (s *Server) startAutoCleanup() {
	maxAge := defaultCleanupMaxAge
	interval := defaultCleanupInterval

	if env := os.Getenv("MEMORY_CLEANUP_MAX_AGE"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			maxAge = d
		}
	}
	if env := os.Getenv("MEMORY_CLEANUP_INTERVAL"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			interval = d
		}
	}

	if maxAge <= 0 || interval <= 0 {
		fmt.Printf("[memory-service] 自动清理未启用（maxAge=%v, interval=%v）\n", maxAge, interval)
		return
	}

	fmt.Printf("[memory-service] 自动清理已启动: 保留 %v, 每 %v 检查一次\n", maxAge, interval)

	go func() {
		time.Sleep(1 * time.Minute)
		s.autoCleanup(maxAge)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			s.autoCleanup(maxAge)
		}
	}()
}

// autoCleanup 执行一次清理：删除超过 maxAge 的记忆
func (s *Server) autoCleanup(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge).Format("2006-01-02 15:04:05")

	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("DELETE FROM memories WHERE created_at < ?", cutoff)
	if err != nil {
		fmt.Printf("[memory-service] 自动清理失败: %v\n", err)
		return
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		fmt.Printf("[memory-service] 自动清理: 删除了 %d 条过期记忆（截止 %s）\n", n, cutoff)
	}
}

// ── 作废消息管理 ──

// handleDiscardEpoch POST /discard-epoch
// 批量将消息标记为已作废（翻篇用）
func (s *Server) handleDiscardEpoch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		SessionKey string   `json:"session_key"`
		MsgIDs     []string `json:"msg_ids"`
		EpochID    int      `json:"epoch_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.SessionKey == "" || len(req.MsgIDs) == 0 {
		writeError(w, "session_key 和 msg_ids 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, msgID := range req.MsgIDs {
		_, err := s.db.Exec(
			"INSERT OR IGNORE INTO discarded_messages (session_key, msg_id, epoch_id, discarded_at) VALUES (?, ?, ?, ?)",
			req.SessionKey, msgID, req.EpochID, now,
		)
		if err != nil {
			writeError(w, "作废失败: "+err.Error(), 500)
			return
		}
	}

	writeJSON(w, map[string]interface{}{"ok": true, "count": len(req.MsgIDs)})
}

// handleDiscardedMsgIDs GET /discarded-msgids?session_key=xxx
// 查询某 session 所有已作废的 msg_id
func (s *Server) handleDiscardedMsgIDs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}

	sessionKey := r.URL.Query().Get("session_key")
	if sessionKey == "" {
		writeError(w, "session_key 不能为空", 400)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT msg_id FROM discarded_messages WHERE session_key=?", sessionKey)
	if err != nil {
		writeError(w, "查询失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	result := make(map[string]bool)
	for rows.Next() {
		var msgID string
		if err := rows.Scan(&msgID); err != nil {
			continue
		}
		result[msgID] = true
	}

	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"msg_ids": result,
	})
}

// ── 辅助函数 ──

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, msg string, code int) {
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": msg})
}


