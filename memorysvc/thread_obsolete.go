// 作废指令处理（翻篇）
package memorysvc

import (
	"fmt"
	"regexp"
	"strings"
)

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
