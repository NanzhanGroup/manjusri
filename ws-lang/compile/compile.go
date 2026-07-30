// Package compile provides the InstructionSet types and compilation logic
// for converting ws-lang KV format into canonical YAML/JSON instruction sets.
package compile

import (
	"fmt"
	"strings"

	"github.com/NanzhanGroup/manjusri/ws-lang/parser"
	"github.com/NanzhanGroup/manjusri/ws-lang/translations"
)

// InstructionSet is the canonical instruction set format
type InstructionSet struct {
	TaskID string  `yaml:"task_id,omitempty" json:"task_id,omitempty"`
	Name   string  `yaml:"name" json:"name"`
	Status string  `yaml:"status,omitempty" json:"status,omitempty"`
	Steps  []*Step `yaml:"steps" json:"steps"`
}

// Step represents a single execution step
type Step struct {
	ID        string            `yaml:"id" json:"id"`
	Name      string            `yaml:"name" json:"name"`
	Cap       string            `yaml:"cap" json:"cap"`
	Input     *Input            `yaml:"input,omitempty" json:"input,omitempty"`
	Args      map[string]string `yaml:"args,omitempty" json:"args,omitempty"`
	Outputs   []*Output         `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	DependsOn []string          `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	OnError   *ErrorHandling    `yaml:"on_error,omitempty" json:"on_error,omitempty"`
}

// Input describes step input source
type Input struct {
	Type    string   `yaml:"type" json:"type"`
	Value   string   `yaml:"value,omitempty" json:"value,omitempty"`
	Path    string   `yaml:"path,omitempty" json:"path,omitempty"`
	From    string   `yaml:"from,omitempty" json:"from,omitempty"`
	Sources []*Input `yaml:"sources,omitempty" json:"sources,omitempty"`
}

// Output describes a named output port
type Output struct {
	Name string `yaml:"name" json:"name"`
	From string `yaml:"from" json:"from"`
}

// ErrorHandling describes error handling policy
type ErrorHandling struct {
	Action     string `yaml:"action" json:"action"`
	MaxRetries int    `yaml:"max_retries,omitempty" json:"max_retries,omitempty"`
	Interval   int    `yaml:"interval,omitempty" json:"interval,omitempty"`
}

// Compile converts parsed ws-lang AST into an InstructionSet.
func Compile(result *parser.ParseResult, name string) *InstructionSet {
	if name == "" {
		name = result.TaskName
	}

	is := &InstructionSet{
		Name:   name,
		Status: "pending",
	}

	stepIndex := 0
	stepNameToID := make(map[string]string)
	tm := translations.GetManager()

	// First pass: register all step names
	for _, node := range result.Nodes {
		stepIndex = registerStepNames(node, &stepIndex, stepNameToID, tm)
	}

	// Second pass: compile steps with dependency resolution
	stepIndex = 0
	for _, node := range result.Nodes {
		stepIndex = compileNode(node, is, &stepIndex, stepNameToID, tm)
	}

	if is.Name == "" {
		is.Name = name
	}

	return is
}

func registerStepNames(node *parser.Node, stepIndex *int, nameToID map[string]string, tm *translations.Manager) int {
	if tm.IsCanonical(node.Key, "step") {
		*stepIndex++
		id := fmt.Sprintf("s-%d", *stepIndex)
		if node.Value != "" {
			nameToID[node.Value] = id
		}
		return *stepIndex
	}

	for _, child := range node.Children {
		if tm.IsCanonical(child.Key, "step") {
			*stepIndex++
			id := fmt.Sprintf("s-%d", *stepIndex)
			if child.Value != "" {
				nameToID[child.Value] = id
			}
		}
	}
	return *stepIndex
}

