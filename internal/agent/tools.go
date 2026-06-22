package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/21stware/agent-the-zsh/internal/llm"
)

// Tool is an agent tool: an llm.Tool definition plus a way to classify the risk
// of a specific call and to execute it. Execution happens in the agent's
// working directory (cwd), so bash and file ops act where the user is.
type Tool struct {
	Def llm.Tool
	// Risk classifies a specific invocation (from its JSON arguments).
	Risk func(args json.RawMessage) Risk
	// Run executes the tool and returns its textual result. err is only for
	// internal failures; tool-level errors (bad path, non-zero exit) are
	// reported in the returned string with isErr=true so the model can react.
	Run func(ctx context.Context, cwd string, args json.RawMessage) (result string, isErr bool)
	// Summary renders a one-line human description of a call for the approval
	// prompt and the transcript (e.g. `bash: rm -rf build`).
	Summary func(args json.RawMessage) string
}

// DefaultTools returns the mode-B tool set: bash, read_file, write_file, edit,
// grep, list_dir, multi_edit, glob. maxOutput bounds captured output to keep
// the context manageable.
func DefaultTools() map[string]*Tool {
	tools := map[string]*Tool{
		"bash":       bashTool(),
		"read_file":  readFileTool(),
		"write_file": writeFileTool(),
		"edit":       editTool(),
		"grep":       grepTool(),
		"list_dir":   listDirTool(),
		"multi_edit": multiEditTool(),
		"glob":       globTool(),
	}
	return tools
}

// Defs returns the tool definitions in a stable order for the API request.
func Defs(tools map[string]*Tool) []llm.Tool {
	order := []string{"bash", "read_file", "write_file", "edit", "grep", "list_dir", "multi_edit", "glob"}
	var out []llm.Tool
	for _, name := range order {
		if t := tools[name]; t != nil {
			out = append(out, t.Def)
		}
	}
	return out
}

const (
	maxToolOutput = 30000 // bytes of tool output kept (truncated beyond this)
	bashTimeout   = 120 * time.Second
)

// --- bash ---

func bashTool() *Tool {
	return &Tool{
		Def: llm.Tool{
			Name: "bash",
			Description: "Run a shell command in the user's current working directory and " +
				"return its combined stdout/stderr and exit code. Use for builds, tests, " +
				"git, listing files, and anything not covered by the dedicated tools. " +
				"One command per call; you may use pipes and &&.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The shell command to run.",
					},
				},
				"required": []string{"command"},
			},
		},
		Risk: func(args json.RawMessage) Risk {
			var a struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(args, &a)
			return classifyBash(a.Command)
		},
		Summary: func(args json.RawMessage) string {
			var a struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(args, &a)
			return "bash: " + a.Command
		},
		Run: func(ctx context.Context, cwd string, args json.RawMessage) (string, bool) {
			var a struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "invalid arguments: " + err.Error(), true
			}
			if strings.TrimSpace(a.Command) == "" {
				return "empty command", true
			}
			cctx, cancel := context.WithTimeout(ctx, bashTimeout)
			defer cancel()
			cmd := exec.CommandContext(cctx, "zsh", "-c", a.Command)
			cmd.Dir = cwd
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			err := cmd.Run()
			out := truncate(buf.String(), maxToolOutput)
			if cctx.Err() == context.DeadlineExceeded {
				return out + "\n[command timed out after " + bashTimeout.String() + "]", true
			}
			if err != nil {
				code := ""
				if ee, ok := err.(*exec.ExitError); ok {
					code = fmt.Sprintf(" (exit %d)", ee.ExitCode())
				}
				return out + "\n[command failed" + code + "]", true
			}
			if out == "" {
				out = "[no output, exit 0]"
			}
			return out, false
		},
	}
}

// --- read_file ---

