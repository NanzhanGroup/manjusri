package parser

import (
	"testing"
)

func TestParseSimpleTask(t *testing.T) {
	input := `task: 会议处理
  step: 转录
    cap: ws-voice
    input: /recordings/file.mp3
  step: 总结
    cap: ws-summarize
    input: $转录`

	result, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if result.TaskName != "会议处理" {
		t.Errorf("expected task name '会议处理', got '%s'", result.TaskName)
	}

	if len(result.Nodes) != 1 {
		t.Fatalf("expected 1 top-level node, got %d", len(result.Nodes))
	}

	task := result.Nodes[0]
	if len(task.Children) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(task.Children))
	}

	// First step
	s1 := task.Children[0]
	if s1.Key != "step" {
		t.Errorf("expected step key, got %s", s1.Key)
	}
	if s1.Value != "转录" {
		t.Errorf("expected step name '转录', got '%s'", s1.Value)
	}
}

func TestParseWithInlineBlock(t *testing.T) {
	input := `task: 测试
  step: 处理 { cap: ws-test, input: test.txt }`

	result, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	step := result.Nodes[0].Children[0]
	if len(step.Children) != 2 {
		t.Fatalf("expected 2 inline children, got %d", len(step.Children))
	}
	if step.Children[0].Value != "ws-test" {
		t.Errorf("expected cap 'ws-test', got '%s'", step.Children[0].Value)
	}
}

func TestParseOutputPort(t *testing.T) {
	input := `task: test
  step: transcribe
    cap: ws-voice
    -> output: transcript`

	result, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	step := result.Nodes[0].Children[0]
	if len(step.Children) < 2 {
		t.Fatalf("expected at least 2 children, got %d", len(step.Children))
	}

	lastChild := step.Children[len(step.Children)-1]
	if !lastChild.IsOutput {
		t.Error("expected IsOutput to be true")
	}
	if lastChild.Value != "transcript" {
		t.Errorf("expected output name 'transcript', got '%s'", lastChild.Value)
	}
}

func TestParseWithRef(t *testing.T) {
	input := `task: test
  step: fetch
    cap: ws-db
  step: process
    cap: ws-calc
    input: $fetch`

	result, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	process := result.Nodes[0].Children[1]
	// find input child
	for _, child := range process.Children {
		if child.Key == "input" {
			if !child.IsRef {
				t.Error("expected IsRef to be true for $fetch")
			}
			if child.Value != "$fetch" {
				t.Errorf("expected '$fetch', got '%s'", child.Value)
			}
			break
		}
	}
}

func TestParseWithComments(t *testing.T) {
	input := `# 这是任务注释
task: 测试
  step: 处理  # 行尾注释
    cap: ws-test`

	result, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if result.TaskName != "测试" {
		t.Errorf("expected '测试', got '%s'", result.TaskName)
	}
}

func TestParseEmptyInput(t *testing.T) {
	result, err := Parse("")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(result.Nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(result.Nodes))
	}
}

func TestParseChineseKeywords(t *testing.T) {
	input := `任务: 日报
  步骤: 查询
    能力: ws-db-query
    sql: SELECT * FROM metrics`

	result, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	task := result.Nodes[0]
	if task.Value != `"日报"` && task.Value != "日报" {
		t.Logf("task value: '%s'", task.Value)
	}

	step := task.Children[0]
	if step.Value != "查询" {
		t.Errorf("expected step '查询', got '%s'", step.Value)
	}
}

func TestYAMLOutput(t *testing.T) {
	input := `task: test
  step: transcribe
    cap: ws-voice
    -> output: transcript
  step: summarize
    cap: ws-summarize
    input: $transcript`

	result, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	yaml := result.ToYAML()
	if len(yaml) == 0 {
		t.Error("expected non-empty YAML output")
	}
	t.Logf("YAML output:\n%s", yaml)
}
