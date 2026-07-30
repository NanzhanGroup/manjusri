package memorysvc

import (
	"database/sql"
	"strings"
	"testing"
)

// ── detectTopicBoundary 强信号触发 ──

func TestDetectTopicBoundary_StrongSignal_以上作废(t *testing.T) {
	isNew, strength := detectTopicBoundary("以上作废，我们换个方案")
	if !isNew {
		t.Errorf("期望 isNewTopic=true，got false")
	}
	if strength != "strong" {
		t.Errorf("期望 strength=strong，got %s", strength)
	}
}

func TestDetectTopicBoundary_StrongSignal_重来(t *testing.T) {
	isNew, _ := detectTopicBoundary("重来，重新设计")
	if !isNew {
		t.Errorf("期望 isNewTopic=true，got false")
	}
}

func TestDetectTopicBoundary_StrongSignal_换个方案(t *testing.T) {
	isNew, _ := detectTopicBoundary("换个方案，用B试试")
	if !isNew {
		t.Errorf("期望 isNewTopic=true，got false")
	}
}

func TestDetectTopicBoundary_NormalText_不触发(t *testing.T) {
	isNew, _ := detectTopicBoundary("今天天气不错，适合出去玩")
	if isNew {
		t.Errorf("期望 isNewTopic=false，got true")
	}
}

// ── detectTopicBoundary 中信号双触发 ──

func TestDetectTopicBoundary_MediumDouble_但是不过(t *testing.T) {
	isNew, _ := detectTopicBoundary("这个方案不错，但是成本太高，不过效果还可以")
	if !isNew {
		t.Errorf("期望 isNewTopic=true（2个中信号），got false")
	}
}

func TestDetectTopicBoundary_MediumSingle_但是(t *testing.T) {
	isNew, _ := detectTopicBoundary("这个方案不错，但是成本太高")
	if isNew {
		t.Errorf("期望 isNewTopic=false（1个中信号），got true")
	}
}

func TestDetectTopicBoundary_ZeroMedium(t *testing.T) {
	isNew, _ := detectTopicBoundary("你好，我想咨询一下")
	if isNew {
		t.Errorf("期望 isNewTopic=false（0个中信号），got true")
	}
}

// ── matchObsoleteCommand 作废指令匹配 ──

func TestMatchObsoleteCommand_以上作废(t *testing.T) {
	matched, action, keyword := matchObsoleteCommand("以上作废")
	if !matched {
		t.Fatal("期望 matched=true")
	}
	if action != "close_current" {
		t.Errorf("期望 action=close_current，got %s", action)
	}
	if keyword != "" {
		t.Errorf("期望 keyword 为空，got %s", keyword)
	}
}

func TestMatchObsoleteCommand_之前不算(t *testing.T) {
	matched, action, keyword := matchObsoleteCommand("之前说的不算")
	if !matched {
		t.Fatal("期望 matched=true")
	}
	if action != "close_current" {
		t.Errorf("期望 action=close_current，got %s", action)
	}
	if keyword != "" {
		t.Errorf("期望 keyword 为空，got %s", keyword)
	}
}

func TestMatchObsoleteCommand_重新来(t *testing.T) {
	matched, action, _ := matchObsoleteCommand("重新来")
	if !matched {
		t.Fatal("期望 matched=true")
	}
	if action != "close_current" {
		t.Errorf("期望 action=close_current，got %s", action)
	}
}

func TestMatchObsoleteCommand_存档(t *testing.T) {
	matched, action, _ := matchObsoleteCommand("把这条对话存档")
	if !matched {
		t.Fatal("期望 matched=true")
	}
	if action != "close_current" {
		t.Errorf("期望 action=close_current，got %s", action)
	}
}

func TestMatchObsoleteCommand_归档(t *testing.T) {
	matched, action, _ := matchObsoleteCommand("归档")
	if !matched {
		t.Fatal("期望 matched=true")
	}
	if action != "close_current" {
		t.Errorf("期望 action=close_current，got %s", action)
	}
}

func TestMatchObsoleteCommand_不讨论话题(t *testing.T) {
	matched, action, keyword := matchObsoleteCommand("不讨论方案A了")
	if !matched {
		t.Fatal("期望 matched=true")
	}
	if action != "close_topic" {
		t.Errorf("期望 action=close_topic，got %s", action)
	}
	if keyword != "方案A" {
		t.Errorf("期望 keyword=方案A，got %s", keyword)
	}
}

func TestMatchObsoleteCommand_话题作废(t *testing.T) {
	matched, action, keyword := matchObsoleteCommand("方案A作废")
	if !matched {
		t.Fatal("期望 matched=true")
	}
	if action != "close_topic" {
		t.Errorf("期望 action=close_topic，got %s", action)
	}
	if keyword != "方案A" {
		t.Errorf("期望 keyword=方案A，got %s", keyword)
	}
}

func TestMatchObsoleteCommand_普通文本不匹配(t *testing.T) {
	matched, action, keyword := matchObsoleteCommand("你好，今天天气不错")
	if matched {
		t.Errorf("期望 matched=false，got true (action=%s, keyword=%s)", action, keyword)
	}
}

// ── generateThreadSummary 摘要提取 ──

// setupTestDB 创建内存 SQLite 的 Server 实例用于测试
func setupTestDB(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		dataDir: t.TempDir(),
	}
	db, err := sql.Open("sqlite", ":memory:?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("打开内存数据库失败: %v", err)
	}
	s.db = db
	// 建表
	if err := s.initSchema(); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return s
}

func TestGenerateThreadSummary(t *testing.T) {
	s := setupTestDB(t)

	// 创建会话
	_, err := s.db.Exec(`INSERT INTO sessions (session_id, message_count, created_at, updated_at) VALUES (?, 0, datetime('now'), datetime('now'))`, "test-session")
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	// 创建线程
	threadID, err := s.createThread("test-session", "测试方案")
	if err != nil {
		t.Fatalf("创建线程失败: %v", err)
	}

	// 插入测试消息
	_, err = s.db.Exec(`INSERT INTO messages (session_id, thread_id, role, content, created_at) VALUES (?, ?, 'user', '我们来试试方案A', datetime('now'))`, "test-session", threadID)
	if err != nil {
		t.Fatalf("插入消息1失败: %v", err)
	}
	_, err = s.db.Exec(`INSERT INTO messages (session_id, thread_id, role, content, created_at) VALUES (?, ?, 'assistant', '好的，方案A的核心思路是…', datetime('now'))`, "test-session", threadID)
	if err != nil {
		t.Fatalf("插入消息2失败: %v", err)
	}
	_, err = s.db.Exec(`INSERT INTO messages (session_id, thread_id, role, content, created_at) VALUES (?, ?, 'user', '这个方案可以再优化一下', datetime('now'))`, "test-session", threadID)
	if err != nil {
		t.Fatalf("插入消息3失败: %v", err)
	}

	// 调用 generateThreadSummary
	summary, err := s.generateThreadSummary(threadID)
	if err != nil {
		t.Fatalf("generateThreadSummary 失败: %v", err)
	}
	if summary == "" {
		t.Fatal("摘要不应为空")
	}

	// 验证摘要包含前 2 条非 system 消息的内容
	if !strings.Contains(summary, "方案A") {
		t.Errorf("摘要应包含'方案A'，实际: %s", summary)
	}
	if !strings.Contains(summary, "核心思路") {
		t.Errorf("摘要应包含'核心思路'，实际: %s", summary)
	}

	// 验证摘要长度不超过 200 字符
	if len(summary) > 200 {
		t.Errorf("摘要长度 %d 超过 200 字符限制", len(summary))
	}
}

