// 分层搜索：今天 → 近7天 → 近30天 → 近90天
package memorysvc

import (
	"strings"
	"time"
)

// ── 分层搜索 ──

// SearchLayers 按层级搜索记忆
// 搜索顺序：今天 → 近7天 → 近30天 → 近90天，找到即止
func (s *Server) SearchLayers(query string) (found bool, source string, content string) {
	if query == "" {
		return false, "", ""
	}

	now := time.Now()
	today := now.Format("2006-01-02")
	weekAgo := now.AddDate(0, 0, -7).Format("2006-01-02 15:04:05")
	monthAgo := now.AddDate(0, 0, -30).Format("2006-01-02 15:04:05")
	quarterAgo := now.AddDate(0, 0, -90).Format("2006-01-02 15:04:05")

	like := "%" + query + "%"
	limit := 10

	// 1. 今天
	if c := s.searchDB(like, today+" 00:00:00", today+" 23:59:59", limit); c != "" {
		return true, "今天", c
	}
	// 2. 近7天（不含今天）
	if c := s.searchDB(like, weekAgo, today+" 00:00:00", limit); c != "" {
		return true, "近7天", c
	}
	// 3. 近30天（不含近7天）
	if c := s.searchDB(like, monthAgo, weekAgo, limit); c != "" {
		return true, "近30天", c
	}
	// 4. 近90天（不含近30天）
	if c := s.searchDB(like, quarterAgo, monthAgo, limit); c != "" {
		return true, "近90天", c
	}

	return false, "", ""
}

// SearchAllLayers 搜索所有层级，返回全部结果
func (s *Server) SearchAllLayers(query string) map[string]string {
	results := make(map[string]string)
	if query == "" {
		return results
	}

	now := time.Now()
	today := now.Format("2006-01-02")
	weekAgo := now.AddDate(0, 0, -7).Format("2006-01-02 15:04:05")
	monthAgo := now.AddDate(0, 0, -30).Format("2006-01-02 15:04:05")
	quarterAgo := now.AddDate(0, 0, -90).Format("2006-01-02 15:04:05")
	like := "%" + query + "%"
	limit := 10

	if c := s.searchDB(like, today+" 00:00:00", today+" 23:59:59", limit); c != "" {
		results["今天"] = c
	}
	if c := s.searchDB(like, weekAgo, today+" 00:00:00", limit); c != "" {
		results["近7天"] = c
	}
	if c := s.searchDB(like, monthAgo, weekAgo, limit); c != "" {
		results["近30天"] = c
	}
	if c := s.searchDB(like, quarterAgo, monthAgo, limit); c != "" {
		results["近90天"] = c
	}

	return results
}

// searchDB 在时间范围内搜索记忆
func (s *Server) searchDB(like, since, until string, limit int) string {
	rows, err := s.db.Query(
		`SELECT content FROM memories
		 WHERE content LIKE ? AND created_at >= ? AND created_at <= ?
		 ORDER BY created_at DESC LIMIT ?`,
		like, since, until, limit,
	)
	if err != nil {
		return ""
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
	if len(results) == 0 {
		return ""
	}
	return strings.Join(results, "\n")
}
