package memorysvc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// ── 话题边界信号词 ──

// strongSignals 强信号词：出现任意一个即触发话题切换
var strongSignals = []string{
	"换一个思路", "换个方案", "行不通", "试试别的",
	"以上作废", "之前说的不算", "重来", "重新来",
	"换个角度", "不考虑了", "推翻",
}

// mediumSignals 中信号词：出现至少 2 次才触发话题切换
var mediumSignals = []string{
	"但是", "不过", "然而", "其实", "实际上",
	"更好的方案是", "我突然想到", "等等", "等一下",
}

// weakSignals 弱信号词：仅作为参考，不单独触发切换
var weakSignals = []string{
	"嗯", "好吧", "那算了", "明白了", "懂了",
}

// ── 线程 CRUD ──

// createThread 创建新线程，返回 thread_id
func (s *Server) createThread(sessionID, title string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO session_threads
		(session_id, title, status, created_at)
		VALUES (?, ?, 'active', datetime('now','localtime'))`, sessionID, title)
	if err != nil {
		return 0, fmt.Errorf("createThread: %w", err)
	}
	return res.LastInsertId()
}

// getThread 按 id 获取线程及其消息
func (s *Server) getThread(id int64) (*Thread, []ThreadMessage, error) {
	t := &Thread{}
	err := s.db.QueryRow(`SELECT id, session_id, title, status,
		COALESCE(start_msg_id,0), COALESCE(end_msg_id,0),
		COALESCE(superseded_by,0), COALESCE(summary,''),
		created_at, COALESCE(closed_at,'')
		FROM session_threads WHERE id=?`, id).Scan(
		&t.ID, &t.SessionID, &t.Title, &t.Status,
		&t.StartMsgID, &t.EndMsgID,
		&t.SupersededBy, &t.Summary,
		&t.CreatedAt, &t.ClosedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("getThread: %w", err)
	}

	rows, err := s.db.Query(`SELECT id, role, content, created_at
		FROM messages WHERE thread_id=? ORDER BY id ASC`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("getThread messages: %w", err)
	}
	defer rows.Close()

	var msgs []ThreadMessage
	for rows.Next() {
		var m ThreadMessage
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("getThread scan: %w", err)
		}
		msgs = append(msgs, m)
	}
	return t, msgs, rows.Err()
}

// listThreads 列举线程，status 为空则查全部状态
func (s *Server) listThreads(sessionID, status string) ([]Thread, error) {
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.Query(`SELECT id, session_id, title, status,
			COALESCE(start_msg_id,0), COALESCE(end_msg_id,0),
			COALESCE(superseded_by,0), COALESCE(summary,''),
			created_at, COALESCE(closed_at,'')
			FROM session_threads
			WHERE session_id=? ORDER BY id DESC`, sessionID)
	} else {
		rows, err = s.db.Query(`SELECT id, session_id, title, status,
			COALESCE(start_msg_id,0), COALESCE(end_msg_id,0),
			COALESCE(superseded_by,0), COALESCE(summary,''),
			created_at, COALESCE(closed_at,'')
			FROM session_threads
			WHERE session_id=? AND status=? ORDER BY id DESC`, sessionID, status)
	}
	if err != nil {
		return nil, fmt.Errorf("listThreads: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		var t Thread
		if err := rows.Scan(
			&t.ID, &t.SessionID, &t.Title, &t.Status,
			&t.StartMsgID, &t.EndMsgID,
			&t.SupersededBy, &t.Summary,
			&t.CreatedAt, &t.ClosedAt,
		); err != nil {
			return nil, fmt.Errorf("listThreads scan: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// getActiveThread 获取当前活跃线程，查不到返回 (nil, nil)
func (s *Server) getActiveThread(sessionID string) (*Thread, error) {
	t := &Thread{}
	err := s.db.QueryRow(`SELECT id, session_id, title, status,
		COALESCE(start_msg_id,0), COALESCE(end_msg_id,0),
		COALESCE(superseded_by,0), COALESCE(summary,''),
		created_at, COALESCE(closed_at,'')
		FROM session_threads
		WHERE session_id=? AND status='active'
		ORDER BY id DESC LIMIT 1`, sessionID).Scan(
		&t.ID, &t.SessionID, &t.Title, &t.Status,
		&t.StartMsgID, &t.EndMsgID,
		&t.SupersededBy, &t.Summary,
		&t.CreatedAt, &t.ClosedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getActiveThread: %w", err)
	}
	return t, nil
}

// closeThread 将线程标记为 obsolete（关闭），先更新 summary
func (s *Server) closeThread(threadID int64) error {
	summary, err := s.generateThreadSummary(threadID)
	if err != nil {
		return fmt.Errorf("closeThread summary: %w", err)
	}
	_, err = s.db.Exec(`UPDATE session_threads
		SET status='obsolete', closed_at=datetime('now','localtime'),
		    summary=?
		WHERE id=? AND status='active'`, summary, threadID)
	if err != nil {
		return fmt.Errorf("closeThread: %w", err)
	}
	return nil
}

// reactivateThread 将已关闭的线程重新激活
func (s *Server) reactivateThread(threadID int64) error {
	_, err := s.db.Exec(`UPDATE session_threads
		SET status='active', closed_at=NULL WHERE id=?`, threadID)
	if err != nil {
		return fmt.Errorf("reactivateThread: %w", err)
	}
	return nil
}

// updateMessageThreadID 将消息移动到指定线程
func (s *Server) updateMessageThreadID(msgID, threadID int64) error {
	_, err := s.db.Exec(`UPDATE messages SET thread_id=? WHERE id=?`, threadID, msgID)
	if err != nil {
		return fmt.Errorf("updateMessageThreadID: %w", err)
	}
	return nil
}

// generateThreadSummary 取该线程前 2 条非 system 消息的 content，用 " / " 拼接
func (s *Server) generateThreadSummary(threadID int64) (string, error) {
	rows, err := s.db.Query(`SELECT content FROM messages
		WHERE thread_id=? AND role!='system'
		ORDER BY id ASC LIMIT 2`, threadID)
	if err != nil {
		return "", fmt.Errorf("generateThreadSummary: %w", err)
	}
	defer rows.Close()

	var parts []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return "", fmt.Errorf("generateThreadSummary scan: %w", err)
		}
		parts = append(parts, content)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	summary := strings.Join(parts, " / ")
	if len(summary) > 200 {
		summary = summary[:200]
	}
	return summary, nil
}

// ── HTTP Handlers ──

// handleThreadClose POST 关闭当前活跃线程并创建新线程
// 请求: {"session_id":"..."}
// 响应: {"ok":true, "old_thread_id":N, "new_thread_id":N}
func (s *Server) handleThreadClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid json", 400)
		return
	}
	if req.SessionID == "" {
		writeError(w, "session_id required", 400)
		return
	}

	activeTh, err := s.getActiveThread(req.SessionID)
	if err != nil {
		writeError(w, err.Error(), 500)
		return
	}

	var oldID int64
	if activeTh != nil {
		oldID = activeTh.ID
		if err := s.closeThread(oldID); err != nil {
			writeError(w, err.Error(), 500)
			return
		}
	}

	newID, err := s.createThread(req.SessionID, "新对话")
	if err != nil {
		writeError(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":            true,
		"old_thread_id": oldID,
		"new_thread_id": newID,
	})
}

// handleThreadList GET 列举 session 的线程
// 参数: session_id (query), status (query, 可选)
// 响应: {"ok":true, "threads":[...], "total":N}
func (s *Server) handleThreadList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "method not allowed", 405)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	status := r.URL.Query().Get("status")
	if sessionID == "" {
		writeError(w, "session_id required", 400)
		return
	}

	threads, err := s.listThreads(sessionID, status)
	if err != nil {
		writeError(w, err.Error(), 500)
		return
	}
	if threads == nil {
		threads = []Thread{}
	}

	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"threads": threads,
		"total":   len(threads),
	})
}

// handleThreadGet GET 按 id 获取线程及其消息
// 参数: id (query)
// 响应: {"ok":true, "thread":{...}, "messages":[...]}
func (s *Server) handleThreadGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "method not allowed", 405)
		return
	}
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, "invalid id", 400)
		return
	}

	thread, msgs, err := s.getThread(id)
	if err != nil {
		writeError(w, err.Error(), 500)
		return
	}
	if thread == nil {
		writeError(w, "thread not found", 404)
		return
	}
	if msgs == nil {
		msgs = []ThreadMessage{}
	}

	writeJSON(w, map[string]interface{}{
		"ok":       true,
		"thread":   thread,
		"messages": msgs,
	})
}

// handleThreadReactivate POST 重新激活已关闭的线程
// 请求: {"thread_id":N}
// 响应: {"ok":true}
func (s *Server) handleThreadReactivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		ThreadID int64 `json:"thread_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid json", 400)
		return
	}
	if req.ThreadID == 0 {
		writeError(w, "thread_id required", 400)
		return
	}

	if err := s.reactivateThread(req.ThreadID); err != nil {
		writeError(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{"ok": true})
}

