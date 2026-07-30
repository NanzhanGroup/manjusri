package compile

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/banshanhanfu/ws-lang/parser"
)

func parseWS(t *testing.T, content string) *parser.ParseResult {
	t.Helper()
	result, err := parser.Parse(content)
	if err != nil {
		t.Fatalf("parser.Parse failed: %v", err)
	}
	return result
}

func TestCompile_EmptyContent(t *testing.T) {
	result, err := parser.Parse("")
	if err == nil && len(result.Nodes) == 0 {
		// 空内容返回空结果，不报错，这是预期的
		is := Compile(result, "empty")
		if len(is.Steps) != 0 {
			t.Errorf("期望 0 个 step, 得到 %d", len(is.Steps))
		}
		return
	}
	// 如果报错也是合理的
}

func TestCompile_SingleStep(t *testing.T) {
	content := `step: 编译
  cap: ws-shell
  input: go build ./...
  -> output: build_ok`

	result := parseWS(t, content)
	is := Compile(result, "test")

	if len(is.Steps) != 1 {
		t.Fatalf("期望1个step, 得到 %d", len(is.Steps))
	}
	if is.Steps[0].Name != "编译" {
		t.Errorf("期望 Name=编译, 得到 %s", is.Steps[0].Name)
	}
	if is.Steps[0].Cap != "ws-shell" {
		t.Errorf("期望 Cap=ws-shell, 得到 %s", is.Steps[0].Cap)
	}
	if is.Steps[0].Input.Type != "literal" || is.Steps[0].Input.Value != "go build ./..." {
		t.Errorf("Input 不匹配: %+v", is.Steps[0].Input)
	}
	if len(is.Steps[0].Outputs) != 1 || is.Steps[0].Outputs[0].Name != "build_ok" {
		t.Errorf("Output 不匹配: %+v", is.Steps[0].Outputs)
	}
}

func TestCompile_MultipleStepsWithRef(t *testing.T) {
	content := `step: 编译
  cap: ws-shell
  input: go build
  -> output: build_ok

step: 测试
  cap: ws-shell
  input: $build_ok
  -> output: test_ok`

	result := parseWS(t, content)
	is := Compile(result, "test")

	if len(is.Steps) != 2 {
		t.Fatalf("期望2个step, 得到 %d", len(is.Steps))
	}

	// 第二个 step 的 input 应该是 ref 类型
	if is.Steps[1].Input.Type != "ref" {
		t.Errorf("期望 ref 类型, 得到 %s", is.Steps[1].Input.Type)
	}
}

func TestCompile_ArrayInput(t *testing.T) {
	content := `step: 上传
  cap: ws-storage
  input: [$img1, $img2]`

	result := parseWS(t, content)
	is := Compile(result, "test")

	if len(is.Steps) != 1 {
		t.Fatalf("期望1个step, 得到 %d", len(is.Steps))
	}

	input := is.Steps[0].Input
	if input.Type != "merge" {
		t.Errorf("期望 merge 类型, 得到 %s", input.Type)
	}
	if len(input.Sources) != 2 {
		t.Errorf("期望2个source, 得到 %d", len(input.Sources))
	}
}

func TestCompile_OnErrorRetry(t *testing.T) {
	content := `step: 部署
  cap: ws-shell
  input: deploy.sh
  on_error: retry(3, 10s)`

	result := parseWS(t, content)
	is := Compile(result, "test")

	oe := is.Steps[0].OnError
	if oe == nil {
		t.Fatal("期望 OnError 不为 nil")
	}
	if oe.Action != "retry" {
		t.Errorf("期望 retry, 得到 %s", oe.Action)
	}
	if oe.MaxRetries != 3 {
		t.Errorf("期望 MaxRetries=3, 得到 %d", oe.MaxRetries)
	}
	if oe.Interval != 10 {
		t.Errorf("期望 Interval=10, 得到 %d", oe.Interval)
	}
}

func TestCompile_Args(t *testing.T) {
	content := `step: 压缩
  cap: ws-image
  input: $images
  quality: 80
  format: webp`

	result := parseWS(t, content)
	is := Compile(result, "test")

	if len(is.Steps[0].Args) != 2 {
		t.Errorf("期望2个args, 得到 %d", len(is.Steps[0].Args))
	}
	if is.Steps[0].Args["quality"] != "80" {
		t.Errorf("期望 quality=80, 得到 %s", is.Steps[0].Args["quality"])
	}
}

func TestToYAML_ProducesValidOutput(t *testing.T) {
	content := `step: 编译
  cap: ws-shell
  input: go build
  -> output: build_ok`

	result := parseWS(t, content)
	is := Compile(result, "test")
	yaml := ToYAML(is)

	if !strings.Contains(yaml, "name: test") {
		t.Errorf("YAML 应包含 name: test")
	}
	if !strings.Contains(yaml, "s-1") {
		t.Errorf("YAML 应包含 step id s-1")
	}
	if !strings.Contains(yaml, "cap: ws-shell") {
		t.Errorf("YAML 应包含 cap: ws-shell")
	}
}

func TestToJSON_Marshal(t *testing.T) {
	content := `step: 编译
  cap: ws-shell
  input: go build`

	result := parseWS(t, content)
	is := Compile(result, "test")

	data, err := json.MarshalIndent(is, "", "  ")
	if err != nil {
		t.Fatalf("JSON marshal failed: %v", err)
	}

	var decoded InstructionSet
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("JSON unmarshal failed: %v", err)
	}

	if len(decoded.Steps) != 1 {
		t.Errorf("期望1个step, 得到 %d", len(decoded.Steps))
	}
}
