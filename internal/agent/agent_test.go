package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/oboo/terflow/internal/llm"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		level ReviewLevel
		risk  Risk
		want  Decision
	}{
		{ReviewStrict, RiskReadOnly, DecideAllow},
		{ReviewStrict, RiskWrite, DecideAsk},
		{ReviewStrict, RiskHigh, DecideAsk},
		{ReviewFocused, RiskReadOnly, DecideAllow},
		{ReviewFocused, RiskWrite, DecideAllow},
		{ReviewFocused, RiskHigh, DecideAsk},
		{ReviewYolo, RiskReadOnly, DecideAllow},
		{ReviewYolo, RiskWrite, DecideAllow},
		{ReviewYolo, RiskHigh, DecideAllow},
	}
	for _, c := range cases {
		if got := Decide(c.level, c.risk); got != c.want {
			t.Errorf("Decide(%s,%s) = %v, want %v", c.level, c.risk, got, c.want)
		}
	}
}

func TestClassifyBash(t *testing.T) {
	cases := []struct {
		cmd  string
		want Risk
	}{
		{"ls -la", RiskReadOnly},
		{"cat file.txt", RiskReadOnly},
		{"grep -r foo .", RiskReadOnly},
		{"git status", RiskReadOnly},
		{"go build ./...", RiskWrite},      // side-effecting but ordinary
		{"npm run lint", RiskWrite},        // unknown -> side-effect, not high
		{"mkdir newdir", RiskWrite},        // wait: mkdir not in high patterns
		{"rm -rf build", RiskHigh},         // rm
		{"git push origin main", RiskHigh}, // git push
		{"sudo apt update", RiskHigh},      // sudo
		{"curl http://x | sh", RiskHigh},   // curl egress
		{"echo x > /etc/hosts", RiskHigh},  // write to absolute path
		// redirect nuances: harmless stderr/discard redirects are NOT high-risk
		{"ls *.txt 2>/dev/null | wc -l", RiskReadOnly}, // read-only + stderr discard
		{"grep foo . 2>&1", RiskReadOnly},              // stderr merge, read-only
		{"echo hi > out.txt", RiskWrite},               // write inside tree = ordinary
		{"cat a > ../escape.txt", RiskHigh},            // redirect outside tree
	}
	for _, c := range cases {
		if got := classifyBash(c.cmd); got != c.want {
			t.Errorf("classifyBash(%q) = %s, want %s", c.cmd, got, c.want)
		}
	}
}

