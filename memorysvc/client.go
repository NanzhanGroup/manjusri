// Package memorysvc — 记忆服务客户端（Unix Socket RPC）
package memorysvc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ── 客户端 ──

// Client 记忆服务客户端，通过 Unix socket 调用 memory-service
type Client struct {
	socketPath string
	httpClient *http.Client
}

// unixDialer 创建 Unix socket 拨号器
func unixDialer(socketPath string) func(string, string) (net.Conn, error) {
	return func(_, _ string) (net.Conn, error) {
		return net.DialTimeout("unix", socketPath, 5*time.Second)
	}
}

// NewClient 创建记忆服务客户端
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				Dial: unixDialer(socketPath),
			},
			Timeout: 10 * time.Second,
		},
	}
}

// request 发送 HTTP 请求到 Unix socket
func (c *Client) request(method, path string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	url := "http://unix" + path
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed (is memory-service running?): %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, string(respData))
	}

	return respData, nil
}

// ── API 方法 ──

// Append 写入一条记忆（自动去重 + 过滤无意义内容）
func (c *Client) Append(content string) error {
	_, err := c.request("POST", "/append", map[string]string{
		"content": content,
	})
	return err
}

// AppendWithOpts 写入一条记忆（支持标签和重要性）
func (c *Client) AppendWithOpts(content, tag string, importance int) error {
	_, err := c.request("POST", "/append-with-opts", map[string]interface{}{
		"content":    content,
		"tag":        tag,
		"importance": importance,
	})
	return err
}

