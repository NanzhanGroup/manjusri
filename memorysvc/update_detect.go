// 语义更新检测：识别新内容是否是对已有记忆的更新
package memorysvc

import (
	"strings"
)

// ── 语义更新检测 ──

// entityPatterns 可更新的属性实体模式
// key=实体类型, values=触发关键词（新内容中包含即认为涉及该实体）
var entityPatterns = map[string][]string{
	"地址": {"搬到", "搬家", "住址", "地址", "家住", "住在", "迁到", "新家", "家庭地址", "家庭住址", "小区", "住宅"},
	"工作": {"换工作", "跳槽", "新公司", "现在在", "入职", "离职", "上班", "工作单位", "职位", "就职", "工作在", "任职", "开发", "工程师", "后端", "前端", "全栈", "产品经理", "设计师", "运维", "算法", "测试"},
	"电话": {"换号", "新手机", "新电话", "手机号", "电话号码", "联系方式", "电话", "手机", "联系"},
	"邮箱": {"新邮箱", "换邮箱", "邮箱地址", "电子邮件", "邮箱", "邮件"},
	"姓名": {"改名为", "现在叫", "新名字", "改名", "姓名", "名字", "称呼", "昵称"},
	"学校": {"转学", "新学校", "考上", "入学", "就读", "学校", "毕业", "大学", "学院", "专业"},
	"生日": {"生日", "出生日期", "出生", "岁数", "年龄"},
	"关系": {"分手", "结婚", "离婚", "新对象", "女朋友", "男朋友", "配偶", "伴侣", "妻子", "丈夫", "老婆", "老公"},
	"宠物": {"新宠物", "养了", "买了只", "领养", "宠物", "狗", "猫"},
	"爱好": {"新爱好", "开始喜欢", "最近迷上", "兴趣", "爱好", "喜欢", "爱玩"},
	"车辆": {"新车", "换车", "买了车", "车牌", "座驾", "车子", "汽车"},
	"账户": {"新账号", "换账号", "用户名", "账户", "账号", "密码"},
}

// extractEntityType 从内容中提取涉及的属性实体类型
// 返回实体类型字符串，未匹配返回 ""
func extractEntityType(content string) string {
	contentLower := strings.ToLower(content)
	for entityType, patterns := range entityPatterns {
		for _, p := range patterns {
			if strings.Contains(contentLower, p) {
				return entityType
			}
		}
	}
	return ""
}

// extractSearchKeywords 从内容中提取用于搜索旧记忆的关键词
// 优先提取实体关键词，再补充其他显著名词
func extractSearchKeywords(content string) []string {
	keywords := make(map[string]bool)

	// 1. 实体模式关键词
	for _, patterns := range entityPatterns {
		for _, p := range patterns {
			if strings.Contains(content, p) {
				keywords[p] = true
			}
		}
	}

	// 2. 如果没匹配到实体，提取内容前 10 个字作为搜索词
	if len(keywords) == 0 {
		runes := []rune(strings.TrimSpace(content))
		if len(runes) > 10 {
			keywords[string(runes[:10])] = true
		} else if len(runes) > 3 {
			keywords[string(runes)] = true
		}
	}

	result := make([]string, 0, len(keywords))
	for k := range keywords {
		result = append(result, k)
	}
	return result
}

// autoDetectUpdate 检测新内容是否是对已有记忆的更新
// 返回：(是否更新, 旧记忆ID, 新内容)
// 如果检测到更新，调用方应 UPDATE 旧记录而非 INSERT 新记录
func (s *Server) autoDetectUpdate(newContent string) (shouldUpdate bool, oldID int64, newContentOut string) {
	// 1. 提取新内容的实体类型
	entityType := extractEntityType(newContent)
	if entityType == "" {
		return false, 0, ""
	}

	// 2. 用该实体类型的**全部模式关键词**搜索已有记忆
	//    例如新内容有"搬到"→实体类型"地址"→搜索"搬到/搬家/地址/住址/家住…"所有模式
	//    这样旧内容的"地址"也能被命中
	patterns := entityPatterns[entityType]
	if len(patterns) == 0 {
		return false, 0, ""
	}

	whereClause := ""
	args := []interface{}{}
	for i, p := range patterns {
		if i > 0 {
			whereClause += " OR "
		}
		whereClause += "content LIKE ?"
		args = append(args, "%"+p+"%")
	}

	query := "SELECT id, content FROM memories WHERE " + whereClause + " ORDER BY created_at DESC LIMIT 5"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return false, 0, ""
	}
	defer rows.Close()

	type candidate struct {
		id      int64
		content string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.content); err != nil {
			continue
		}
		candidates = append(candidates, c)
	}

	if len(candidates) == 0 {
		return false, 0, ""
	}

	// 3. 检查候选记忆是否与新内容涉及同一实体类型
	for _, c := range candidates {
		oldEntityType := extractEntityType(c.content)
		if oldEntityType == "" {
			continue
		}
		// 同一实体类型 → 这是更新！（如旧"地址"+新"地址"，内容不同但实体一致）
		if oldEntityType == entityType {
			// 但如果新旧内容完全相同 → 是重复，不是更新（由上游 isDuplicate 处理）
			if c.content == newContent {
				continue
			}
			return true, c.id, newContent
		}
	}

	// 4. 没找到匹配的实体对 → 不是更新
	return false, 0, ""
}
