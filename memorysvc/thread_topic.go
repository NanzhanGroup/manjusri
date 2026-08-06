// 话题边界检测与自动切换
package memorysvc

import (
	"fmt"
	"strings"
)

func detectTopicBoundary(content string) (isNewTopic bool, strength string) {
	// 强信号：任意一个命中即触发话题切换
	for _, signal := range strongSignals {
		if strings.Contains(content, signal) {
			return true, "strong"
		}
	}

	// 中信号：统计命中次数
	mediumCount := 0
	for _, signal := range mediumSignals {
		mediumCount += strings.Count(content, signal)
	}

	if mediumCount >= 2 {
		return true, "medium"
	}
	return false, "none"
}

// ── 线程自动管理 ──

// ensureSessionHasActiveThread 确保会话有 active 线程，没有则创建
func (s *Server) ensureSessionHasActiveThread(sessionID string) (int64, error) {
	activeTh, err := s.getActiveThread(sessionID)
	if err != nil {
		return 0, fmt.Errorf("ensureSessionHasActiveThread: %w", err)
	}
	if activeTh != nil {
		return activeTh.ID, nil
	}

	newID, err := s.createThread(sessionID, "初始讨论")
	if err != nil {
		return 0, fmt.Errorf("ensureSessionHasActiveThread create: %w", err)
	}
	return newID, nil
}

// autoSwitchThread 自动闭合当前线程并创建新线程
// newContent 用于提取新线程标题
func (s *Server) autoSwitchThread(sessionID string, newContent string) (newThreadID int64, switched bool, err error) {
	activeTh, err := s.getActiveThread(sessionID)
	if err != nil {
		return 0, false, fmt.Errorf("autoSwitchThread getActive: %w", err)
	}
	if activeTh == nil {
		return 0, false, nil
	}

	if err := s.closeThread(activeTh.ID); err != nil {
		return 0, false, fmt.Errorf("autoSwitchThread close: %w", err)
	}

	// 取前 20 个字作为新线程标题
	title := newContent
	runes := []rune(title)
	if len(runes) > 20 {
		title = string(runes[:20]) + "…"
	} else if len(runes) == 0 {
		title = "新对话"
	}

	newID, err := s.createThread(sessionID, title)
	if err != nil {
		return 0, false, fmt.Errorf("autoSwitchThread create: %w", err)
	}
	return newID, true, nil
}

// ── 作废指令模式 ──

// obsoletePattern 作废指令匹配项
