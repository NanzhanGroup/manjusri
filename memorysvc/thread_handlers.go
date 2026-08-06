// 线程 HTTP 处理器
package memorysvc

import (
	"encoding/json"
	"net/http"
	"strconv"
)

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
