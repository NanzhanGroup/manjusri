// 上下文拼装 / 评分 / 合并客户端 API
package memorysvc

import (
	"encoding/json"
	"fmt"
)

func (c *Client) BuildContext(sessionID, query string, maxMessages, maxMemories int, includeObsolete bool) (*ContextResult, error) {
	data, err := c.request("POST", "/context/build", map[string]interface{}{
		"session_id":       sessionID,
		"query":            query,
		"max_messages":     maxMessages,
		"max_memories":     maxMemories,
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
