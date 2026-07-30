package memorysvc

import (
	"testing"
)

// ── 6.3.1 空会话第一个消息 → 自动创建线程 ──

func TestEdge_空会话自动创建线程(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	// 验证初始无线程
	threads, err := s.listThreads(sessionID, "")
	if err != nil {
		t.Fatalf("listThreads 失败: %v", err)
	}
	if len(threads) != 0 {
		t.Errorf("期望初始0个线程，got %d", len(threads))
	}

	// 模拟第一条消息：ensureSessionHasActiveThread 会自动创建线程
	threadID, err := s.ensureSessionHasActiveThread(sessionID)
	if err != nil {
		t.Fatalf("ensureSessionHasActiveThread 失败: %v", err)
	}
	if threadID <= 0 {
		t.Fatal("期望 threadID > 0")
	}

	// 验证线程已创建且 title 不为空
	thread, _, err := s.getThread(threadID)
	if err != nil {
		t.Fatalf("getThread 失败: %v", err)
	}
	if thread.Title == "" {
		t.Error("期望线程 title 不为空")
	}
	if thread.Status != "active" {
		t.Errorf("期望状态=active，got %s", thread.Status)
	}
}

// ── 6.3.2 单线程无切换 ──

func TestEdge_单线程无切换(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	// 创建线程
	threadID, err := s.createThread(sessionID, "单一话题")
	if err != nil {
		t.Fatalf("创建线程失败: %v", err)
	}
	insertMessage(t, s, sessionID, threadID, "user", "你好")
	insertMessage(t, s, sessionID, threadID, "assistant", "你好！有什么可以帮助你的？")
	insertMessage(t, s, sessionID, threadID, "user", "今天天气不错")
	insertMessage(t, s, sessionID, threadID, "assistant", "是的，天气很好")
	insertMessage(t, s, sessionID, threadID, "user", "你有什么推荐的活动吗")

	// 验证线程唯一、无切换
	threads, err := s.listThreads(sessionID, "")
	if err != nil {
		t.Fatalf("listThreads 失败: %v", err)
	}
	if len(threads) != 1 {
		t.Errorf("期望1个线程，got %d", len(threads))
	}
	if threads[0].ID != threadID {
		t.Errorf("期望线程ID不变，got %d", threads[0].ID)
	}

	// 验证活跃线程
	active, err := s.getActiveThread(sessionID)
	if err != nil || active == nil {
		t.Fatalf("getActiveThread 失败: %v", err)
	}
	if active.ID != threadID {
		t.Errorf("期望活跃线程ID=%d，got %d", threadID, active.ID)
	}

	// 验证消息数
	var msgCount int
	s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id=?", sessionID).Scan(&msgCount)
	if msgCount != 5 {
		t.Errorf("期望5条消息，got %d", msgCount)
	}
}

// ── 6.3.3 误检测 → reactivate 恢复 ──

func TestEdge_误检测恢复(t *testing.T) {
	s, sessionID := setupIntegrationTest(t)

	// 创建线程并插入消息
	threadID, err := s.createThread(sessionID, "正常讨论")
	if err != nil {
		t.Fatalf("创建线程失败: %v", err)
	}
	insertMessage(t, s, sessionID, threadID, "user", "我们来讨论一下方案")

	// 误关闭（模拟用户说"以上作废"）
	if err := s.closeThread(threadID); err != nil {
		t.Fatalf("closeThread 失败: %v", err)
	}

	// 验证已关闭
	thread, _, err := s.getThread(threadID)
	if err != nil {
		t.Fatalf("getThread 失败: %v", err)
	}
	if thread.Status != "obsolete" {
		t.Errorf("期望状态=obsolete，got %s", thread.Status)
	}

	// 恢复
	if err := s.reactivateThread(threadID); err != nil {
		t.Fatalf("reactivateThread 失败: %v", err)
	}

	// 验证已恢复
	thread2, _, err := s.getThread(threadID)
	if err != nil {
		t.Fatalf("getThread 失败: %v", err)
	}
	if thread2.Status != "active" {
		t.Errorf("期望恢复后状态=active，got %s", thread2.Status)
	}

	// 验证消息仍然存在
	var msgCount int
	s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE thread_id=?", threadID).Scan(&msgCount)
	if msgCount != 1 {
		t.Errorf("期望1条消息，got %d", msgCount)
	}
}
