package parser

import (
	"fmt"
	"strings"

	"github.com/NanzhanGroup/manjusri/ws-lang/translations"
)

// Node represents a parsed KV line node
type Node struct {
	Key      string
	Value    string
	Children []*Node
	IsRef    bool   // $xxx reference
	IsOutput bool   // -> output: name
	IsArray  bool   // [...] value
	LineNum  int
}

// ParseResult holds the full parse output
type ParseResult struct {
	Nodes     []*Node
	TaskName  string
}

// Parse parses a ws-lang KV line format string into a tree of nodes
func Parse(input string) (*ParseResult, error) {
	lines := strings.Split(input, "\n")
	result := &ParseResult{}
	var stack []*Node
	var currentParent *Node

	tm := translations.GetManager()

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Skip empty lines and comments
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Calculate indentation level (2 spaces per level)
		indent := countIndent(line)

		// Remove inline comments (v2 feature)
		if idx := strings.Index(trimmed, " #"); idx > 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}

		// Detect special patterns
		node, err := parseLine(trimmed, lineNum)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum, err)
		}

		// Adjust stack based on indentation
		for len(stack) > indent {
			stack = stack[:len(stack)-1]
		}
		if indent > 0 && len(stack) > 0 {
			currentParent = stack[len(stack)-1]
			currentParent.Children = append(currentParent.Children, node)
		} else {
			result.Nodes = append(result.Nodes, node)
			// First top-level key is treated as task name key
			if len(result.Nodes) == 1 && tm.IsCanonical(node.Key, "task", "name") {
				result.TaskName = node.Value
			}
		}

		// Push to stack if it might have children (next line with more indent)
		stack = append(stack, node)
	}

	return result, nil
}

func countIndent(line string) int {
	count := 0
	for _, ch := range line {
		if ch == ' ' {
			count++
		} else if ch == '\t' {
			count += 2
		} else {
			break
		}
	}
	return count / 2
}

func parseLine(line string, lineNum int) (*Node, error) {
	node := &Node{LineNum: lineNum}
	tm := translations.GetManager()

	// -> output: xxx  (output port definition)
	if strings.HasPrefix(line, "->") {
		rest := strings.TrimPrefix(line, "->")
		rest = strings.TrimSpace(rest)

		// Check if first part is an output keyword
		parts := splitKV(rest)
		if len(parts) >= 1 && tm.IsCanonical(parts[0], "output") {
			if len(parts) == 2 {
				node.Key = "output"
				node.Value = parts[1]
				node.IsOutput = true
				return node, nil
			}
		}
		// -> key: value (inline output)
		if len(parts) == 2 {
			node.Key = parts[0]
			node.Value = parts[1]
			return node, nil
		}
		return nil, fmt.Errorf("invalid output syntax: %s", line)
	}

	// Normal key: value parsing
	parts := splitKV(line)
	if len(parts) < 2 {
		// Single word on its own line could be a block start
		if len(parts) == 1 && tm.IsCanonical(parts[0], "task", "step") {
			node.Key = parts[0]
			return node, nil
		}
		return nil, fmt.Errorf("invalid format, expected key: value")
	}

	node.Key = parts[0]
	value := parts[1]

	// Handle { ... } inline block (value before { is preserved as node value)
	if braceStart := strings.Index(value, "{"); braceStart >= 0 {
		if strings.HasSuffix(value, "}") {
			// Extract text before the brace as node value
			beforeBrace := strings.TrimSpace(value[:braceStart])
			if beforeBrace != "" {
				node.Value = beforeBrace
			}

			inner := strings.TrimSpace(value[braceStart+1 : len(value)-1])
			// Parse inline key: value pairs
			pairs := splitInlinePairs(inner)
			for _, pair := range pairs {
				p := splitKV(pair)
				if len(p) == 2 {
					child := &Node{
						Key:     p[0],
						Value:   p[1],
						LineNum: lineNum,
					}
					node.Children = append(node.Children, child)
				}
			}
			return node, nil
		}
	}

	// Handle [$ref, $ref] array
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		node.IsArray = true
		node.Value = value
		return node, nil
	}

	// Handle $ref references
	if strings.HasPrefix(value, "$") {
		node.IsRef = true
	}

	node.Value = value
	return node, nil
}

func splitKV(line string) []string {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return []string{strings.TrimSpace(line)}
	}
	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])
	return []string{key, value}
}

func splitInlinePairs(inner string) []string {
	var pairs []string
	depth := 0
	start := 0
	for i, ch := range inner {
		switch ch {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				pairs = append(pairs, strings.TrimSpace(inner[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(inner) {
		pairs = append(pairs, strings.TrimSpace(inner[start:]))
	}
	return pairs
}
