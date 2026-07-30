package memorysvc

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ── 集成测试辅助 ──

func setupIntegrationTest(t *testing.T) (*Server, string) {
	t.Helper()
	s := &Server{dataDir: t.TempDir()}
	db, err := sql.Open("sqlite", ":memory:?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开内存数据库失败: %v", err)
	}
	s.db = db
	if err := s.initSchema(); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	_, err = s.db.Exec(`INSERT INTO sessions (session_id, message_count, created_at, updated_at) VALUES (?, 0, datetime('now'), datetime('now'))`, "test-session")
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	return s, "test-session"
}

func insertMessage(t *testing.T, s *Server, sessionID string, threadID int64, role, content string) int64 {
	t.Helper()
	now := time.Now().Format("2006-01-02 15:04:05")
	result, err := s.db.Exec(
		"INSERT INTO messages (session_id, thread_id, role, content, tokens, created_at) VALUES (?, ?, ?, ?, 0, ?)",
		sessionID, threadID, role, content, now,
	)
	if err != nil {
		t.Fatalf("插入消息失败: %v", err)
	}
	msgID, _ := result.LastInsertId()
	return msgID
}

// ── 6.2.1 场景 A：三个方案迭代 ──

func TestIntegration_三个方案迭代(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	thread1ID, err := s.createThread(sessionID, "方案A")
	if err != nil {
		t.Fatalf("创建线程1失败: %v", err)
	}
	insertMessage(t, s, sessionID, thread1ID, "user", "我们试试方案A")

	active, err := s.getActiveThread(sessionID)
	if err != nil || active == nil {
		t.Fatalf("获取活跃线程失败: %v", err)
	}
	if active.ID != thread1ID {
		t.Errorf("期望活跃线程ID=%d，got %d", thread1ID, active.ID)
	}

	newID, switched, err := s.autoSwitchThread(sessionID, "换个方案，用B")
	if err != nil {
		t.Fatalf("autoSwitchThread 失败: %v", err)
	}
	if !switched {
		t.Fatal("期望线程切换=true")
	}
	insertMessage(t, s, sessionID, newID, "user", "试试方案B")

	thread1, _, err := s.getThread(thread1ID)
	if err != nil {
		t.Fatalf("获取线程1失败: %v", err)
	}
	if thread1.Status != "obsolete" {
		t.Errorf("期望线程1=obsolete，got %s", thread1.Status)
	}

	_ = newID
	newID2, switched2, err := s.autoSwitchThread(sessionID, "行不通，换方案C")
	if err != nil {
		t.Fatalf("autoSwitchThread 失败: %v", err)
	}
	if !switched2 {
		t.Fatal("期望第2次切换=true")
	}
	insertMessage(t, s, sessionID, newID2, "user", "最终方案C")

	threads, err := s.listThreads(sessionID, "")
	if err != nil {
		t.Fatalf("listThreads 失败: %v", err)
	}
	if len(threads) != 3 {
		t.Errorf("期望3个线程，got %d", len(threads))
	}
	// ORDER BY id DESC: 最新在前，所以 thread3=active, thread2=obsolete, thread1=obsolete
	for i := 1; i < len(threads); i++ {
		if threads[i].Status != "obsolete" {
			t.Errorf("线程%d: 期望=obsolete，got %s", i+1, threads[i].Status)
		}
	}
	if threads[0].Status != "active" {
		t.Errorf("第一线程（最新）: 期望=active，got %s", threads[len(threads)-1].Status)
	}
}

// ── 6.2.2 场景 B：作废指令识别 ──

