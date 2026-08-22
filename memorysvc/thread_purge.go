// 线程彻底清理（purge）——长期生效规矩：只删线程记录，消息归档不物理删除
package memorysvc

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// handleThreadPurge POST /threads/purge
// 请求: {"session_id":"...", "thread_ids":[1,2,3]}   // thread_ids 可选，缺省清理该 session 所有 obsolete 线程
// 响应: {"ok":true, "purged_threads":N, "archived_messages":M, "kept_messages":K}
//
// 设计原则（长期生效，固化于代码）：
//   1. 只处理 status='obsolete' 的线程，绝不误删 active 线程
//   2. 消息绝不物理删除：先完整归档进 archived_messages（INSERT OR IGNORE，保留原 id 防重复），
//      messages 表原记录原样保留（双份保全）
//   3. 仅删除线程记录本身（session_threads 行）
//   4. 已删线程的消息因 thread_id 指向已不存在的线程，不参与 /context/build 的 active 查询，不会混入当前上下文
func (s *Server) handleThreadPurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		SessionID string  `json:"session_id"`
		ThreadIDs []int64 `json:"thread_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid json", 400)
		return
	}
	if req.SessionID == "" {
		writeError(w, "session_id required", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 确定目标线程：仅 obsolete
	rows, err := s.db.Query(
		`SELECT id FROM session_threads WHERE session_id=? AND status='obsolete'`, req.SessionID)
	if err != nil {
		writeError(w, fmt.Sprintf("query threads: %v", err), 500)
		return
	}
	allowed := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			writeError(w, fmt.Sprintf("scan thread: %v", err), 500)
			return
		}
		allowed[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeError(w, fmt.Sprintf("query threads err: %v", err), 500)
		return
	}

	var targetIDs []int64
	if len(req.ThreadIDs) > 0 {
		for _, id := range req.ThreadIDs {
			if allowed[id] {
				targetIDs = append(targetIDs, id)
			}
		}
	} else {
		for id := range allowed {
			targetIDs = append(targetIDs, id)
		}
	}

	if len(targetIDs) == 0 {
		writeJSON(w, map[string]interface{}{
			"ok": true, "purged_threads": 0, "archived_messages": 0, "kept_messages": 0,
		})
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, fmt.Sprintf("begin tx: %v", err), 500)
		return
	}
	defer tx.Rollback()

	purged := 0
	archived := 0
	kept := 0
	for _, tid := range targetIDs {
		// 1. 归档消息（不删除原记录；保留原 id，重复归档自动忽略）
		res, err := tx.Exec(`INSERT OR IGNORE INTO archived_messages
			(id, session_id, role, content, created_at, tokens, thread_id, metadata, archived_at, archived_from)
			SELECT id, session_id, role, content, created_at, tokens, thread_id, metadata,
			       datetime('now','localtime'), 'thread_purge'
			FROM messages WHERE thread_id=?`, tid)
		if err != nil {
			writeError(w, fmt.Sprintf("archive messages thread %d: %v", tid, err), 500)
			return
		}
		n, _ := res.RowsAffected()
		archived += int(n)

		// 2. 统计 messages 中保留的消息数（只删线程记录，消息原样保留）
		var cnt int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE thread_id=?`, tid).Scan(&cnt); err != nil {
			writeError(w, fmt.Sprintf("count messages thread %d: %v", tid, err), 500)
			return
		}
		kept += cnt

		// 3. 删除线程记录（再次限定 obsolete，双保险）
		if _, err := tx.Exec(`DELETE FROM session_threads WHERE id=? AND status='obsolete'`, tid); err != nil {
			writeError(w, fmt.Sprintf("delete thread %d: %v", tid, err), 500)
			return
		}
		purged++
	}

	if err := tx.Commit(); err != nil {
		writeError(w, fmt.Sprintf("commit tx: %v", err), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":               true,
		"purged_threads":   purged,
		"archived_messages": archived,
		"kept_messages":    kept,
	})
}
