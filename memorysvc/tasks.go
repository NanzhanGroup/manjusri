// Package memorysvc — 任务状态机支持
//
// Task（任务）：一条用户消息（或相关消息组）触发的完整处理流程。
// 提供任务 CRUD + 步骤 checkpoint，供微信网关在消息处理过程中记录进度，
// 重启后可从中断的步骤继续，而不是从头重跑。
package memorysvc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ── 任务常量 ──

// 任务状态
const (
	TaskStatusPending         = "pending"          // 已创建，等待开始
	TaskStatusRunning         = "running"          // 处理中
	TaskStatusWaitingInput    = "waiting_input"    // 等待用户补充消息
	TaskStatusWaitingApproval = "waiting_approval" // 等待审批
	TaskStatusDone            = "done"             // 完成
	TaskStatusFailed          = "failed"           // 失败
	TaskStatusAborted         = "aborted"          // 用户取消/作废
)

// 步骤状态
const (
	StepStatusPending = "pending"
	StepStatusRunning = "running"
	StepStatusDone    = "done"
	StepStatusFailed  = "failed"
	StepStatusSkipped = "skipped"
)

// ── 数据结构 ──

// Task 任务
type Task struct {
	ID              int64  `json:"id"`
	TaskID          string `json:"task_id"`
	SessionID       string `json:"session_id"`
	UserID          string `json:"user_id"`
	Title           string `json:"title"`
	Status          string `json:"status"`
	Steps           string `json:"steps"`            // JSON 数组字符串
	CurrentStep     int    `json:"current_step"`     // 当前步骤索引（0 起）
	CurrentStepName string `json:"current_step_name"`
	Summary         string `json:"summary"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	FinishedAt      string `json:"finished_at"`
}

// TaskStep 任务步骤（Steps JSON 的单个元素）
type TaskStep struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// ParseSteps 解析 steps JSON 字符串为 []TaskStep
func ParseSteps(stepsJSON string) []TaskStep {
	if stepsJSON == "" {
		return nil
	}
	var steps []TaskStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return nil
	}
	return steps
}

// SerializeSteps 序列化 []TaskStep 为 JSON 字符串
func SerializeSteps(steps []TaskStep) string {
	if len(steps) == 0 {
		return "[]"
	}
	b, err := json.Marshal(steps)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ── CRUD ──

// createTask 创建任务，返回 task_id
func (s *Server) createTask(sessionID, userID, title string) (string, error) {
	taskID := fmt.Sprintf("T%d%03d", time.Now().UnixNano()/1e6, time.Now().UnixNano()%1000)
	_, err := s.db.Exec(`INSERT INTO tasks
		(task_id, session_id, user_id, title, status, steps, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', '[]', datetime('now','localtime'), datetime('now','localtime'))`,
		taskID, sessionID, userID, title)
	if err != nil {
		return "", fmt.Errorf("createTask: %w", err)
	}
	return taskID, nil
}

// getTask 按 task_id 获取任务
func (s *Server) getTask(taskID string) (*Task, error) {
	t := &Task{}
	err := s.db.QueryRow(`SELECT id, task_id, session_id, COALESCE(user_id,''), COALESCE(title,''),
		status, COALESCE(steps,'[]'), COALESCE(current_step,0), COALESCE(current_step_name,''),
		COALESCE(summary,''), created_at, updated_at, COALESCE(finished_at,'')
		FROM tasks WHERE task_id=?`, taskID).Scan(
		&t.ID, &t.TaskID, &t.SessionID, &t.UserID, &t.Title,
		&t.Status, &t.Steps, &t.CurrentStep, &t.CurrentStepName,
		&t.Summary, &t.CreatedAt, &t.UpdatedAt, &t.FinishedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getTask: %w", err)
	}
	return t, nil
}

// getActiveTask 获取指定 session 最近一个未结束的任务（running/waiting_*）
func (s *Server) getActiveTask(sessionID string) (*Task, error) {
	t := &Task{}
	err := s.db.QueryRow(`SELECT id, task_id, session_id, COALESCE(user_id,''), COALESCE(title,''),
		status, COALESCE(steps,'[]'), COALESCE(current_step,0), COALESCE(current_step_name,''),
		COALESCE(summary,''), created_at, updated_at, COALESCE(finished_at,'')
		FROM tasks
		WHERE session_id=? AND status IN ('pending','running','waiting_input','waiting_approval')
		ORDER BY id DESC LIMIT 1`, sessionID).Scan(
		&t.ID, &t.TaskID, &t.SessionID, &t.UserID, &t.Title,
		&t.Status, &t.Steps, &t.CurrentStep, &t.CurrentStepName,
		&t.Summary, &t.CreatedAt, &t.UpdatedAt, &t.FinishedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getActiveTask: %w", err)
	}
	return t, nil
}