func compileNode(node *parser.Node, is *InstructionSet, stepIndex *int, nameToID map[string]string, tm *translations.Manager) int {
	if tm.IsCanonical(node.Key, "step") {
		*stepIndex++
		step := buildStep(node, *stepIndex, nameToID, tm)
		is.Steps = append(is.Steps, step)
		return *stepIndex
	}

	for _, child := range node.Children {
		if tm.IsCanonical(child.Key, "step") {
			*stepIndex++
			step := buildStep(child, *stepIndex, nameToID, tm)
			is.Steps = append(is.Steps, step)
		} else {
			*stepIndex = compileNode(child, is, stepIndex, nameToID, tm)
		}
	}
	return *stepIndex
}

func buildStep(node *parser.Node, index int, nameToID map[string]string, tm *translations.Manager) *Step {
	step := &Step{
		ID:   fmt.Sprintf("s-%d", index),
		Name: node.Value,
	}

	for _, child := range node.Children {
		if tm.IsCanonical(child.Key, "cap", "capability") {
			step.Cap = child.Value
		} else if tm.IsCanonical(child.Key, "input") {
			step.Input = compileInput(child, nameToID)
		} else if tm.IsCanonical(child.Key, "on_error") {
			step.OnError = compileErrorHandling(child.Value, tm)
		} else if tm.IsCanonical(child.Key, "output") {
			// handled by nodeOutputs
		} else {
			if child.IsOutput {
				continue
			}
			if len(child.Children) > 0 && tm.IsCanonical(child.Key, "args") {
				if step.Args == nil {
					step.Args = make(map[string]string)
				}
				for _, argChild := range child.Children {
					step.Args[argChild.Key] = argChild.Value
				}
			} else if step.Args == nil {
				step.Args = make(map[string]string)
				step.Args[child.Key] = child.Value
			} else {
				step.Args[child.Key] = child.Value
			}
		}
	}

	if step.Cap != "" && len(nodeOutputs(node, tm)) == 0 {
		step.Outputs = []*Output{
			{Name: "default", From: "stdout.text"},
		}
	} else {
		step.Outputs = nodeOutputs(node, tm)
	}

	return step
}

func nodeOutputs(node *parser.Node, tm *translations.Manager) []*Output {
	var outputs []*Output
	for _, child := range node.Children {
		if child.IsOutput || tm.IsCanonical(child.Key, "output") {
			outputs = append(outputs, &Output{
				Name: child.Value,
				From: "stdout.text",
			})
		}
	}
	return outputs
}

func compileInput(node *parser.Node, nameToID map[string]string) *Input {
	if node.IsArray {
		input := &Input{Type: "merge"}
		inner := strings.TrimSpace(node.Value[1 : len(node.Value)-1])
		refs := strings.Split(inner, ",")
		for _, ref := range refs {
			ref = strings.TrimSpace(ref)
			refName := strings.TrimPrefix(ref, "$")
			fromID := resolveStepRef(refName, nameToID)
			input.Sources = append(input.Sources, &Input{
				Type: "ref",
				From: fromID,
			})
		}
		return input
	}

	if node.IsRef {
		refName := strings.TrimPrefix(node.Value, "$")
		fromID := resolveStepRef(refName, nameToID)
		return &Input{Type: "ref", From: fromID}
	}

	val := node.Value
	if strings.HasPrefix(val, "/") || strings.HasPrefix(val, "./") || strings.HasPrefix(val, "../") ||
		strings.HasPrefix(val, "s3://") || strings.HasPrefix(val, "http://") || strings.HasPrefix(val, "https://") {
		return &Input{Type: "file", Path: val}
	}

	return &Input{Type: "literal", Value: val}
}

func resolveStepRef(refName string, nameToID map[string]string) string {
	if id, ok := nameToID[refName]; ok {
		return id
	}
	for name, id := range nameToID {
		if strings.ReplaceAll(name, " ", "") == strings.ReplaceAll(refName, " ", "") {
			return id
		}
	}
	return refName
}

