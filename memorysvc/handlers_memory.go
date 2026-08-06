// 记忆相关 HTTP 处理器（追加、搜索、标签、清理、BASIC/SECURITY 文件）
package memorysvc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── HTTP 处理 ──

// handleAppend POST /append
// 写入记忆：过滤无意义 → 语义更新检测 → 去重 → AutoDetectTag → AutoScoreImportance → 入库
func (s *Server) handleAppend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.Content == "" {
		writeError(w, "content 不能为空", 400)
		return
	}

	// 过滤无意义内容
	if IsMeaningless(req.Content) {
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"skipped": true,
			"reason":  "无意义内容已跳过",
		})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// ═══ 步骤 1：语义更新检测 ═══
	// 检测新内容是否是对已有记忆的更新（如搬家换地址、换工作等）
	if shouldUpdate, oldID, _ := s.autoDetectUpdate(req.Content); shouldUpdate {
		tag := AutoDetectTag(req.Content)
		importance := AutoScoreImportance(req.Content)
		now := time.Now().Format("2006-01-02 15:04:05")

		_, err := s.db.Exec(
			"UPDATE memories SET content = ?, tag = ?, importance = ?, created_at = ? WHERE id = ?",
			req.Content, tag, importance, now, oldID,
		)
		if err != nil {
			writeError(w, "更新失败: "+err.Error(), 500)
			return
		}

		writeJSON(w, map[string]interface{}{
			"ok":         true,
			"skipped":    false,
			"updated":    true,
			"old_id":     oldID,
			"tag":        tag,
			"importance": importance,
			"reason":     "检测到「" + extractEntityType(req.Content) + "」信息更新",
		})
		return
	}

	// ═══ 步骤 2：全局去重 ═══
	if s.isDuplicate(req.Content) {
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"skipped": true,
			"reason":  "重复内容已跳过",
		})
		return
	}

	// ═══ 步骤 3：新增记忆 ═══
	tag := AutoDetectTag(req.Content)
	importance := AutoScoreImportance(req.Content)

	_, err := s.db.Exec(
		"INSERT INTO memories (content, tag, importance, created_at) VALUES (?, ?, ?, ?)",
		req.Content, tag, importance, time.Now().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		writeError(w, "写入失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":         true,
		"skipped":    false,
		"updated":    false,
		"tag":        tag,
		"importance": importance,
	})
}

// handleAppendWithOpts POST /append-with-opts
// 直接写入（跳过自动分类），支持指定 tag 和 importance
func (s *Server) handleAppendWithOpts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		Content    string `json:"content"`
		Tag        string `json:"tag"`
		Importance int    `json:"importance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.Content == "" {
		writeError(w, "content 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO memories (content, tag, importance, created_at) VALUES (?, ?, ?, ?)",
		req.Content, req.Tag, req.Importance, time.Now().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		writeError(w, "写入失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok": true,
	})
}

// handleSearchLayers GET /search-layers?query=xxx
// 分层搜索：今天 → 近7天 → 近30天 → 近90天，找到即止
func (s *Server) handleSearchLayers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, "query 参数不能为空", 400)
		return
	}

	s.mu.RLock()
	found, source, content := s.SearchLayers(query)
	s.mu.RUnlock()

	writeJSON(w, map[string]interface{}{
		"found":   found,
		"source":  source,
		"content": content,
	})
}

// handleSearchAll GET /search-all?query=xxx
// 搜索全部层级，返回所有层级的结果
func (s *Server) handleSearchAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, "query 参数不能为空", 400)
		return
	}

	s.mu.RLock()
	results := s.SearchAllLayers(query)
	s.mu.RUnlock()

	writeJSON(w, map[string]interface{}{
		"found":   len(results) > 0,
		"results": results,
	})
}

// handleSearchByTag GET /search-by-tag?tag=xxx&limit=20
// 按标签搜索记忆
func (s *Server) handleSearchByTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		writeError(w, "tag 参数不能为空", 400)
		return
	}
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		"SELECT content FROM memories WHERE tag = ? ORDER BY created_at DESC LIMIT ?",
		tag, limit,
	)
	if err != nil {
		writeError(w, "搜索失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			continue
		}
		results = append(results, content)
	}

	writeJSON(w, map[string]interface{}{
		"found":   len(results) > 0,
		"results": results,
	})
}

// handleCount GET /count
// 返回记忆总数
func (s *Server) handleCount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&count)
	if err != nil {
		writeError(w, "查询失败: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]int{"count": count})
}

// handleCleanup POST /cleanup
// 清理超过 90 天的记忆
func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -90).Format("2006-01-02 15:04:05")
	result, err := s.db.Exec("DELETE FROM memories WHERE created_at < ?", cutoff)
	if err != nil {
		writeError(w, "清理失败: "+err.Error(), 500)
		return
	}
	n, _ := result.RowsAffected()

	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"deleted": n,
	})
}

// handleUpdateTopic POST /update-topic
// {keyword, new_content} 按关键字更新记忆内容
func (s *Server) handleUpdateTopic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		Keyword    string `json:"keyword"`
		NewContent string `json:"new_content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.Keyword == "" || req.NewContent == "" {
		writeError(w, "keyword 和 new_content 不能为空", 400)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	like := "%" + req.Keyword + "%"
	result, err := s.db.Exec(
		"UPDATE memories SET content = ? WHERE content LIKE ?",
		req.NewContent, like,
	)
	if err != nil {
		writeError(w, "更新失败: "+err.Error(), 500)
		return
	}
	n, _ := result.RowsAffected()

	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"updated": n,
	})
}

// handleBasic GET/POST /basic
// GET → 读取 BASIC.MD；POST → 写入 BASIC.MD
func (s *Server) handleBasic(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		defer s.mu.RUnlock()
		content := s.readFile(filepath.Join(s.dataDir, "BASIC.MD"))
		writeJSON(w, map[string]string{"content": content})

	case http.MethodPost:
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "请求体解析失败: "+err.Error(), 400)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := os.WriteFile(filepath.Join(s.dataDir, "BASIC.MD"), []byte(req.Content), 0644); err != nil {
			writeError(w, "写入失败: "+err.Error(), 500)
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})

	default:
		writeError(w, "仅支持 GET/POST", 405)
	}
}

// handleSecurity GET /security
// 读取 SECURITY.MD（只读，由系统管理）
func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "仅支持 GET", 405)
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	content := s.readFile(filepath.Join(s.dataDir, "SECURITY.MD"))
	writeJSON(w, map[string]string{"content": content})
}

// readFile 读取文件内容，文件不存在返回空字符串
func (s *Server) readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
