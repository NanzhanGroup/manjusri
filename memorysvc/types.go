// Package memorysvc — 记忆服务（独占 SQLite，Unix Socket RPC）
//
// 与 sessionsvc 相同架构，统一管理 memory.db。
// chat、api-server、微信网关都通过本服务读写记忆，消除并发锁冲突。
package memorysvc

// MemoryItem 单条记忆数据
type MemoryItem struct {
	ID         int64  `json:"id"`
	Content    string `json:"content"`
	CreatedAt  string `json:"created_at"`
	Tag        string `json:"tag"`
	Importance int    `json:"importance"`
}

// Session 会话元数据
// 由 memory-service 统一管理（独占 SQLite）
type Session struct {
	SessionID    string `json:"session_id"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	MessageCount int    `json:"message_count"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	Tags         string `json:"tags"`
	// gateway_session 兼容字段
	UserID      string `json:"user_id"`
	NickName    string `json:"nick_name"`
	Platform    string `json:"platform"`
	Processed   bool   `json:"processed"`
	ProcessedAt string `json:"processed_at"`
}

// Message 单条消息流水
type Message struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	Tokens    int    `json:"tokens"`
	Metadata  string `json:"metadata,omitempty"`
}

// ── 上下文拼装 ──

// ContextResult 上下文拼装结果
type ContextResult struct {
	SystemPrompt string       `json:"system_prompt"`
	Memories     string       `json:"memories"`
	History      string       `json:"history"`
	Meta         ContextMeta  `json:"meta"`
}

// ContextMeta 上下文元数据
type ContextMeta struct {
	MemoryCount        int    `json:"memory_count"`
	HistoryCount       int    `json:"history_count"`
	MemorySource       string `json:"memory_source"`
	ThreadActiveCount  int    `json:"thread_active_count"`
	ThreadObsoleteCount int   `json:"thread_obsolete_count"`
}

// ── 智能评分 ──

// ScoreResult 智能评分结果
type ScoreResult struct {
	Tag        string `json:"tag"`
	Importance int    `json:"importance"`
	Confidence int    `json:"confidence"`
}

// ── 记忆合并 ──

// MergeResult 合并结果
type MergeResult struct {
	DeletedCount int    `json:"deleted_count"`
	Result       string `json:"result"`
}

// SummarizeResult 精简结果
type SummarizeResult struct {
	DeletedCount int `json:"deleted_count"`
	KeptCount    int `json:"kept_count"`
}

// ── 线程管理 ──

// Thread 会话线程
type Thread struct {
	ID          int64  `json:"id"`
	SessionID   string `json:"session_id"`
	Title       string `json:"title"`
	Status      string `json:"status"` // active / obsolete / milestone
	StartMsgID  *int64 `json:"start_msg_id"`
	EndMsgID    *int64 `json:"end_msg_id"`
	SupersededBy *int64 `json:"superseded_by"`
	Summary     string `json:"summary"`
	CreatedAt   string `json:"created_at"`
	ClosedAt    *string `json:"closed_at"`
}

// ThreadMessage 线程中的消息（含摘要）
type ThreadMessage struct {
	ID        int64  `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// ThreadListResult 线程列表结果
type ThreadListResult struct {
	Threads []Thread `json:"threads"`
	Total   int      `json:"total"`
}

// ── 消息追加结果 ──

// AppendMessageResult 追加消息的完整返回结果
type AppendMessageResult struct {
	ID              int64  `json:"id"`
	ThreadID        int64  `json:"thread_id"`
	ThreadSwitched  bool   `json:"thread_switched"`
	NewThreadID     int64  `json:"new_thread_id"`
	ObsoleteDetected bool  `json:"obsolete_detected"`
	ObsoleteAction  string `json:"obsolete_action"`
	ObsoleteReason  string `json:"obsolete_reason"`
}

// ── 跨平台用户绑定 ──

// UserBinding 跨平台身份绑定：把 alias session（如 wecom_xxx）映射到主 session（如 weixin_xxx）
// 绑定后 alias 平台的消息读写都落到主 session，实现跨平台记忆互通
type UserBinding struct {
	AliasSession   string `json:"alias_session"`   // 别名 session（发起绑定的一方）
	PrimarySession string `json:"primary_session"` // 主 session（记忆归属方）
	BoundAt        string `json:"bound_at"`
	Note           string `json:"note"`
}

// ResolveResult 会话解析结果
type ResolveResult struct {
	Resolved string `json:"resolved"` // 解析后的 session_id（无绑定则原样返回）
	Bound    bool   `json:"bound"`    // 是否命中绑定
}