// ── 话题边界检测 ──

// detectTopicBoundary 检测用户消息是否包含话题切换信号
// 返回: isNewTopic 是否切换, strength 信号强度(strong/medium/none)
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
type obsoletePattern struct {
	pattern *regexp.Regexp
	action  string // "close_current" 或 "close_topic"
}

// obsoleteCommandPatterns 作废指令正则列表
var obsoleteCommandPatterns = []obsoletePattern{
	{
		pattern: regexp.MustCompile(`^(以上|之前|前面).*(作废|不算|不算数|放弃|不要)`),
		action:  "close_current",
	},
	{
		pattern: regexp.MustCompile(`^(重新|重来|从头).*(开始|来)`),
		action:  "close_current",
	},
	{
		pattern: regexp.MustCompile(`存档|归档`),
		action:  "close_current",
	},
	{
		pattern: regexp.MustCompile(`不讨论.+了$`),
		action:  "close_topic",
	},
	{
		pattern: regexp.MustCompile(`.+作废$`),
		action:  "close_topic",
	},
}

// matchObsoleteCommand 匹配用户消息中是否包含作废指令
// 返回: matched 是否匹配, action 动作类型, keyword 提取的关键词(close_topic时使用)
func matchObsoleteCommand(content string) (matched bool, action string, keyword string) {
	for _, p := range obsoleteCommandPatterns {
		if p.pattern.MatchString(content) {
			if p.action == "close_current" {
				return true, "close_current", ""
			}
			if p.action == "close_topic" {
				// 尝试提取 "不讨论XXX了" 中的关键词
				re := regexp.MustCompile(`不讨论(.+)了`)
				matches := re.FindStringSubmatch(content)
				if len(matches) >= 2 {
					keyword = strings.TrimSpace(matches[1])
				} else {
					// 尝试提取 "XXX作废" 中的关键词
					re2 := regexp.MustCompile(`(.+)作废$`)
					matches2 := re2.FindStringSubmatch(content)
					if len(matches2) >= 2 {
						keyword = strings.TrimSpace(matches2[1])
					} else {
						keyword = content
					}
				}
				return true, "close_topic", keyword
			}
		}
	}
	return false, "", ""
}