// listTasks 按 session 列出任务，status 为空则全部
func (s *Server) listTasks(sessionID, status string) ([]Task, error) {
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.Query(`SELECT id, task_id, session_id, COALESCE(user_id,''), COALESCE(title,''),
			status, COALESCE(steps,'[]'), COALESCE(current_step,0), COALESCE(current_step_name,''),
			COALESCE(summary,''), created_at, updated_at, COALESCE(finished_at,'')
			FROM tasks WHERE session_id=? ORDER BY id DESC LIMIT 100`, sessionID)
	} else {
		rows, err = s.db.Query(`SELECT id, task_id, session_id, COALESCE(user_id,''), COALESCE(title,''),
			status, COALESCE(steps,'[]'), COALESCE(current_step,0), COALESCE(current_step_name,''),
			COALESCE(summary,''), created_at, updated_at, COALESCE(finished_at,'')
			FROM tasks WHERE session_id=? AND status=? ORDER BY id DESC LIMIT 100`, sessionID, status)
	}
	if err != nil {
		return nil, fmt.Errorf("listTasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(
			&t.ID, &t.TaskID, &t.SessionID, &t.UserID, &t.Title,
			&t.Status, &t.Steps, &t.CurrentStep, &t.CurrentStepName,
			&t.Summary, &t.CreatedAt, &t.UpdatedAt, &t.FinishedAt,
		); err != nil {
			return nil, fmt.Errorf("listTasks scan: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// updateTaskStatus 更新任务状态（终态时写 finished_at）
func (s *Server) updateTaskStatus(taskID, status string) error {
	var finishedSQL string
	var args []interface{}
	if status == TaskStatusDone || status == TaskStatusFailed || status == TaskStatusAborted {
		finishedSQL = `, finished_at=datetime('now','localtime')`
	} else {
		finishedSQL = `, finished_at=NULL`
	}
	args = append(args, status, taskID)
	_, err := s.db.Exec(`UPDATE tasks SET status=?, updated_at=datetime('now','localtime')`+finishedSQL+` WHERE task_id=?`, args...)
	if err != nil {
		return fmt.Errorf("updateTaskStatus: %w", err)
	}
	return nil
}

// taskCheckpoint 记录步骤进度
//   - steps：如果提供，整体替换步骤数组（JSON 字符串）
//   - step/status/stepName：单独更新当前步骤
//   - 更新 current_step / current_step_name / summary
func (s *Server) taskCheckpoint(taskID string, step int, status, stepName, note string, stepsJSON string) error {
	// 读取当前任务
	t, err := s.getTask(taskID)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("task not found: %s", taskID)
	}

	// 处理 steps
	steps := ParseSteps(t.Steps)
	if stepsJSON != "" {
		steps = ParseSteps(stepsJSON)
	}
	if step >= 0 && stepName != "" {
		// 更新指定步骤状态
		for len(steps) <= step {
			steps = append(steps, TaskStep{Name: fmt.Sprintf("步骤%d", len(steps)+1), Status: StepStatusPending})
		}
		steps[step].Name = stepName
		if status != "" {
			steps[step].Status = status
		}
		if note != "" {
			steps[step].Note = note
		}
	} else if stepName != "" {
		// 没有指定 step 索引，追加或查找同名步骤
		found := false
		for i := range steps {
			if steps[i].Name == stepName {
				if status != "" {
					steps[i].Status = status
				}
				if note != "" {
					steps[i].Note = note
				}
				found = true
				step = i
				break
			}
		}
		if !found {
			steps = append(steps, TaskStep{Name: stepName, Status: statusOr(status, StepStatusDone), Note: note})
			step = len(steps) - 1
		}
	}

	// 更新当前步骤：step >= 0 时使用；否则保持原值
	newStep := t.CurrentStep
	newStepName := t.CurrentStepName
	if step >= 0 {
		newStep = step
		newStepName = stepName
	}
	// 若步骤标记为 done，自动推进 current_step 到下一个未完成步骤
	if step >= 0 && status == StepStatusDone {
		for i := range steps {
			if steps[i].Status != StepStatusDone && steps[i].Status != StepStatusSkipped {
				newStep = i
				newStepName = steps[i].Name
				break
			}
		}
		if newStep == t.CurrentStep && len(steps) > 0 && steps[len(steps)-1].Status == StepStatusDone {
			// 全部完成
			newStep = len(steps)
			newStepName = ""
		}
	}

	_, err = s.db.Exec(`UPDATE tasks SET
		steps=?, current_step=?, current_step_name=?, updated_at=datetime('now','localtime')
		WHERE task_id=?`, SerializeSteps(steps), newStep, newStepName, taskID)
	if err != nil {
		return fmt.Errorf("taskCheckpoint: %w", err)
	}
	return nil
}

func statusOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ── HTTP Handlers ──

// handleTaskCreate POST /tasks/create
// 请求: {"session_id":"...","user_id":"...","title":"..."}
// 响应: {"ok":true,"task_id":"..."}
func (s *Server) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		UserID    string `json:"user_id"`
		Title     string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid json", 400)
		return
	}
	if req.SessionID == "" {
		writeError(w, "session_id required", 400)
		return
	}
	taskID, err := s.createTask(req.SessionID, req.UserID, req.Title)
	if err != nil {
		writeError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "task_id": taskID})
}

