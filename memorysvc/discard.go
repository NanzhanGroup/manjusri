// 作废消息管理（翻篇作废）
package memorysvc

import (
	"encoding/json"
	"net/http"
	"time"
)

// ── 作废消息管理 ──

// handleDiscardEpoch POST /discard-epoch
// 批量将消息标记为已作废（翻篇用）
func (s *Server) handleDiscardEpoch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		SessionKey string   `json:"session_key"`
		MsgIDs     []string `json:"msg_ids"`
		EpochID    int      `json:"epoch_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.SessionKey == "" || len(req.MsgIDs) == 0 {
		writeError(w, "session_key 和 msg_ids 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, msgID := range req.MsgIDs {
		_, err := s.db.Exec(
			"INSERT OR IGNORE INTO discarded_messages (session_key, msg_id, epoch_id, discarded_at) VALUES (?, ?, ?, ?)",
			req.SessionKey, msgID, req.EpochID, now,
		)
		if err != nil {
			writeError(w, "作废失败: "+err.Error(), 500)
			return
		}
	}

	writeJSON(w, map[string]interface{}{"ok": true, "count": len(req.MsgIDs)})
}

// handleDiscardedMsgIDs GET /discarded-msgids?session_key=xxx
// 查询某 session 所有已作废的 msg_id
func (s *Server) handleDiscardedMsgIDs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}

	sessionKey := r.URL.Query().Get("session_key")
	if sessionKey == "" {
		writeError(w, "session_key 不能为空", 400)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT msg_id FROM discarded_messages WHERE session_key=?", sessionKey)
	if err != nil {
		writeError(w, "查询失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	result := make(map[string]bool)
	for rows.Next() {
		var msgID string
		if err := rows.Scan(&msgID); err != nil {
			continue
		}
		result[msgID] = true
	}

	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"msg_ids": result,
	})
}