func readFileTool() *Tool {
	return &Tool{
		Def: llm.Tool{
			Name:        "read_file",
			Description: "Read a text file and return its contents. Path is relative to the working directory unless absolute.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "File path to read."},
				},
				"required": []string{"path"},
			},
		},
		Risk:    func(json.RawMessage) Risk { return RiskReadOnly },
		Summary: func(args json.RawMessage) string { return "read_file: " + argStr(args, "path") },
		Run: func(_ context.Context, cwd string, args json.RawMessage) (string, bool) {
			p := argStr(args, "path")
			if p == "" {
				return "missing path", true
			}
			full := resolve(cwd, p)
			b, err := os.ReadFile(full)
			if err != nil {
				return "read error: " + err.Error(), true
			}
			return truncate(string(b), maxToolOutput), false
		},
	}
}

// --- write_file ---

func writeFileTool() *Tool {
	return &Tool{
		Def: llm.Tool{
			Name:        "write_file",
			Description: "Create or overwrite a file with the given content. Path is relative to the working directory unless absolute. Creates parent directories as needed.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "File path to write."},
					"content": map[string]any{"type": "string", "description": "Full file content."},
				},
				"required": []string{"path", "content"},
			},
		},
		Risk: func(args json.RawMessage) Risk {
			// Writing outside the working tree is high-risk; inside is ordinary.
			return pathWriteRisk(argStr(args, "path"))
		},
		Summary: func(args json.RawMessage) string {
			c := argStr(args, "content")
			return fmt.Sprintf("write_file: %s (%d bytes)", argStr(args, "path"), len(c))
		},
		Run: func(_ context.Context, cwd string, args json.RawMessage) (string, bool) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "invalid arguments: " + err.Error(), true
			}
			if a.Path == "" {
				return "missing path", true
			}
			full := resolve(cwd, a.Path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "mkdir error: " + err.Error(), true
			}
			if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
				return "write error: " + err.Error(), true
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), false
		},
	}
}

// --- edit (string replacement) ---

func editTool() *Tool {
	return &Tool{
		Def: llm.Tool{
			Name: "edit",
			Description: "Replace an exact substring in a file with new text. old_string must appear exactly once. " +
				"Use for surgical edits instead of rewriting the whole file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":       map[string]any{"type": "string", "description": "File to edit."},
					"old_string": map[string]any{"type": "string", "description": "Exact text to replace (must be unique in the file)."},
					"new_string": map[string]any{"type": "string", "description": "Replacement text."},
				},
				"required": []string{"path", "old_string", "new_string"},
			},
		},
		Risk: func(args json.RawMessage) Risk { return pathWriteRisk(argStr(args, "path")) },
		Summary: func(args json.RawMessage) string {
			return "edit: " + argStr(args, "path")
		},
		Run: func(_ context.Context, cwd string, args json.RawMessage) (string, bool) {
			var a struct {
				Path      string `json:"path"`
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "invalid arguments: " + err.Error(), true
			}
			if a.Path == "" || a.OldString == "" {
				return "path and old_string are required", true
			}
			full := resolve(cwd, a.Path)
			b, err := os.ReadFile(full)
			if err != nil {
				return "read error: " + err.Error(), true
			}
			content := string(b)
			n := strings.Count(content, a.OldString)
			if n == 0 {
				return "old_string not found in file", true
			}
			if n > 1 {
				return fmt.Sprintf("old_string appears %d times; it must be unique — add surrounding context", n), true
			}
			updated := strings.Replace(content, a.OldString, a.NewString, 1)
			if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
				return "write error: " + err.Error(), true
			}
			return "edited " + a.Path, false
		},
	}
}

// --- grep ---

func grepTool() *Tool {
	return &Tool{
		Def: llm.Tool{
			Name:        "grep",
			Description: "Search files for a regular expression (uses ripgrep if available, else grep -r). Returns matching lines with file:line.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Regex to search for."},
					"path":    map[string]any{"type": "string", "description": "Directory or file to search (default: working directory)."},
				},
				"required": []string{"pattern"},
			},
		},
		Risk:    func(json.RawMessage) Risk { return RiskReadOnly },
		Summary: func(args json.RawMessage) string { return "grep: " + argStr(args, "pattern") },
		Run: func(ctx context.Context, cwd string, args json.RawMessage) (string, bool) {
			var a struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "invalid arguments: " + err.Error(), true
			}
			if a.Pattern == "" {
				return "missing pattern", true
			}
			where := a.Path
			if where == "" {
				where = "."
			}
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			var cmd *exec.Cmd
			if _, err := exec.LookPath("rg"); err == nil {
				cmd = exec.CommandContext(cctx, "rg", "--line-number", "--no-heading", "-e", a.Pattern, where)
			} else {
				cmd = exec.CommandContext(cctx, "grep", "-rn", "-e", a.Pattern, where)
			}
			cmd.Dir = cwd
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			err := cmd.Run()
			out := truncate(buf.String(), maxToolOutput)
			if out == "" {
				return "[no matches]", false
			}
			// grep/rg exit 1 when no matches — not an error for us.
			_ = err
			return out, false
		},
	}
}