// handleTaskGet GET /tasks/get?task_id=xxx
func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "method not allowed", 405)
		return
	}
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeError(w, "task_id required", 400)
		return
	}
	t, err := s.getTask(taskID)
	if err != nil {
		writeError(w, err.Error(), 500)
		return
	}
	if t == nil {
		writeError(w, "task not found", 404)
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "task": t})
}

// handleTaskList GET /tasks/list?session_id=xxx&status=xxx
func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "method not allowed", 405)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	status := r.URL.Query().Get("status")
	if sessionID == "" {
		writeError(w, "session_id required", 400)
		return
	}
	tasks, err := s.listTasks(sessionID, status)
	if err != nil {
		writeError(w, err.Error(), 500)
		return
	}
	if tasks == nil {
		tasks = []Task{}
	}
	writeJSON(w, map[string]interface{}{"ok": true, "tasks": tasks, "total": len(tasks)})
}

// handleTaskUpdate POST /tasks/update
// 请求: {"task_id":"...","status":"...","title":"...","summary":"..."}
func (s *Server) handleTaskUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
		Title   string `json:"title"`
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid json", 400)
		return
	}
	if req.TaskID == "" {
		writeError(w, "task_id required", 400)
		return
	}
	if req.Status != "" {
		if err := s.updateTaskStatus(req.TaskID, req.Status); err != nil {
			writeError(w, err.Error(), 500)
			return
		}
	}
	// 更新 title / summary
	if req.Title != "" || req.Summary != "" {
		var sb strings.Builder
		var args []interface{}
		sb.WriteString("UPDATE tasks SET updated_at=datetime('now','localtime')")
		if req.Title != "" {
			sb.WriteString(", title=?")
			args = append(args, req.Title)
		}
		if req.Summary != "" {
			sb.WriteString(", summary=?")
			args = append(args, req.Summary)
		}
		sb.WriteString(" WHERE task_id=?")
		args = append(args, req.TaskID)
		if _, err := s.db.Exec(sb.String(), args...); err != nil {
			writeError(w, fmt.Sprintf("update task meta: %v", err), 500)
			return
		}
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleTaskCheckpoint POST /tasks/checkpoint
// 请求: {"task_id":"...","step":N,"status":"...","step_name":"...","note":"...","steps":"[...]"}
func (s *Server) handleTaskCheckpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		TaskID   string `json:"task_id"`
		Step     int    `json:"step"`
		Status   string `json:"status"`
		StepName string `json:"step_name"`
		Note     string `json:"note"`
		Steps    string `json:"steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid json", 400)
		return
	}
	if req.TaskID == "" {
		writeError(w, "task_id required", 400)
		return
	}
	if err := s.taskCheckpoint(req.TaskID, req.Step, req.Status, req.StepName, req.Note, req.Steps); err != nil {
		writeError(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleTaskFinish POST /tasks/finish
// 请求: {"task_id":"...","status":"done|failed|aborted","summary":"..."}
func (s *Server) handleTaskFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "method not allowed", 405)
		return
	}
	var req struct {
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid json", 400)
		return
	}
	if req.TaskID == "" {
		writeError(w, "task_id required", 400)
		return
	}
	status := req.Status
	if status == "" {
		status = TaskStatusDone
	}
	if err := s.updateTaskStatus(req.TaskID, status); err != nil {
		writeError(w, err.Error(), 500)
		return
	}
	if req.Summary != "" {
		if _, err := s.db.Exec(`UPDATE tasks SET summary=?, updated_at=datetime('now','localtime') WHERE task_id=?`, req.Summary, req.TaskID); err != nil {
			writeError(w, fmt.Sprintf("update summary: %v", err), 500)
			return
		}
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleTaskActive GET /tasks/active?session_id=xxx
// 获取指定 session 最近一个未结束的任务
func (s *Server) handleTaskActive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "method not allowed", 405)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, "session_id required", 400)
		return
	}
	t, err := s.getActiveTask(sessionID)
	if err != nil {
		writeError(w, err.Error(), 500)
		return
	}
	if t == nil {
		writeJSON(w, map[string]interface{}{"ok": true, "found": false})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "found": true, "task": t})
}