func TestIntegration_作废指令识别(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	thread1ID, err := s.createThread(sessionID, "方案A")
	if err != nil {
		t.Fatalf("创建线程失败: %v", err)
	}
	insertMessage(t, s, sessionID, thread1ID, "user", "方案A怎么样")

	newID, switched, err := s.autoSwitchThread(sessionID, "以上作废")
	if err != nil {
		t.Fatalf("autoSwitchThread 失败: %v", err)
	}
	if !switched {
		t.Fatal("期望切换=true")
	}

	t1, _, _ := s.getThread(thread1ID)
	if t1.Status != "obsolete" {
		t.Errorf("期望thread1=obsolete，got %s", t1.Status)
	}

	active, _ := s.getActiveThread(sessionID)
	if active.ID != newID {
		t.Errorf("期望活跃线程ID=%d，got %d", newID, active.ID)
	}

	threadBID, _ := s.createThread(sessionID, "方案B讨论")
	threadCID, _ := s.createThread(sessionID, "方案C讨论")

	closed, _ := s.closeThreadsByKeyword(sessionID, "方案B")
	if closed < 1 {
		t.Error("期望至少关闭1个线程")
	}

	tB, _, _ := s.getThread(threadBID)
	if tB.Status != "obsolete" {
		t.Errorf("期望threadB=obsolete，got %s", tB.Status)
	}
	tC, _, _ := s.getThread(threadCID)
	if tC.Status != "active" {
		t.Errorf("期望threadC仍为active，got %s", tC.Status)
	}
}

// ── 6.2.3 场景 C：context/build 活跃线程 ──

func TestIntegration_ContextBuild仅活跃线程(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	t1, _ := s.createThread(sessionID, "方案A")
	insertMessage(t, s, sessionID, t1, "user", "方案A消息")
	s.closeThread(t1)

	t2, _ := s.createThread(sessionID, "方案B")
	insertMessage(t, s, sessionID, t2, "user", "方案B消息")

	var ac, oc int
	s.db.QueryRow("SELECT COUNT(*) FROM session_threads WHERE session_id=? AND status='active'", sessionID).Scan(&ac)
	s.db.QueryRow("SELECT COUNT(*) FROM session_threads WHERE session_id=? AND status='obsolete'", sessionID).Scan(&oc)
	if ac != 1 || oc != 1 {
		t.Errorf("期望active=1 obsolete=1，got active=%d obsolete=%d", ac, oc)
	}

	milestones, _ := s.getObsoleteSummaries(sessionID)
	if milestones == "" {
		t.Error("期望里程碑不为空")
	}
	if !strings.Contains(milestones, "方案A") {
		t.Errorf("里程碑应包含方案A: %s", milestones)
	}

	_, msgs, _ := s.getThread(t2)
	if len(msgs) == 0 {
		t.Error("期望活跃线程有消息")
	}
}

// ── 6.2.4 场景 D：include_obsolete ──

func TestIntegration_ContextBuild包含作废线程(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	t1, _ := s.createThread(sessionID, "旧方案")
	insertMessage(t, s, sessionID, t1, "user", "旧方案内容")
	s.closeThread(t1)

	t2, _ := s.createThread(sessionID, "新方案")
	insertMessage(t, s, sessionID, t2, "user", "新方案内容")

	var total int
	s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id=?", sessionID).Scan(&total)
	if total != 2 {
		t.Errorf("期望2条消息，got %d", total)
	}

	milestones, _ := s.getObsoleteSummaries(sessionID)
	if milestones != "" && !strings.Contains(milestones, "旧方案") {
		t.Errorf("里程碑应包含旧方案: %s", milestones)
	}
}

// ── 6.2.5 场景 E：reactivate ──

func TestIntegration_Reactivate恢复线程(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	tid, _ := s.createThread(sessionID, "被关闭的方案")
	insertMessage(t, s, sessionID, tid, "user", "这条消息本应关闭")
	s.closeThread(tid)

	th, _, _ := s.getThread(tid)
	if th.Status != "obsolete" {
		t.Errorf("期望obsolete，got %s", th.Status)
	}

	s.reactivateThread(tid)
	th2, _, _ := s.getThread(tid)
	if th2.Status != "active" {
		t.Errorf("期望恢复后active，got %s", th2.Status)
	}

	threads, _ := s.listThreads(sessionID, "active")
	found := false
	for _, t2 := range threads {
		if t2.ID == tid {
			found = true
		}
	}
	if !found {
		t.Error("恢复的线程应在active列表中")
	}
}
