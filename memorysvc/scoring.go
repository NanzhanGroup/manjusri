// 业务逻辑：标签分类、重要性/置信度评分、无意义过滤、去重
package memorysvc

import (
	"regexp"
	"strings"
)

// ── 业务逻辑函数 ──

// AutoDetectTag 自动分类记忆标签
// 分类：emotion（情感/情绪）、growth（成长/觉醒）、insight（洞见/领悟）
//
//	profile（个人设定）、preference（偏好）、note（笔记）、knowledge（知识）
//
// 让记忆系统从"记事实"变成"记人心"——优先捕捉情绪、成长和洞见
func AutoDetectTag(content string) string {
	content = strings.ToLower(content)

	// emotion — 情感/情绪（优先级最高，因为记的是人心）
	emotionKeywords := []string{
		"难过", "伤心", "开心", "快乐", "痛苦", "焦虑", "害怕", "孤独",
		"感动", "温暖", "失落", "迷茫", "困惑", "空虚", "满足", "平静",
		"愤怒", "委屈", "不安", "期待", "释然", "感恩", "幸福",
		"心里", "心情", "感觉", "感受", "情绪",
		"苦", "累", "痛", "怕",
	}
	for _, kw := range emotionKeywords {
		if strings.Contains(content, kw) {
			return "emotion"
		}
	}

	// growth — 成长/觉醒（第二优先级）
	growthKeywords := []string{
		"悟了", "明白了", "懂了", "觉醒", "成长", "改变", "放下",
		"接受", "释怀", "看开", "想通", "领悟", "觉悟",
		"自在", "自省", "觉察", "接纳", "修行",
	}
	for _, kw := range growthKeywords {
		if strings.Contains(content, kw) {
			return "growth"
		}
	}

	// insight — 洞见/领悟
	insightKeywords := []string{
		"发现", "意识到", "认识到", "原来", "其实",
		"对我来说", "我的理解", "我的体会",
	}
	for _, kw := range insightKeywords {
		if strings.Contains(content, kw) {
			return "insight"
		}
	}

	// profile — 个人身份/设定
	profileKeywords := []string{
		"我叫", "我是", "我的名字", "我来自", "我住在", "我今年", "我的职业",
		"我是个", "我是名", "我的生日", "我的性别", "我的家乡",
	}
	for _, kw := range profileKeywords {
		if strings.Contains(content, kw) {
			return "profile"
		}
	}

	// preference — 偏好/喜好
	prefKeywords := []string{
		"我喜欢", "我不喜欢", "我爱", "我讨厌", "我最爱", "我偏爱",
		"我习惯", "我的爱好", "我的兴趣", "我想吃", "我想去",
	}
	for _, kw := range prefKeywords {
		if strings.Contains(content, kw) {
			return "preference"
		}
	}

	// config — 配置/设置
	configKeywords := []string{
		"设置", "配置", "开启", "关闭", "启用", "禁用",
		"设为", "改成", "调整为", "默认",
	}
	for _, kw := range configKeywords {
		if strings.Contains(content, kw) {
			return "config"
		}
	}

	// knowledge — 知识/信息（含"是"的陈述句、含专业术语）
	knowledgePatterns := []string{
		"是指", "指的是", "意思是", "定义为", "又称",
		"是一种", "是一个", "属于", "位于", "成立于",
	}
	for _, kw := range knowledgePatterns {
		if strings.Contains(content, kw) {
			return "knowledge"
		}
	}

	return "note"
}