// SearchLayers 按层级搜索记忆（今天→近7天→近30天→近90天，找到即止）
func (c *Client) SearchLayers(query string) (found bool, source string, content string, err error) {
	data, reqErr := c.request("GET", "/search-layers?query="+query, nil)
	if reqErr != nil {
		return false, "", "", reqErr
	}
	var resp struct {
		Found   bool   `json:"found"`
		Source  string `json:"source"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, "", "", fmt.Errorf("parse response: %w", err)
	}
	return resp.Found, resp.Source, resp.Content, nil
}

// SearchAllLayers 搜索所有层级（不停止，返回全部结果）
func (c *Client) SearchAllLayers(query string) (map[string]string, error) {
	data, err := c.request("GET", "/search-all?query="+query, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results map[string]string `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return resp.Results, nil
}

// SearchByTag 按标签搜索记忆
func (c *Client) SearchByTag(tag string, limit int) (string, error) {
	path := fmt.Sprintf("/search-by-tag?tag=%s&limit=%d", tag, limit)
	data, err := c.request("GET", path, nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	return resp.Content, nil
}

// Count 返回记忆总数
func (c *Client) Count() (int, error) {
	data, err := c.request("GET", "/count", nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("parse response: %w", err)
	}
	return resp.Count, nil
}

// Cleanup 清理超过 90 天的记忆
func (c *Client) Cleanup() error {
	_, err := c.request("POST", "/cleanup", nil)
	return err
}

// UpdateTopic 按主题关键词更新记忆：删除含关键词的旧内容 → 写入新内容
func (c *Client) UpdateTopic(keyword, newContent string) (int, error) {
	data, err := c.request("POST", "/update-topic", map[string]string{
		"keyword":     keyword,
		"new_content": newContent,
	})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Deleted int `json:"deleted"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("parse response: %w", err)
	}
	return resp.Deleted, nil
}

// ReadBASIC 读取 BASIC.MD
func (c *Client) ReadBASIC() (string, error) {
	data, err := c.request("GET", "/basic", nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	return resp.Content, nil
}

// ReadSECURITY 读取 SECURITY.MD
func (c *Client) ReadSECURITY() (string, error) {
	data, err := c.request("GET", "/security", nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	return resp.Content, nil
}

// WriteBASIC 写入 BASIC.MD
func (c *Client) WriteBASIC(content string) error {
	_, err := c.request("POST", "/basic", map[string]string{
		"content": content,
	})
	return err
}

// ── 会话管理 ──

// CreateSession 创建会话
func (c *Client) CreateSession(sessionID, title, userID, nickName, platform string) error {
	_, err := c.request("POST", "/sessions/create", map[string]string{
		"session_id": sessionID,
		"title":      title,
		"user_id":    userID,
		"nick_name":  nickName,
		"platform":   platform,
	})
	return err
}

// UpdateSessionOptions 更新会话的可选参数
type UpdateSessionOptions struct {
	Title       string `json:"title,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Tags        string `json:"tags,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	NickName    string `json:"nick_name,omitempty"`
	Platform    string `json:"platform,omitempty"`
	Processed   *bool  `json:"processed,omitempty"`   // nil=不更新
	ProcessedAt string `json:"processed_at,omitempty"`
}

// UpdateSession 更新会话元数据
func (c *Client) UpdateSession(sessionID string, opts UpdateSessionOptions) error {
	body := map[string]interface{}{
		"session_id": sessionID,
	}
	if opts.Title != "" {
		body["title"] = opts.Title
	}
	if opts.Summary != "" {
		body["summary"] = opts.Summary
	}
	if opts.Tags != "" {
		body["tags"] = opts.Tags
	}
	if opts.UserID != "" {
		body["user_id"] = opts.UserID
	}
	if opts.NickName != "" {
		body["nick_name"] = opts.NickName
	}
	if opts.Platform != "" {
		body["platform"] = opts.Platform
	}
	if opts.Processed != nil {
		body["processed"] = *opts.Processed
	}
	if opts.ProcessedAt != "" {
		body["processed_at"] = opts.ProcessedAt
	}
	_, err := c.request("POST", "/sessions/update", body)
	return err
}

// GetSession 获取会话元数据
func (c *Client) GetSession(sessionID string) (*Session, error) {
	data, err := c.request("GET", "/sessions/get?session_id="+sessionID, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK      bool     `json:"ok"`
		Session *Session `json:"session"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("session not found")
	}
	return resp.Session, nil
}

// ListSessions 列出最近会话
func (c *Client) ListSessions(limit, offset int) ([]*Session, error) {
	path := fmt.Sprintf("/sessions/list?limit=%d&offset=%d", limit, offset)
	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK       bool       `json:"ok"`
		Sessions []*Session `json:"sessions"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return resp.Sessions, nil
}

// DeleteSession 删除会话及其所有消息
func (c *Client) DeleteSession(sessionID string) error {
	_, err := c.request("DELETE", "/sessions/delete?session_id="+sessionID, nil)
	return err
}

// FindSessionByUser GET /sessions/find-by-user?platform=xxx&user_id=xxx
// 按平台+用户查找活跃会话，返回 (found, session, error)
func (c *Client) FindSessionByUser(platform, userID string) (bool, *Session, error) {
	path := fmt.Sprintf("/sessions/find-by-user?platform=%s&user_id=%s", platform, userID)
	data, err := c.request("GET", path, nil)
	if err != nil {
		return false, nil, err
	}
	var resp struct {
		OK      bool     `json:"ok"`
		Found   bool     `json:"found"`
		Session *Session `json:"session"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, nil, fmt.Errorf("parse response: %w", err)
	}
	return resp.Found, resp.Session, nil
}

// UpdateSessionActivity POST /sessions/update-activity
// 更新会话的最后活动时间，可选更新状态
func (c *Client) UpdateSessionActivity(sessionID string, status ...string) error {
	body := map[string]interface{}{
		"session_id": sessionID,
	}
	if len(status) > 0 && status[0] != "" {
		body["status"] = status[0]
	}
	_, err := c.request("POST", "/sessions/update-activity", body)
	return err
}

// ── 消息管理 ──

// AppendMessage 追加消息到会话，返回完整结果（含线程信息）
func (c *Client) AppendMessage(sessionID, role, content string, tokens int, metadata ...string) (*AppendMessageResult, error) {
	params := map[string]interface{}{
		"session_id": sessionID,
		"role":       role,
		"content":    content,
		"tokens":     tokens,
	}
	if len(metadata) > 0 && metadata[0] != "" {
		params["metadata"] = metadata[0]
	}
	data, err := c.request("POST", "/messages/append", params)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK               bool   `json:"ok"`
		ID               int64  `json:"id"`
		ThreadID         int64  `json:"thread_id"`
		ThreadSwitched   bool   `json:"thread_switched"`
		NewThreadID      int64  `json:"new_thread_id"`
		ObsoleteDetected bool   `json:"obsolete_detected"`
		ObsoleteAction   string `json:"obsolete_action"`
		ObsoleteReason   string `json:"obsolete_reason"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse AppendMessage response: %w", err)
	}
	return &AppendMessageResult{
		ID:               resp.ID,
		ThreadID:         resp.ThreadID,
		ThreadSwitched:   resp.ThreadSwitched,
		NewThreadID:      resp.NewThreadID,
		ObsoleteDetected: resp.ObsoleteDetected,
		ObsoleteAction:   resp.ObsoleteAction,
		ObsoleteReason:   resp.ObsoleteReason,
	}, nil
}

// ListMessages 列出会话的消息（支持 before_id 游标分页）
func (c *Client) ListMessages(sessionID string, limit int, beforeID int64) ([]*Message, error) {
	path := fmt.Sprintf("/messages/list?session_id=%s&limit=%d&before_id=%d", sessionID, limit, beforeID)
	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK       bool       `json:"ok"`
		Messages []*Message `json:"messages"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return resp.Messages, nil
}

// TrimMessages 按 token 预算截断早期消息
type TrimResult struct {
	TrimmedCount int    `json:"trimmed_count"`
	Summary      string `json:"summary"`
	Remaining    int    `json:"remaining"`
}

func (c *Client) TrimMessages(sessionID string, maxTokens int) (*TrimResult, error) {
	data, err := c.request("POST", "/messages/trim", map[string]interface{}{
		"session_id": sessionID,
		"max_tokens": maxTokens,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK           bool   `json:"ok"`
		TrimmedCount int    `json:"trimmed_count"`
		Summary      string `json:"summary"`
		Remaining    int    `json:"remaining"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &TrimResult{
		TrimmedCount: resp.TrimmedCount,
		Summary:      resp.Summary,
		Remaining:    resp.Remaining,
	}, nil
}

// ── 线程管理 ──

// ListThreads 列出会话的线程，status 为空则列出全部
func (c *Client) ListThreads(sessionID, status string) (*ThreadListResult, error) {
	path := fmt.Sprintf("/threads/list?session_id=%s&status=%s", sessionID, status)
	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK      bool     `json:"ok"`
		Threads []Thread `json:"threads"`
		Total   int      `json:"total"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse ListThreads response: %w", err)
	}
	return &ThreadListResult{
		Threads: resp.Threads,
		Total:   resp.Total,
	}, nil
}

// GetThread 获取线程详情及其消息
func (c *Client) GetThread(id int64) (*Thread, []ThreadMessage, error) {
	path := fmt.Sprintf("/threads/get?id=%d", id)
	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, nil, err
	}
	var resp struct {
		OK       bool           `json:"ok"`
		Thread   Thread         `json:"thread"`
		Messages []ThreadMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, nil, fmt.Errorf("parse GetThread response: %w", err)
	}
	return &resp.Thread, resp.Messages, nil
}

// CloseThread 关闭（作废）指定线程
func (c *Client) CloseThread(sessionID string, threadID int64) error {
	data, err := c.request("POST", "/threads/close", map[string]interface{}{
		"session_id": sessionID,
		"thread_id":  threadID,
	})
	if err != nil {
		return err
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("parse CloseThread response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("close thread failed")
	}
	return nil
}

// ReactivateThread 重新激活已关闭的线程
func (c *Client) ReactivateThread(sessionID string, threadID int64) error {
	data, err := c.request("POST", "/threads/reactivate", map[string]interface{}{
		"session_id": sessionID,
		"thread_id":  threadID,
	})
	if err != nil {
		return err
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("parse ReactivateThread response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("reactivate thread failed")
	}
	return nil
}

// DiscardEpoch POST /discard-epoch
// 批量将消息标记为已作废（翻篇用）
func (c *Client) DiscardEpoch(sessionKey string, msgIDs []string, epochID int) error {
	_, err := c.request("POST", "/discard-epoch", map[string]interface{}{
		"session_key": sessionKey,
		"msg_ids":     msgIDs,
		"epoch_id":    epochID,
	})
	return err
}

// GetDiscardedMsgIDs GET /discarded-msgids?session_key=xxx
// 查询某 session 所有已作废的 msg_id
func (c *Client) GetDiscardedMsgIDs(sessionKey string) (map[string]bool, error) {
	data, err := c.request("GET", "/discarded-msgids?session_key="+sessionKey, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK     bool           `json:"ok"`
		MsgIDs map[string]bool `json:"msg_ids"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return resp.MsgIDs, nil
}

// ── 上下文拼装 ──

// BuildContext 拼装上下文
func (c *Client) BuildContext(sessionID, query string, maxMessages, maxMemories int, includeObsolete bool) (*ContextResult, error) {
	data, err := c.request("POST", "/context/build", map[string]interface{}{
		"session_id":      sessionID,
		"query":           query,
		"max_messages":    maxMessages,
		"max_memories":    maxMemories,
		"include_obsolete": includeObsolete,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK      bool           `json:"ok"`
		Context *ContextResult `json:"context"`
		Meta    *ContextMeta   `json:"meta"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse BuildContext response: %w", err)
	}
	if resp.Context != nil {
		resp.Context.Meta = *resp.Meta
	}
	return resp.Context, nil
}

// ── 智能评分 ──

// AutoScore 评估一段文本的标签、重要性、置信度
func (c *Client) AutoScore(content string) (*ScoreResult, error) {
	data, err := c.request("POST", "/auto-score", map[string]string{
		"content": content,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK         bool   `json:"ok"`
		Tag        string `json:"tag"`
		Importance int    `json:"importance"`
		Confidence int    `json:"confidence"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &ScoreResult{
		Tag:        resp.Tag,
		Importance: resp.Importance,
		Confidence: resp.Confidence,
	}, nil
}

// ── 记忆合并/总结 ──

// MergeByTag 按标签合并同主题记忆
func (c *Client) MergeByTag(tag string, maxAgeDays int) (int, string, error) {
	data, err := c.request("POST", "/memories/merge-by-tag", map[string]interface{}{
		"tag":          tag,
		"max_age_days": maxAgeDays,
	})
	if err != nil {
		return 0, "", err
	}
	var resp struct {
		OK          bool   `json:"ok"`
		MergedCount int    `json:"merged_count"`
		Result      string `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, "", fmt.Errorf("parse response: %w", err)
	}
	return resp.MergedCount, resp.Result, nil
}

// SummarizeMemories 对旧记忆做精简
func (c *Client) SummarizeMemories(maxDays, maxMemories int) (*SummarizeResult, error) {
	data, err := c.request("POST", "/memories/summarize", map[string]interface{}{
		"max_days":     maxDays,
		"max_memories": maxMemories,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK           bool `json:"ok"`
		DeletedCount int  `json:"deleted_count"`
		KeptCount    int  `json:"kept_count"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &SummarizeResult{
		DeletedCount: resp.DeletedCount,
		KeptCount:    resp.KeptCount,
	}, nil
}


