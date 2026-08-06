// 会话管理 HTTP 处理器
package memorysvc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

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
		Processed   *bool  `json:"processed"` // pointer to detect "not set"
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