// closeThreadsByKeyword 按关键词关闭会话中匹配的活跃线程
func (s *Server) closeThreadsByKeyword(sessionID, keyword string) (int, error) {
	rows, err := s.db.Query(
		`SELECT id FROM session_threads
		 WHERE session_id=? AND status='active' AND title LIKE ?`,
		sessionID, "%"+keyword+"%",
	)
	if err != nil {
		return 0, fmt.Errorf("closeThreadsByKeyword query: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("closeThreadsByKeyword scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range ids {
		if err := s.closeThread(id); err != nil {
			return 0, fmt.Errorf("closeThreadsByKeyword close %d: %w", id, err)
		}
	}
	return len(ids), nil
}

// ── 作废线程摘要 ──

// getObsoleteSummaries 获取会话中所有已作废线程的摘要信息
func (s *Server) getObsoleteSummaries(sessionID string) (string, error) {
	rows, err := s.db.Query(
		`SELECT id, title, summary, created_at FROM session_threads
		 WHERE session_id=? AND status='obsolete'
		   AND summary IS NOT NULL AND summary != ''
		 ORDER BY id DESC LIMIT 50`,
		sessionID,
	)
	if err != nil {
		return "", fmt.Errorf("getObsoleteSummaries query: %w", err)
	}
	defer rows.Close()

	var items []string
	for rows.Next() {
		var id int64
		var title, summary, createdAt string
		if err := rows.Scan(&id, &title, &summary, &createdAt); err != nil {
			continue
		}
		// 截取时间到日期
		dateStr := createdAt
		if len(dateStr) >= 10 {
			dateStr = dateStr[:10]
		}
		items = append(items, fmt.Sprintf("- [%s] (%s): %s", title, dateStr, summary))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	if len(items) == 0 {
		return "", nil
	}

	return "📋 历史话题里程碑：\n" + strings.Join(items, "\n"), nil
}
