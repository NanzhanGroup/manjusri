package memorysvc

import (
	"database/sql"
	"fmt"
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
