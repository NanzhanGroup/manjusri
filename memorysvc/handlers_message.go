// 消息管理 + 上下文拼装 HTTP 处理器
package memorysvc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

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

	// 4. 已作废线程的摘要不再拼接到 system_prompt（2026-08-04 zxq）：
	//    作废话题不应再出现在 LLM 上下文中，否则作废机制失去意义。
	//    历史消息（第 5 步）也只加载 active 线程，作废线程的消息同样不进上下文。

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
			"memory_count":          memoryCount,
			"history_count":         historyCount,
			"memory_source":         memorySource,
			"thread_active_count":   threadActiveCount,
			"thread_obsolete_count": threadObsoleteCount,
		},
	})
}
