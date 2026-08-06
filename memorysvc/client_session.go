// 会话管理客户端 API
package memorysvc

import (
	"encoding/json"
	"fmt"
)

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
	Processed   *bool  `json:"processed,omitempty"` // nil=不更新
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