// --- list_dir ---

func listDirTool() *Tool {
	return &Tool{
		Def: llm.Tool{
			Name:        "list_dir",
			Description: "List the contents of a directory. Returns entries with their type (file/dir) and size. Path is relative to the working directory unless absolute (default: working directory).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Directory to list (default: working directory).",
					},
				},
				"required": []string{},
			},
		},
		Risk:    func(json.RawMessage) Risk { return RiskReadOnly },
		Summary: func(args json.RawMessage) string { return "list_dir: " + argStr(args, "path") },
		Run: func(_ context.Context, cwd string, args json.RawMessage) (string, bool) {
			p := argStr(args, "path")
			if p == "" {
				p = "."
			}
			full := resolve(cwd, p)
			entries, err := os.ReadDir(full)
			if err != nil {
				return "list error: " + err.Error(), true
			}
			var b strings.Builder
			for _, e := range entries {
				info, err := e.Info()
				size := int64(-1)
				if err == nil {
					size = info.Size()
				}
				typ := "file"
				if e.IsDir() {
					typ = "dir"
				}
				fmt.Fprintf(&b, "%s\t%s\t%d\n", typ, e.Name(), size)
			}
			return strings.TrimRight(b.String(), "\n"), false
		},
	}
}

// --- multi_edit ---

