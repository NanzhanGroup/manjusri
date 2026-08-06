// 消息与线程客户端 API
package memorysvc

import (
	"encoding/json"
	"fmt"
)

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
		OK       bool            `json:"ok"`
		Thread   Thread          `json:"thread"`
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
		OK     bool            `json:"ok"`
		MsgIDs map[string]bool `json:"msg_ids"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return resp.MsgIDs, nil
}

// ── 上下文拼装 ──

// BuildContext 拼装上下文
