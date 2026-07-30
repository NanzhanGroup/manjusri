// Package translations provides keyword translation for ws-lang.
// It loads translation JSON files and builds forward and reverse maps.
package translations

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Forward maps native keyword → canonical keyword (e.g. "任务" → "task")
type Forward map[string]string

// Reverse maps canonical keyword → all native variants (e.g. "task" → ["任务", "タスク", "tarea", ...])
type Reverse map[string][]string

// regexpKey uniquely identifies a compiled regexp for a keyword in a language
type regexpKey struct {
	langCode string
	keyword  string // the native keyword (original, before escaping)
}

// Manager holds all loaded translations and provides keyword checking
type Manager struct {
	mu       sync.RWMutex
	forwards map[string]Forward   // lang code → forward map
	reverses map[string]Reverse   // lang code → reverse map
	allKeys  map[string]bool      // set of all native keywords across all languages
	canonSet map[string]bool      // canonical keyword → isStep, isTask, etc.
	regexpMu sync.RWMutex
	regexps  map[regexpKey]*regexp.Regexp // cached compiled regexps
}

var defaultManager *Manager
var once sync.Once

// GetManager returns the singleton translation manager
func GetManager() *Manager {
	once.Do(func() {
		mgr := NewManager()
		// Search multiple paths for translations
		exe, _ := os.Executable()
		cwd, _ := os.Getwd()
		dirs := []string{
			filepath.Dir(exe) + "/translations",
			"translations",
			"/data/ws-lang/translations",
			filepath.Join(cwd, "translations"),
			filepath.Join(cwd, "..", "translations"),
			filepath.Join(cwd, "..", "..", "translations"),
			filepath.Join(cwd, "..", "..", "..", "translations"),
			filepath.Join(os.Getenv("HOME"), "ws-lang", "translations"),
			filepath.Join(os.Getenv("GOPATH"), "src", "github.com", "banshanhanfu", "ws-lang", "translations"),
		}
		for _, d := range dirs {
			if info, err := os.Stat(d); err == nil && info.IsDir() {
				mgr.LoadDir(d)
				defaultManager = mgr
				return
			}
		}
		// Fallback: try to find translations directory relative to module root
		if modRoot := findModuleRoot(); modRoot != "" {
			d := filepath.Join(modRoot, "translations")
			if info, err := os.Stat(d); err == nil && info.IsDir() {
				mgr.LoadDir(d)
			}
		}
		defaultManager = mgr
	})
	return defaultManager
}

// findModuleRoot looks for go.mod to determine module root
func findModuleRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ResetForTesting resets the singleton for tests (test use only)
func ResetForTesting() {
	once = sync.Once{}
	once.Do(func() {}) // prevent re-init
	defaultManager = NewManager()
	once = sync.Once{} // reset for real init
}

func NewManager() *Manager {
	return &Manager{
		forwards: make(map[string]Forward),
		reverses: make(map[string]Reverse),
		allKeys:  make(map[string]bool),
		canonSet: make(map[string]bool),
		regexps:  make(map[regexpKey]*regexp.Regexp),
	}
}

// LoadDir loads all *.json translation files from a directory
func (m *Manager) LoadDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		langCode := strings.TrimSuffix(e.Name(), ".json")
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("read %s: %w", e.Name(), err)
		}
		if err := m.LoadJSON(langCode, data); err != nil {
			return fmt.Errorf("load %s: %w", e.Name(), err)
		}
	}
	return nil
}

// LoadJSON loads translations from JSON bytes for a given language code
func (m *Manager) LoadJSON(langCode string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var fwd Forward
	if err := json.Unmarshal(data, &fwd); err != nil {
		return err
	}

	m.forwards[langCode] = fwd

	// Build reverse map
	rev := make(Reverse)
	for native, canonical := range fwd {
		rev[canonical] = append(rev[canonical], native)
		m.allKeys[native] = true
		m.canonSet[canonical] = true
	}
	m.reverses[langCode] = rev

	return nil
}

// GetForward returns the forward map for a language code
func (m *Manager) GetForward(langCode string) Forward {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.forwards[langCode]
}

// getRegexp returns a cached compiled regexp for the given language and keyword.
// Thread-safe; compiles once and reuses forever.
func (m *Manager) getRegexp(langCode, nativeKeyword string) *regexp.Regexp {
	key := regexpKey{langCode: langCode, keyword: nativeKeyword}

	m.regexpMu.RLock()
	re, ok := m.regexps[key]
	m.regexpMu.RUnlock()
	if ok {
		return re
	}

	escaped := regexp.QuoteMeta(nativeKeyword)
	// Match keyword preceded by line-start or whitespace,
	// and followed by ':', '(', or end-of-line (with optional whitespace)
	compiled := regexp.MustCompile(`(^|\s)` + escaped + `(\s*[:\(]|\s*$)`)

	m.regexpMu.Lock()
	// Double-check after acquiring write lock
	if existing, ok := m.regexps[key]; ok {
		m.regexpMu.Unlock()
		return existing
	}
	m.regexps[key] = compiled
	m.regexpMu.Unlock()
	return compiled
}

// Translate translates a string using the given language's forward map.
// Uses cached compiled regexps for performance — zero recompilation on repeat calls.
// Keywords are only matched when they appear in ws-lang syntactic positions:
// followed by ':' (YAML key) or '(' (function call), or end-of-line.
func (m *Manager) Translate(content string, langCode string) string {
	fwd := m.GetForward(langCode)
	if fwd == nil {
		return content
	}

	// Sort keys by length descending to avoid partial replacements
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(fwd))
	for k, v := range fwd {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		return len(pairs[i].k) > len(pairs[j].k)
	})

	result := content
	for _, p := range pairs {
		re := m.getRegexp(langCode, p.k)
		result = re.ReplaceAllString(result, `${1}`+p.v+`${2}`)
	}
	return result
}

// IsKeyword checks if a given key matches any canonical keyword.
// Returns the canonical keyword if matched, empty string otherwise.
func (m *Manager) IsKeyword(key string) (string, bool) {
	// First check if key is already canonical
	if m.canonSet[key] {
		return key, true
	}
	// Check if key is a native keyword
	if m.allKeys[key] {
		// Find what canonical it maps to
		for _, fwd := range m.forwards {
			if canon, ok := fwd[key]; ok {
				return canon, true
			}
		}
	}
	return "", false
}

// IsCanonical checks if a key matches ANY of the given canonical keywords (OR logic)
func (m *Manager) IsCanonical(key string, canons ...string) bool {
	canon, ok := m.IsKeyword(key)
	if !ok {
		return false
	}
	for _, c := range canons {
		if canon == c {
			return true
		}
	}
	return false
}

// NativeKeywords returns all native keywords that map to the given canonical keyword(s)
func (m *Manager) NativeKeywords(canons ...string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool)
	var result []string
	for _, rev := range m.reverses {
		for canon, natives := range rev {
			for _, c := range canons {
				if canon == c {
					for _, n := range natives {
						if !seen[n] {
							seen[n] = true
							result = append(result, n)
						}
					}
				}
			}
		}
	}
	return result
}

// GetForwardMap returns the forward map for compiler use
// (This is a convenience function for backward compatibility)
func GetForwardMap(langCode string) Forward {
	mgr := GetManager()
	return mgr.GetForward(langCode)
}

// TranslateContent translates content using the manager
func TranslateContent(content string, langCode string) string {
	mgr := GetManager()
	return mgr.Translate(content, langCode)
}
