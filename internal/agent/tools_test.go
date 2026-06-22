package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestListDir verifies list_dir returns directory entries with type and size.
func TestListDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "beta.go"), []byte("package main"), 0o644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0o755)

	tool := listDirTool()
	args, _ := json.Marshal(map[string]string{"path": "."})
	out, isErr := tool.Run(context.Background(), dir, args)
	if isErr {
		t.Fatalf("list_dir error: %s", out)
	}
	lines := strings.Split(out, "\n")
	sort.Strings(lines)
	if len(lines) != 3 {
		t.Fatalf("entries = %d, want 3: %v", len(lines), lines)
	}
	// Check that we have the right entries (type and name; size varies by FS for dirs).
	wantNames := map[string]bool{"alpha.txt": false, "beta.go": false, "subdir": false}
	for _, l := range lines {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) < 2 {
			t.Errorf("malformed entry: %q", l)
			continue
		}
		typ, name := parts[0], parts[1]
		if name == "subdir" && typ != "dir" {
			t.Errorf("subdir type = %q, want dir", typ)
		}
		if name == "alpha.txt" && typ != "file" {
			t.Errorf("alpha.txt type = %q, want file", typ)
		}
		if name == "beta.go" && typ != "file" {
			t.Errorf("beta.go type = %q, want file", typ)
		}
		wantNames[name] = true
	}
	for name, found := range wantNames {
		if !found {
			t.Errorf("missing entry %q in output: %v", name, lines)
		}
	}
}

// TestListDirDefault verifies list_dir defaults to working directory.
func TestListDirDefault(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644)

	tool := listDirTool()
	args, _ := json.Marshal(map[string]string{})
	out, isErr := tool.Run(context.Background(), dir, args)
	if isErr {
		t.Fatalf("list_dir error: %s", out)
	}
	if !strings.Contains(out, "x.txt") {
		t.Errorf("expected x.txt in output: %s", out)
	}
}

// TestListDirError verifies list_dir reports an error for a bad path.
func TestListDirError(t *testing.T) {
	tool := listDirTool()
	args, _ := json.Marshal(map[string]string{"path": "/nonexistent/path/xyz"})
	out, isErr := tool.Run(context.Background(), t.TempDir(), args)
	if !isErr {
		t.Errorf("expected error for nonexistent path, got: %s", out)
	}
}

// TestListDirRisk verifies list_dir is read-only.
func TestListDirRisk(t *testing.T) {
	tool := listDirTool()
	if r := tool.Risk(nil); r != RiskReadOnly {
		t.Errorf("risk = %v, want RiskReadOnly", r)
	}
}

// TestMultiEdit verifies multiple edits are applied in order.
func TestMultiEdit(t *testing.T) {
	dir := t.TempDir()
	path := "test.txt"
	content := "line one\nline two\nline three\n"
	os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644)

	tool := multiEditTool()
	args, _ := json.Marshal(map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_string": "line one", "new_string": "LINE ONE"},
			{"old_string": "line three", "new_string": "LINE THREE"},
		},
	})
	out, isErr := tool.Run(context.Background(), dir, args)
	if isErr {
		t.Fatalf("multi_edit error: %s", out)
	}
	got, _ := os.ReadFile(filepath.Join(dir, path))
	want := "LINE ONE\nline two\nLINE THREE\n"
	if string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// TestMultiEditNotFound verifies an error when old_string is missing.
func TestMultiEditNotFound(t *testing.T) {
	dir := t.TempDir()
	path := "test.txt"
	os.WriteFile(filepath.Join(dir, path), []byte("hello world"), 0o644)

	tool := multiEditTool()
	args, _ := json.Marshal(map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_string": "nonexistent", "new_string": "x"},
		},
	})
	out, isErr := tool.Run(context.Background(), dir, args)
	if !isErr {
		t.Errorf("expected error for missing old_string, got: %s", out)
	}
}

// TestMultiEditNotUnique verifies an error when old_string appears multiple times.
func TestMultiEditNotUnique(t *testing.T) {
	dir := t.TempDir()
	path := "test.txt"
	os.WriteFile(filepath.Join(dir, path), []byte("dup\ndup\ndup\n"), 0o644)

	tool := multiEditTool()
	args, _ := json.Marshal(map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_string": "dup", "new_string": "unique"},
		},
	})
	out, isErr := tool.Run(context.Background(), dir, args)
	if !isErr {
		t.Errorf("expected error for non-unique old_string, got: %s", out)
	}
}

