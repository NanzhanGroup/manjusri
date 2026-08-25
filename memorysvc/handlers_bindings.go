// 跨平台用户绑定 HTTP 处理器
package memorysvc

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// ── 跨平台身份绑定 ──
// 用途：用户在多个平台（微信/企微/飞书/Telegram）与同一智能体对话时，
// 把"别名 session"绑定到"主 session"，让记忆/历史跨平台共享。
// 绑定关系存 user_bindings 表，由 memory-service 统一管理。

// handleBind POST /bind
// 绑定别名 session → 主 session（幂等：重复绑定覆盖）
func (s *Server) handleBind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		AliasSession   string `json:"alias_session"`
		PrimarySession string `json:"primary_session"`
		Note           string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	req.AliasSession = strings.TrimSpace(req.AliasSession)
	req.PrimarySession = strings.TrimSpace(req.PrimarySession)
	if req.AliasSession == "" || req.PrimarySession == "" {
		writeError(w, "alias_session 与 primary_session 均不能为空", 400)
		return
	}
	if req.AliasSession == req.PrimarySession {
		writeError(w, "别名与主 session 不能相同", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Format("2006-01-02 15:04:05")
	_, err := s.db.Exec(
		`INSERT INTO user_bindings (alias_session, primary_session, bound_at, note)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(alias_session) DO UPDATE SET
		   primary_session=excluded.primary_session,
		   bound_at=excluded.bound_at,
		   note=excluded.note`,
		req.AliasSession, req.PrimarySession, now, req.Note,
	)
	if err != nil {
		writeError(w, "绑定失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":             true,
		"alias_session":  req.AliasSession,
		"primary_session": req.PrimarySession,
		"bound_at":       now,
		"message":        "绑定成功",
	})
}

// handleUnbind POST /unbind
// 解除别名 session 的绑定（主 session 不受影响）
func (s *Server) handleUnbind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		AliasSession string `json:"alias_session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	req.AliasSession = strings.TrimSpace(req.AliasSession)
	if req.AliasSession == "" {
		writeError(w, "alias_session 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec("DELETE FROM user_bindings WHERE alias_session = ?", req.AliasSession)
	if err != nil {
		writeError(w, "解绑失败: "+err.Error(), 500)
		return
	}
	n, _ := res.RowsAffected()

	writeJSON(w, map[string]interface{}{
		"ok":            true,
		"alias_session": req.AliasSession,
		"deleted":       n,
		"message":       "已解绑",
	})
}

// handleResolveSession POST /resolve-session
// 解析 session_id：若命中绑定返回主 session，否则原样返回
// 网关在读写记忆前调用此接口，实现跨平台记忆互通
func (s *Server) handleResolveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID == "" {
		writeError(w, "session_id 不能为空", 400)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var primary string
	err := s.db.QueryRow("SELECT primary_session FROM user_bindings WHERE alias_session = ?", req.SessionID).Scan(&primary)
	if err == nil && primary != "" {
		writeJSON(w, ResolveResult{Resolved: primary, Bound: true})
		return
	}

	writeJSON(w, ResolveResult{Resolved: req.SessionID, Bound: false})
}

// handleListBindings GET /list-bindings
// 列出全部绑定关系（调试/管理用）
func (s *Server) handleListBindings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT alias_session, primary_session, bound_at, note FROM user_bindings ORDER BY bound_at DESC")
	if err != nil {
		writeError(w, "查询绑定失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	bindings := make([]UserBinding, 0)
	for rows.Next() {
		var b UserBinding
		if err := rows.Scan(&b.AliasSession, &b.PrimarySession, &b.BoundAt, &b.Note); err != nil {
			writeError(w, "读取绑定失败: "+err.Error(), 500)
			return
		}
		bindings = append(bindings, b)
	}

	writeJSON(w, map[string]interface{}{
		"ok":       true,
		"bindings": bindings,
		"total":    len(bindings),
	})
}
