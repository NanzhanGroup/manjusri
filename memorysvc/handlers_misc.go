// 智能评分 / 记忆合并 / 总结 HTTP 处理器
package memorysvc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ── 智能评分 ──

// handleAutoScore POST /auto-score
// 评估一段文本的标签、重要性、置信度
func (s *Server) handleAutoScore(w http.ResponseWriter, r *http.Request) {
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

	// 计算该内容在 memories 表中出现的次数（用于置信度评分）
	s.mu.RLock()
	var occurrenceCount int
	s.db.QueryRow("SELECT COUNT(*) FROM memories WHERE content = ?", req.Content).Scan(&occurrenceCount)
	s.mu.RUnlock()

	tag := AutoDetectTag(req.Content)
	importance := AutoScoreImportance(req.Content)
	confidence := AutoScoreConfidence(req.Content, occurrenceCount)

	writeJSON(w, map[string]interface{}{
		"ok":         true,
		"tag":        tag,
		"importance": importance,
		"confidence": confidence,
	})
}

// ── 记忆合并/总结 ──

// handleMergeByTag POST /memories/merge-by-tag
// 按标签合并同主题记忆
func (s *Server) handleMergeByTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		Tag        string `json:"tag"`
		MaxAgeDays int    `json:"max_age_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.Tag == "" {
		writeError(w, "tag 不能为空", 400)
		return
	}
	if req.MaxAgeDays <= 0 || req.MaxAgeDays > 365 {
		req.MaxAgeDays = 30
	}

	cutoff := time.Now().AddDate(0, 0, -req.MaxAgeDays).Format("2006-01-02 15:04:05")

	s.mu.Lock()
	defer s.mu.Unlock()

	// 查询该标签下所有记忆
	rows, err := s.db.Query(
		"SELECT id, content FROM memories WHERE tag = ? AND created_at >= ? ORDER BY id DESC",
		req.Tag, cutoff,
	)
	if err != nil {
		writeError(w, "查询记忆失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	type rec struct {
		id      int64
		content string
	}
	var records []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.content); err != nil {
			continue
		}
		records = append(records, r)
	}

	if len(records) < 3 {
		writeJSON(w, map[string]interface{}{
			"ok":           true,
			"merged_count": 0,
			"result":       "",
			"reason":       fmt.Sprintf("记忆数 %d < 3，跳过合并", len(records)),
		})
		return
	}

	// 去重合并内容
	seen := make(map[string]bool)
	var uniqueLines []string
	maxImportance := 0
	for _, rec := range records {
		line := strings.TrimSpace(rec.content)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		uniqueLines = append(uniqueLines, line)
		// 取重要性最大值
		imp := AutoScoreImportance(line)
		if imp > maxImportance {
			maxImportance = imp
		}
	}

	mergedText := strings.Join(uniqueLines, "\n")

	// 删除旧碎片
	for _, rec := range records {
		s.db.Exec("DELETE FROM memories WHERE id = ?", rec.id)
	}

	// 写入合并结果
	s.db.Exec(
		"INSERT INTO memories (content, tag, importance, created_at) VALUES (?, ?, ?, ?)",
		mergedText, req.Tag, maxImportance, time.Now().Format("2006-01-02 15:04:05"),
	)

	writeJSON(w, map[string]interface{}{
		"ok":           true,
		"merged_count": len(records),
		"result":       mergedText,
	})
}

// handleSummarizeMemories POST /memories/summarize
// 对旧记忆做精简
func (s *Server) handleSummarizeMemories(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "仅支持 POST", 405)
		return
	}

	var req struct {
		MaxDays     int `json:"max_days"`
		MaxMemories int `json:"max_memories"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "请求体解析失败: "+err.Error(), 400)
		return
	}
	if req.MaxDays <= 0 || req.MaxDays > 365 {
		req.MaxDays = 60
	}
	if req.MaxMemories <= 0 || req.MaxMemories > 1000 {
		req.MaxMemories = 100
	}

	cutoff := time.Now().AddDate(0, 0, -req.MaxDays).Format("2006-01-02 15:04:05")

	s.mu.Lock()
	defer s.mu.Unlock()

	// 查询超过截止时间的记忆，按 tag 分组
	rows, err := s.db.Query(
		"SELECT id, tag, content, importance FROM memories WHERE created_at < ? ORDER BY tag, created_at DESC",
		cutoff,
	)
	if err != nil {
		writeError(w, "查询记忆失败: "+err.Error(), 500)
		return
	}
	defer rows.Close()

	type rec struct {
		id         int64
		tag        string
		content    string
		importance int
	}
	var allRecs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.tag, &r.content, &r.importance); err != nil {
			continue
		}
		allRecs = append(allRecs, r)
	}

	// 按 tag 分组，每组保留最新的 3 条
	tagGroups := make(map[string][]rec)
	for _, r := range allRecs {
		tagGroups[r.tag] = append(tagGroups[r.tag], r)
	}

	totalDeleted := 0
	totalKept := 0
	for tag, group := range tagGroups {
		if len(group) <= 3 {
			totalKept += len(group)
			continue
		}
		// 保留前 3 条（已按 created_at DESC 排序）
		keep := group[:3]
		delete := group[3:]

		for _, d := range delete {
			s.db.Exec("DELETE FROM memories WHERE id = ?", d.id)
			totalDeleted++
		}
		totalKept += len(keep)
		_ = tag // tag 用于分组标识
	}

	writeJSON(w, map[string]interface{}{
		"ok":            true,
		"deleted_count": totalDeleted,
		"kept_count":    totalKept,
	})
}
