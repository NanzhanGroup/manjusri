// 任务状态机客户端 API
package memorysvc

import (
	"encoding/json"
	"fmt"
)

// CreateTask 创建任务，返回 task_id
func (c *Client) CreateTask(sessionID, userID, title string) (string, error) {
	data, err := c.request("POST", "/tasks/create", map[string]interface{}{
		"session_id": sessionID,
		"user_id":    userID,
		"title":      title,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		OK     bool   `json:"ok"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse CreateTask response: %w", err)
	}
	if !resp.OK {
		return "", fmt.Errorf("create task failed")
	}
	return resp.TaskID, nil
}

// GetTask 获取任务详情
func (c *Client) GetTask(taskID string) (*Task, error) {
	data, err := c.request("GET", "/tasks/get?task_id="+taskID, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK   bool  `json:"ok"`
		Task *Task `json:"task"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse GetTask response: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("task not found")
	}
	return resp.Task, nil
}

// ListTasks 列出会话的任务，status 为空则列出全部
func (c *Client) ListTasks(sessionID, status string) ([]Task, error) {
	path := fmt.Sprintf("/tasks/list?session_id=%s&status=%s", sessionID, status)
	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Tasks []Task `json:"tasks"`
		Total int    `json:"total"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse ListTasks response: %w", err)
	}
	return resp.Tasks, nil
}

// GetActiveTask 获取会话最近一个未结束的任务
// 返回 (found, task, error)
func (c *Client) GetActiveTask(sessionID string) (bool, *Task, error) {
	path := fmt.Sprintf("/tasks/active?session_id=%s", sessionID)
	data, err := c.request("GET", path, nil)
	if err != nil {
		return false, nil, err
	}
	var resp struct {
		OK    bool  `json:"ok"`
		Found bool  `json:"found"`
		Task  *Task `json:"task"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, nil, fmt.Errorf("parse GetActiveTask response: %w", err)
	}
	return resp.Found, resp.Task, nil
}

// UpdateTaskStatus 更新任务状态（done/failed/aborted 时自动写 finished_at）
func (c *Client) UpdateTaskStatus(taskID, status string) error {
	_, err := c.request("POST", "/tasks/update", map[string]interface{}{
		"task_id": taskID,
		"status":  status,
	})
	return err
}

// UpdateTaskMeta 更新任务元数据（title/summary）
func (c *Client) UpdateTaskMeta(taskID, title, summary string) error {
	body := map[string]interface{}{"task_id": taskID}
	if title != "" {
		body["title"] = title
	}
	if summary != "" {
		body["summary"] = summary
	}
	_, err := c.request("POST", "/tasks/update", body)
	return err
}

// TaskCheckpoint 记录任务步骤进度
//   - step: 步骤索引（0 起）；-1 表示仅更新 steps 整体
//   - status: step 状态（done/running/failed/skipped）
//   - stepName: 步骤名
//   - note: 备注
//   - stepsJSON: 可选，整体替换 steps（JSON 数组字符串），传 "" 表示不替换
func (c *Client) TaskCheckpoint(taskID string, step int, status, stepName, note, stepsJSON string) error {
	_, err := c.request("POST", "/tasks/checkpoint", map[string]interface{}{
		"task_id":   taskID,
		"step":      step,
		"status":    status,
		"step_name": stepName,
		"note":      note,
		"steps":     stepsJSON,
	})
	return err
}

// FinishTask 完成任务
func (c *Client) FinishTask(taskID, status, summary string) error {
	_, err := c.request("POST", "/tasks/finish", map[string]interface{}{
		"task_id": taskID,
		"status":  status,
		"summary": summary,
	})
	return err
}