// AutoScoreImportance 自动评分记忆重要性（0/1/2）
// 把"记人心"放在第一位：情绪表达、成长感悟、个人经历 → 高重要性
func AutoScoreImportance(content string) int {
	content = strings.ToLower(content)

	// 高分（2）：强烈情感、生命感悟、重要人生变化
	highScore := []string{
		"非常重要", "极其重要", "绝对", "必须", "务必",
		"永远", "始终", "核心", "关键", "根本",
		"最爱", "最讨厌", "不能", "禁止", "重要",
		"必须记住", "重中之重",
		// 情绪高分
		"痛苦", "绝望", "崩溃", "害怕", "恐惧",
		"感动", "幸福", "温暖", "感恩", "释然",
		"觉醒", "悟了", "放下",
	}
	for _, kw := range highScore {
		if strings.Contains(content, kw) {
			return 2
		}
	}

	// 中分（1）：情绪表达、个人成长、一般信息
	mediumScore := []string{
		// 情绪类
		"难过", "开心", "快乐", "焦虑", "孤独", "失落", "迷茫", "困惑",
		"委屈", "不安", "期待", "平静", "空虚", "满足",
		"感觉", "感受", "心情", "心里", "情绪",
		// 成长类
		"明白了", "懂了", "改变", "接受", "释怀", "看开", "想通",
		"领悟", "觉悟", "成长", "自省", "觉察", "接纳", "修行",
		"自在", "意识到", "认识到", "发现",
		// 一般重要
		"是", "有", "在", "可以", "需要",
		"觉得", "认为", "想", "要", "会",
		"请记住", "别忘了", "记住", "重点",
	}

	// 个人信息关键词 → 重要性+1
	profilePatterns := []string{"我叫", "我是", "我的", "我住在", "我今年", "电话", "邮箱"}
	for _, p := range profilePatterns {
		if strings.Contains(content, p) {
			return 1
		}
	}

	for _, kw := range mediumScore {
		if strings.Contains(content, kw) {
			return 1
		}
	}

	// 低分（0）：默认
	return 0
}

// AutoScoreConfidence 评估一条记忆的置信度（0~5）
// 场景：从对话中自动提炼的记忆，置信度取决于来源、重复次数等
func AutoScoreConfidence(content string, occurrenceCount int) int {
	score := 0

	// 用户明确要求记住 → 高置信度
	if strings.Contains(content, "请记住") || strings.Contains(content, "务必记住") {
		score += 2
	}

	// 包含具体事实（数字、日期、专有名词）→ 较高置信度
	if containsConcreteInfo(content) {
		score += 1
	}

	// 多次提及（occurrenceCount >= 2）→ 加置信度
	if occurrenceCount >= 2 {
		score += 1
	}
	if occurrenceCount >= 5 {
		score += 1
	}

	// 上限 5
	if score > 5 {
		score = 5
	}
	return score
}

// containsConcreteInfo 判断是否包含具体信息：数字、日期、专有名词特征
func containsConcreteInfo(s string) bool {
	hasNumber := regexp.MustCompile(`\d+`).MatchString(s)
	hasDate := regexp.MustCompile(`\d{4}[-/年]\d{1,2}[-/月]\d{1,2}`).MatchString(s)
	return hasNumber || hasDate
}

// IsMeaningless 判断内容是否无意义（返回 true 表示无意义，应跳过）
func IsMeaningless(content string) bool {
	if len(strings.TrimSpace(content)) < 3 {
		return true
	}

	// 纯标点符号或特殊字符
	trimmed := strings.TrimSpace(content)
	re := regexp.MustCompile(`^[\s\p{P}\p{S}]+$`)
	if re.MatchString(trimmed) {
		return true
	}

	// 纯数字
	reDigits := regexp.MustCompile(`^\d+$`)
	if reDigits.MatchString(trimmed) {
		return true
	}

	// 单字重复（如：哈哈哈、啊啊啊）
	if len(trimmed) >= 3 {
		allSame := true
		for i := 1; i < len(trimmed); i++ {
			if trimmed[i] != trimmed[0] {
				allSame = false
				break
			}
		}
		if allSame {
			return true
		}
	}

	// 常见的无意义填充词（保留单字回应如"嗯""好""对"等简短互动）
	meaninglessPhrases := []string{
		"test", "测试", "121", "asdf",
	}
	for _, phrase := range meaninglessPhrases {
		if strings.TrimSpace(content) == phrase {
			return true
		}
	}

	return false
}

// isDuplicate 检查 content 是否已存在于数据库中
func (s *Server) isDuplicate(content string) bool {
	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM memories WHERE content = ?", content,
	).Scan(&count)
	if err != nil {
		return false
	}
	return count > 0
}
