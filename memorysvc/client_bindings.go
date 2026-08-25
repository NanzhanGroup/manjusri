// 跨平台用户绑定客户端 API
package memorysvc

import (
	"encoding/json"
	"fmt"
)

// BindUser 绑定别名 session → 主 session（幂等）
// aliasSession: 发起绑定一方的 session（如 wecom_xxx）
// primarySession: 记忆归属方 session（如 weixin_xxx）
func (c *Client) BindUser(aliasSession, primarySession, note string) error {
	_, err := c.request("POST", "/bind", map[string]string{
		"alias_session":   aliasSession,
		"primary_session": primarySession,
		"note":            note,
	})
	return err
}

// UnbindUser 解除别名 session 的绑定
func (c *Client) UnbindUser(aliasSession string) error {
	_, err := c.request("POST", "/unbind", map[string]string{
		"alias_session": aliasSession,
	})
	return err
}

// ResolveSession 解析 session_id：命中绑定返回主 session，否则原样返回。
// 网关在读写记忆前调用，实现跨平台记忆互通。RPC 失败时返回原 session_id（降级，不影响主流程）。
func (c *Client) ResolveSession(sessionID string) string {
	data, err := c.request("POST", "/resolve-session", map[string]string{
		"session_id": sessionID,
	})
	if err != nil {
		// 降级：memory-service 不可用时原样返回，保证消息处理不中断
		return sessionID
	}
	var resp ResolveResult
	if err := json.Unmarshal(data, &resp); err != nil {
		return sessionID
	}
	if resp.Resolved == "" {
		return sessionID
	}
	return resp.Resolved
}

// ListBindings 列出全部绑定关系
func (c *Client) ListBindings() ([]UserBinding, error) {
	data, err := c.request("GET", "/list-bindings", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK       bool          `json:"ok"`
		Bindings []UserBinding `json:"bindings"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("list bindings failed")
	}
	if resp.Bindings == nil {
		resp.Bindings = []UserBinding{}
	}
	return resp.Bindings, nil
}