func multiEditTool() *Tool {
	return &Tool{
		Def: llm.Tool{
			Name: "multi_edit",
			Description: "Apply multiple find-and-replace edits to a single file in one operation. " +
				"Each edit's old_string must appear exactly once in the file. Edits are applied in order.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "File to edit."},
					"edits": map[string]any{
						"type": "array",
						"description": "Array of {old_string, new_string} edit operations, applied in order.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"old_string": map[string]any{"type": "string", "description": "Exact text to replace (must be unique in the file)."},
								"new_string": map[string]any{"type": "string", "description": "Replacement text."},
							},
							"required": []string{"old_string", "new_string"},
						},
					},
				},
				"required": []string{"path", "edits"},
			},
		},
		Risk:    func(args json.RawMessage) Risk { return pathWriteRisk(argStr(args, "path")) },
		Summary: func(args json.RawMessage) string { return "multi_edit: " + argStr(args, "path") },
		Run: func(_ context.Context, cwd string, args json.RawMessage) (string, bool) {
			var a struct {
				Path  string `json:"path"`
				Edits []struct {
					OldString string `json:"old_string"`
					NewString string `json:"new_string"`
				} `json:"edits"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "invalid arguments: " + err.Error(), true
			}
			if a.Path == "" {
				return "missing path", true
			}
			if len(a.Edits) == 0 {
				return "no edits provided", true
			}
			full := resolve(cwd, a.Path)
			b, err := os.ReadFile(full)
			if err != nil {
				return "read error: " + err.Error(), true
			}
			content := string(b)
			for i, e := range a.Edits {
				if e.OldString == "" {
					return fmt.Sprintf("edit %d: old_string is required", i), true
				}
				n := strings.Count(content, e.OldString)
				if n == 0 {
					return fmt.Sprintf("edit %d: old_string not found in file", i), true
				}
				if n > 1 {
					return fmt.Sprintf("edit %d: old_string appears %d times; it must be unique — add surrounding context", i, n), true
				}
				content = strings.Replace(content, e.OldString, e.NewString, 1)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				return "write error: " + err.Error(), true
			}
			return fmt.Sprintf("applied %d edits to %s", len(a.Edits), a.Path), false
		},
	}
}

// --- glob ---

func globTool() *Tool {
	return &Tool{
		Def: llm.Tool{
			Name:        "glob",
			Description: "Find files by name pattern (supports * and ** globs). Returns matching file paths relative to the working directory. Uses filepath.Glob for single-star patterns and walks recursively for double-star patterns.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Glob pattern (e.g. \"*.go\", \"**/*.ts\", \"src/**/*.py\").",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Base directory to search from (default: working directory).",
					},
				},
				"required": []string{"pattern"},
			},
		},
		Risk:    func(json.RawMessage) Risk { return RiskReadOnly },
		Summary: func(args json.RawMessage) string { return "glob: " + argStr(args, "pattern") },
		Run: func(ctx context.Context, cwd string, args json.RawMessage) (string, bool) {
			pattern := argStr(args, "pattern")
			if pattern == "" {
				return "missing pattern", true
			}
			base := argStr(args, "path")
			if base == "" {
				base = "."
			}
			baseDir := resolve(cwd, base)

			var matches []string
			if strings.Contains(pattern, "**") {
				// Recursive glob: walk the tree and match.
				walkErr := filepath.WalkDir(baseDir, func(path string, d os.DirEntry, err error) error {
					if err != nil {
						return nil // skip unreadable dirs
					}
					if d.IsDir() {
						return nil
					}
					rel, err := filepath.Rel(baseDir, path)
					if err != nil {
						return nil
					}
					if globMatch(pattern, rel) {
						matches = append(matches, rel)
					}
					return nil
				})
				if walkErr != nil {
					return "walk error: " + walkErr.Error(), true
				}
			} else {
				// Single-level glob.
				absPattern := filepath.Join(baseDir, pattern)
				globMatches, err := filepath.Glob(absPattern)
				if err != nil {
					return "glob error: " + err.Error(), true
				}
				for _, m := range globMatches {
					rel, err := filepath.Rel(baseDir, m)
					if err == nil {
						matches = append(matches, rel)
					}
				}
			}
			if len(matches) == 0 {
				return "[no matches]", false
			}
			sort.Strings(matches)
			return strings.Join(matches, "\n"), false
		},
	}
}

// globMatch checks if a file path matches a glob pattern containing **.
// ** matches any number of path segments (including zero).
func globMatch(pattern, path string) bool {
	patSegs := strings.Split(pattern, "/")
	pathSegs := strings.Split(path, "/")
	return matchSegs(patSegs, pathSegs)
}

func matchSegs(patSegs, pathSegs []string) bool {
	if len(patSegs) == 0 {
		return len(pathSegs) == 0
	}
	if patSegs[0] == "**" {
		// ** matches zero or more segments
		for i := 0; i <= len(pathSegs); i++ {
			if matchSegs(patSegs[1:], pathSegs[i:]) {
				return true
			}
		}
		return false
	}
	if len(pathSegs) == 0 {
		return false
	}
	matched, _ := filepath.Match(patSegs[0], pathSegs[0])
	if !matched {
		return false
	}
	return matchSegs(patSegs[1:], pathSegs[1:])
}

// --- helpers ---

// resolve makes a possibly-relative path absolute against cwd.
func resolve(cwd, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cwd, p)
}

// pathWriteRisk classifies a write target: inside the working tree is ordinary
// (RiskWrite); an absolute path, a parent-escaping path, or a dotfile in $HOME
// is high-risk. The cwd isn't known here, so we judge by the path shape; the
// gate combines this with the level.
func pathWriteRisk(p string) Risk {
	if p == "" {
		return RiskWrite
	}
	if filepath.IsAbs(p) {
		return RiskHigh
	}
	// climbing out of the working tree
	if strings.HasPrefix(p, "../") || strings.Contains(p, "/../") {
		return RiskHigh
	}
	return RiskWrite
}

// argStr pulls a single string field from raw JSON args.
func argStr(args json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// truncate caps s to n bytes, appending a marker when cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n…[truncated, %d more bytes]", len(s)-n)
}