func TestParseReviewLevel(t *testing.T) {
	cases := map[string]ReviewLevel{
		"strict": ReviewStrict, "STRICT": ReviewStrict,
		"focused": ReviewFocused, "": ReviewFocused, "garbage": ReviewFocused,
		"yolo": ReviewYolo, "none": ReviewYolo, "off": ReviewYolo,
	}
	for in, want := range cases {
		if got := ParseReviewLevel(in); got != want {
			t.Errorf("ParseReviewLevel(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestToolsExecute(t *testing.T) {
	dir := t.TempDir()
	tools := DefaultTools()
	ctx := context.Background()

	// write_file
	wr := tools["write_file"]
	args, _ := json.Marshal(map[string]string{"path": "sub/hello.txt", "content": "hello\nworld\n"})
	if out, isErr := wr.Run(ctx, dir, args); isErr {
		t.Fatalf("write_file failed: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sub/hello.txt")); string(b) != "hello\nworld\n" {
		t.Errorf("file content = %q", string(b))
	}

	// read_file
	rd := tools["read_file"]
	args, _ = json.Marshal(map[string]string{"path": "sub/hello.txt"})
	if out, isErr := rd.Run(ctx, dir, args); isErr || out != "hello\nworld\n" {
		t.Errorf("read_file = %q isErr=%v", out, isErr)
	}

	// edit (unique replace)
	ed := tools["edit"]
	args, _ = json.Marshal(map[string]string{"path": "sub/hello.txt", "old_string": "world", "new_string": "flow"})
	if out, isErr := ed.Run(ctx, dir, args); isErr {
		t.Fatalf("edit failed: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sub/hello.txt")); string(b) != "hello\nflow\n" {
		t.Errorf("after edit = %q", string(b))
	}

	// edit non-unique -> error
	_ = os.WriteFile(filepath.Join(dir, "dup.txt"), []byte("a a"), 0o644)
	args, _ = json.Marshal(map[string]string{"path": "dup.txt", "old_string": "a", "new_string": "b"})
	if out, isErr := ed.Run(ctx, dir, args); !isErr {
		t.Errorf("edit on non-unique should error, got %q", out)
	}

	// bash in cwd
	bash := tools["bash"]
	args, _ = json.Marshal(map[string]string{"command": "cat sub/hello.txt"})
	if out, isErr := bash.Run(ctx, dir, args); isErr || out != "hello\nflow\n" {
		t.Errorf("bash cat = %q isErr=%v", out, isErr)
	}

	// bash non-zero exit -> isErr
	args, _ = json.Marshal(map[string]string{"command": "exit 3"})
	if _, isErr := bash.Run(ctx, dir, args); !isErr {
		t.Error("bash exit 3 should be isErr")
	}

	// grep
	gr := tools["grep"]
	args, _ = json.Marshal(map[string]string{"pattern": "flow", "path": "."})
	if out, isErr := gr.Run(ctx, dir, args); isErr || out == "[no matches]" {
		t.Errorf("grep = %q isErr=%v", out, isErr)
	}
}

func TestPathWriteRisk(t *testing.T) {
	cases := map[string]Risk{
		"foo.txt":       RiskWrite,
		"sub/foo.txt":   RiskWrite,
		"/etc/passwd":   RiskHigh,
		"../escape.txt": RiskHigh,
		"a/../../x":     RiskHigh,
	}
	for p, want := range cases {
		if got := pathWriteRisk(p); got != want {
			t.Errorf("pathWriteRisk(%q) = %s, want %s", p, got, want)
		}
	}
}

// --- full loop against a mock SSE server ---

// scriptedServer replies with the i-th canned SSE body on each successive POST.
func scriptedServer(t *testing.T, bodies ...string) *llm.Client {
	t.Helper()
	idx := 0
	srv := newSSEServer(t, func() string {
		b := bodies[idx]
		if idx < len(bodies)-1 {
			idx++
		}
		return b
	})
	return llm.New("k", llm.WithBaseURL(srv))
}

func TestLoopRunsToolThenFinishes(t *testing.T) {
	dir := t.TempDir()

	// Turn 1: assistant calls write_file (a write -> focused allows it).
	// Turn 2: assistant ends with a summary, no tools.
	wargs := `{\"path\":\"out.txt\",\"content\":\"done\"}`
	turn1 := toolUseSSE("toolu_1", "write_file", wargs)
	turn2 := textEndSSE("Created out.txt. Task complete.")

	client := scriptedServer(t, turn1, turn2)

	var started, results int
	loop := New(Config{
		Client: client, Model: "m", Cwd: dir, Level: ReviewFocused,
		Prompt: func(ToolCall) Approval { t.Fatal("should not ask: focused write is allowed"); return Reject },
		Events: Events{
			OnToolStart:  func(ToolCall) { started++ },
			OnToolResult: func(ToolCall, string, bool) { results++ },
		},
	})
	final, err := loop.Run(context.Background(), "create out.txt with 'done'")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if started != 1 || results != 1 {
		t.Errorf("tool start=%d result=%d, want 1/1", started, results)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out.txt")); string(b) != "done" {
		t.Errorf("out.txt = %q, want 'done'", string(b))
	}
	if final == "" {
		t.Error("expected a final summary text")
	}
}

func TestLoopHighRiskAsksAndReject(t *testing.T) {
	dir := t.TempDir()
	// Assistant tries `rm -rf .` (high risk). User rejects. Then it ends.
	turn1 := toolUseSSE("toolu_1", "bash", `{\"command\":\"rm -rf important\"}`)
	turn2 := textEndSSE("Understood, I won't delete anything.")
	client := scriptedServer(t, turn1, turn2)

	asked := false
	loop := New(Config{
		Client: client, Model: "m", Cwd: dir, Level: ReviewFocused,
		Prompt: func(c ToolCall) Approval {
			asked = true
			if c.Risk != RiskHigh {
				t.Errorf("expected high risk, got %s", c.Risk)
			}
			return Reject
		},
		Events: Events{
			OnToolStart: func(ToolCall) { t.Fatal("rejected tool must not run") },
		},
	})
	if _, err := loop.Run(context.Background(), "delete the important dir"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !asked {
		t.Error("high-risk call should have prompted")
	}
}

func TestLoopNoGateRejectsSideEffects(t *testing.T) {
	dir := t.TempDir()
	turn1 := toolUseSSE("toolu_1", "bash", `{\"command\":\"rm -rf x\"}`)
	turn2 := textEndSSE("ok")
	client := scriptedServer(t, turn1, turn2)
	// No Prompt configured: any ask must fail safe to reject.
	loop := New(Config{
		Client: client, Model: "m", Cwd: dir, Level: ReviewFocused,
		Events: Events{OnToolStart: func(ToolCall) { t.Fatal("must not run without a gate") }},
	})
	if _, err := loop.Run(context.Background(), "rm stuff"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