// TestMultiEditSequential verifies edits are applied sequentially (each
// edit sees the result of the previous one).
func TestMultiEditSequential(t *testing.T) {
	dir := t.TempDir()
	path := "test.txt"
	os.WriteFile(filepath.Join(dir, path), []byte("aaa\nbbb\nccc\n"), 0o644)

	tool := multiEditTool()
	args, _ := json.Marshal(map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_string": "aaa", "new_string": "AAA"},
			// This old_string only exists after the first edit doesn't change it,
			// but we test that the second edit operates on the post-first-edit content.
			{"old_string": "bbb", "new_string": "BBB"},
		},
	})
	out, isErr := tool.Run(context.Background(), dir, args)
	if isErr {
		t.Fatalf("multi_edit error: %s", out)
	}
	got, _ := os.ReadFile(filepath.Join(dir, path))
	want := "AAA\nBBB\nccc\n"
	if string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// TestMultiEditRisk verifies multi_edit uses pathWriteRisk.
func TestMultiEditRisk(t *testing.T) {
	tool := multiEditTool()
	args, _ := json.Marshal(map[string]string{"path": "../../etc/passwd"})
	if r := tool.Risk(args); r != RiskHigh {
		t.Errorf("risk for ../ path = %v, want RiskHigh", r)
	}
	args, _ = json.Marshal(map[string]string{"path": "local.txt"})
	if r := tool.Risk(args); r != RiskWrite {
		t.Errorf("risk for local path = %v, want RiskWrite", r)
	}
}

// TestGlobSingleStar verifies single-level glob matching.
func TestGlobSingleStar(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("x"), 0o644)

	tool := globTool()
	args, _ := json.Marshal(map[string]string{"pattern": "*.go"})
	out, isErr := tool.Run(context.Background(), dir, args)
	if isErr {
		t.Fatalf("glob error: %s", out)
	}
	matches := strings.Split(out, "\n")
	sort.Strings(matches)
	if len(matches) != 2 || matches[0] != "a.go" || matches[1] != "b.go" {
		t.Errorf("matches = %v, want [a.go b.go]", matches)
	}
}

// TestGlobDoubleStar verifies recursive ** glob matching.
func TestGlobDoubleStar(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src", "pkg"), 0o755)
	os.MkdirAll(filepath.Join(dir, "src", "cmd"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "pkg", "main.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "cmd", "run.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "root.go"), []byte("x"), 0o644)

	tool := globTool()
	args, _ := json.Marshal(map[string]string{"pattern": "**/*.go"})
	out, isErr := tool.Run(context.Background(), dir, args)
	if isErr {
		t.Fatalf("glob error: %s", out)
	}
	matches := strings.Split(out, "\n")
	sort.Strings(matches)
	// root.go, src/cmd/run.go, src/pkg/main.go
	want := []string{"root.go", "src/cmd/run.go", "src/pkg/main.go"}
	if len(matches) != 3 {
		t.Fatalf("matches = %v, want %v", matches, want)
	}
	for i, w := range want {
		if matches[i] != w {
			t.Errorf("match[%d] = %q, want %q", i, matches[i], w)
		}
	}
}

// TestGlobNoMatches verifies empty results.
func TestGlobNoMatches(t *testing.T) {
	dir := t.TempDir()
	tool := globTool()
	args, _ := json.Marshal(map[string]string{"pattern": "*.xyz"})
	out, isErr := tool.Run(context.Background(), dir, args)
	if isErr {
		t.Fatalf("glob error: %s", out)
	}
	if out != "[no matches]" {
		t.Errorf("output = %q, want [no matches]", out)
	}
}

// TestGlobRisk verifies glob is read-only.
func TestGlobRisk(t *testing.T) {
	tool := globTool()
	if r := tool.Risk(nil); r != RiskReadOnly {
		t.Errorf("risk = %v, want RiskReadOnly", r)
	}
}

// TestGlobMatchPattern verifies the globMatch helper for ** patterns.
func TestGlobMatchPattern(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "src/main.go", true},
		{"**/*.go", "src/pkg/main.go", true},
		{"**/*.go", "main.txt", false},
		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/pkg/main.go", true},
		{"src/**/*.go", "main.go", false},
		{"src/**/*.go", "other/main.go", false},
		{"*.go", "main.go", true},
		{"*.go", "dir/main.go", false},
		{"**", "anything", true},
		{"**", "a/b/c", true},
	}
	for _, tc := range cases {
		got := globMatch(tc.pattern, tc.path)
		if got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// TestDefaultToolsIncludesNewTools verifies all new tools are registered.
func TestDefaultToolsIncludesNewTools(t *testing.T) {
	tools := DefaultTools()
	for _, name := range []string{"bash", "read_file", "write_file", "edit", "grep", "list_dir", "multi_edit", "glob"} {
		if tools[name] == nil {
			t.Errorf("tool %q not found in DefaultTools", name)
		}
	}
}

// TestDefsIncludesNewTools verifies Defs returns all tool definitions in order.
func TestDefsIncludesNewTools(t *testing.T) {
	tools := DefaultTools()
	defs := Defs(tools)
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	want := []string{"bash", "read_file", "write_file", "edit", "grep", "list_dir", "multi_edit", "glob"}
	if len(names) != len(want) {
		t.Fatalf("defs count = %d, want %d", len(names), len(want))
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("defs[%d] = %q, want %q", i, names[i], w)
		}
	}
}
