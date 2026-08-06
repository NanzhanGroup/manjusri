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
