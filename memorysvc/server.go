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
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ── 默认清理配置（可通过环境变量覆盖） ──
const (
	defaultCleanupMaxAge   = 90 * 24 * time.Hour // 默认保留90天
	defaultCleanupInterval = 1 * time.Hour       // 每小时检查一次
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

	// ── socket 文件处理（防"误删活实例 socket → 活实例变匿名监听"）──
	// 先探测：路径可连接 → 仍有活实例在监听（flock 之外的冗余防线），拒绝启动；
	// 连不上 → 说明是无人监听的僵尸文件（进程 SIGKILL 等场景残留），删除后重新监听。
	if conn, err := net.DialTimeout("unix", s.socketPath, 300*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("已有实例监听 %s，拒绝启动（防双实例）", s.socketPath)
	}
	// 删除僵尸 socket 文件（确认无活实例后才删，避免把活实例变成匿名监听）
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

// ── 辅助函数 ──

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, msg string, code int) {
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": msg})
}