func compileErrorHandling(value string, tm *translations.Manager) *ErrorHandling {
	eh := &ErrorHandling{Action: "stop"}

	isRetry := false
	if value == "retry" || strings.HasPrefix(value, "retry(") {
		isRetry = true
	}
	for _, native := range tm.NativeKeywords("retry") {
		if value == native || strings.HasPrefix(value, native+"(") {
			isRetry = true
			break
		}
	}

	if isRetry {
		eh.Action = "retry"
		eh.MaxRetries = 3
		eh.Interval = 5

		if idx := strings.Index(value, "("); idx > 0 {
			end := strings.Index(value, ")")
			if end < 0 {
				end = len(value)
			}
			params := value[idx+1 : end]
			parts := strings.Split(params, ",")
			if len(parts) > 0 {
				fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &eh.MaxRetries)
			}
			if len(parts) > 1 {
				var interval int
				intervalStr := strings.TrimSpace(parts[1])
				intervalStr = strings.TrimSuffix(intervalStr, "s")
				fmt.Sscanf(intervalStr, "%d", &interval)
				if interval > 0 {
					eh.Interval = interval
				}
			}
		}
	} else if value == "ignore" {
		eh.Action = "ignore"
	} else if value == "stop" {
		eh.Action = "stop"
	}

	return eh
}

// ToYAML serializes the InstructionSet to canonical YAML format.
func ToYAML(is *InstructionSet) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("# ws-lang instruction set\n"))
	b.WriteString(fmt.Sprintf("---\n"))
	b.WriteString(fmt.Sprintf("name: %s\n", is.Name))
	b.WriteString(fmt.Sprintf("status: %s\n", is.Status))
	if is.TaskID != "" {
		b.WriteString(fmt.Sprintf("task_id: %s\n", is.TaskID))
	}
	b.WriteString("steps:\n")

	for _, step := range is.Steps {
		b.WriteString(fmt.Sprintf("  - id: %s\n", step.ID))
		b.WriteString(fmt.Sprintf("    name: %s\n", step.Name))
		if step.Cap != "" {
			b.WriteString(fmt.Sprintf("    cap: %s\n", step.Cap))
		}
		if step.Input != nil {
			b.WriteString("    input:\n")
			writeInput(&b, step.Input, "      ")
		}
		if len(step.Args) > 0 {
			b.WriteString("    args:\n")
			for k, v := range step.Args {
				b.WriteString(fmt.Sprintf("      %s: %s\n", k, v))
			}
		}
		if len(step.Outputs) > 0 {
			b.WriteString("    outputs:\n")
			for _, o := range step.Outputs {
				b.WriteString(fmt.Sprintf("      - name: %s\n", o.Name))
				b.WriteString(fmt.Sprintf("        from: %s\n", o.From))
			}
		}
		if len(step.DependsOn) > 0 {
			b.WriteString("    depends_on:\n")
			for _, d := range step.DependsOn {
				b.WriteString(fmt.Sprintf("      - %s\n", d))
			}
		}
		if step.OnError != nil {
			b.WriteString("    on_error:\n")
			b.WriteString(fmt.Sprintf("      action: %s\n", step.OnError.Action))
			if step.OnError.MaxRetries > 0 {
				b.WriteString(fmt.Sprintf("      max_retries: %d\n", step.OnError.MaxRetries))
			}
			if step.OnError.Interval > 0 {
				b.WriteString(fmt.Sprintf("      interval: %d\n", step.OnError.Interval))
			}
		}
	}

	return b.String()
}

func writeInput(b *strings.Builder, input *Input, indent string) {
	b.WriteString(fmt.Sprintf("%stype: %s\n", indent, input.Type))
	switch input.Type {
	case "file":
		b.WriteString(fmt.Sprintf("%spath: %s\n", indent, input.Path))
	case "ref":
		b.WriteString(fmt.Sprintf("%sfrom: %s\n", indent, input.From))
	case "literal":
		b.WriteString(fmt.Sprintf("%svalue: %s\n", indent, input.Value))
	case "merge":
		b.WriteString(fmt.Sprintf("%ssources:\n", indent))
		for _, s := range input.Sources {
			b.WriteString(fmt.Sprintf("%s  - type: %s\n", indent, s.Type))
			if s.From != "" {
				b.WriteString(fmt.Sprintf("%s    from: %s\n", indent, s.From))
			}
		}
	}
}
