package memorysvc

import (
	"testing"
	"time"
)

// ── 6.4.1 兼容无 thread_id 的旧消息 ──

func TestMigration_无ThreadID旧消息(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	// 直接插入旧消息（thread_id=NULL）
	insertMessageRaw(t, s, sessionID, 0, "user", "这是一条旧消息，没有thread_id")

	// 创建活跃线程并插入消息
	threadID, err := s.createThread(sessionID, "新话题")
	if err != nil {
		t.Fatalf("创建线程失败: %v", err)
	}
	insertMessage(t, s, sessionID, threadID, "user", "新话题的消息")

	// 验证旧消息存在
	var oldMsgCount int
	s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id=? AND thread_id IS NULL", sessionID).Scan(&oldMsgCount)
	if oldMsgCount != 1 {
		t.Errorf("期望1条无thread_id的旧消息，got %d", oldMsgCount)
	}

	// 验证活跃线程消息存在
	var activeMsgCount int
	s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id=? AND thread_id=?", sessionID, threadID).Scan(&activeMsgCount)
	if activeMsgCount != 1 {
		t.Errorf("期望1条活跃线程消息，got %d", activeMsgCount)
	}

	// 验证总消息数
	var totalCount int
	s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id=?", sessionID).Scan(&totalCount)
	if totalCount != 2 {
		t.Errorf("期望总共2条消息，got %d", totalCount)
	}
}

// insertMessageRaw 插入消息（不自动绑定 thread_id）
func insertMessageRaw(t *testing.T, s *Server, sessionID string, threadID int64, role, content string) {
	t.Helper()
	now := time.Now().Format("2006-01-02 15:04:05")
	var err error
	if threadID > 0 {
		_, err = s.db.Exec(
			"INSERT INTO messages (session_id, thread_id, role, content, tokens, created_at) VALUES (?, ?, ?, ?, 0, ?)",
			sessionID, threadID, role, content, now,
		)
	} else {
		_, err = s.db.Exec(
			"INSERT INTO messages (session_id, role, content, tokens, created_at) VALUES (?, ?, ?, 0, ?)",
			sessionID, role, content, now,
		)
	}
	if err != nil {
		t.Fatalf("插入消息失败: %v", err)
	}
}

// ── 6.4.2 迁移后 /context/build 正常工作 ──

func TestMigration_混搭消息ContextBuild(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	// 插入旧消息（无 thread_id）
	insertMessageRaw(t, s, sessionID, 0, "user", "旧消息：之前的历史")

	// 创建活跃线程并插入消息
	threadID, err := s.createThread(sessionID, "活跃话题")
	if err != nil {
		t.Fatalf("创建线程失败: %v", err)
	}
	insertMessage(t, s, sessionID, threadID, "user", "活跃线程的消息")

	// 验证不 panic
	var historyCount int
	s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id=? AND (thread_id IN (SELECT id FROM session_threads WHERE session_id=? AND status='active') OR thread_id IS NULL)", sessionID, sessionID).Scan(&historyCount)
	if historyCount != 2 {
		t.Errorf("期望2条消息（活跃消息+无thread_id旧消息），got %d", historyCount)
	}

	// 验证线程统计
	var activeCount, obsoleteCount int
	s.db.QueryRow("SELECT COUNT(*) FROM session_threads WHERE session_id=? AND status='active'", sessionID).Scan(&activeCount)
	s.db.QueryRow("SELECT COUNT(*) FROM session_threads WHERE session_id=? AND status='obsolete'", sessionID).Scan(&obsoleteCount)
	if activeCount != 1 {
		t.Errorf("期望1个active线程，got %d", activeCount)
	}
	if obsoleteCount != 0 {
		t.Errorf("期望0个obsolete线程，got %d", obsoleteCount)
	}
}
