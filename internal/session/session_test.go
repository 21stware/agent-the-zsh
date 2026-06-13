package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/21stware/agent-the-zsh/internal/llm"
)

func TestPathFromTmpdir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	os.Unsetenv("XDG_RUNTIME_DIR")

	p, err := Path("abc")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(tmp, "flow-"+itoa(os.Getuid()), "sessions", "abc.jsonl")
	if p != want {
		t.Errorf("Path = %q, want %q", p, want)
	}
	// The sessions dir must exist and be 0700.
	info, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatalf("stat sessions dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("sessions dir perm = %o, want 700", perm)
	}
}

func TestPathXDGWins(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	t.Setenv("TMPDIR", t.TempDir())

	p, err := Path("xyz")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(xdg, "flow", "sessions", "xyz.jsonl")
	if p != want {
		t.Errorf("Path = %q, want %q", p, want)
	}
}

func TestPathEmptyID(t *testing.T) {
	p, err := Path("")
	if err != nil || p != "" {
		t.Errorf("Path(\"\") = (%q,%v), want (\"\",nil)", p, err)
	}
}

// TestRoundTrip is the key test: a transcript with a user text turn, an
// assistant turn carrying a tool_use block, and a user turn with a tool_result
// block must survive Append (in two separate calls) -> Load with every block
// field intact, proving the tagged-union JSON round-trips and incremental
// appends concatenate.
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")

	userTurn := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.TextBlock("rename foo to bar")},
	}
	assistantTurn := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.TextBlock("I'll do that."),
			{Type: "tool_use", ID: "toolu_1", Name: "bash",
				Input: json.RawMessage(`{"command":"mv foo bar"}`)},
		},
	}
	toolTurn := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{llm.ToolResultBlock("toolu_1", "renamed", false)},
	}

	// Two separate Append calls, mirroring how the loop persists turn by turn.
	if err := Append(path, []llm.Message{userTurn, assistantTurn}); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if err := Append(path, []llm.Message{toolTurn}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []llm.Message{userTurn, assistantTurn, toolTurn}
	if len(got) != len(want) {
		t.Fatalf("loaded %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Role != want[i].Role {
			t.Errorf("msg %d role = %q, want %q", i, got[i].Role, want[i].Role)
		}
		if len(got[i].Content) != len(want[i].Content) {
			t.Fatalf("msg %d block count = %d, want %d", i, len(got[i].Content), len(want[i].Content))
		}
		for j := range want[i].Content {
			wb, gb := want[i].Content[j], got[i].Content[j]
			if gb.Type != wb.Type || gb.Text != wb.Text || gb.ID != wb.ID ||
				gb.Name != wb.Name || gb.ToolUseID != wb.ToolUseID ||
				gb.Content != wb.Content || gb.IsError != wb.IsError ||
				string(gb.Input) != string(wb.Input) {
				t.Errorf("msg %d block %d = %+v, want %+v", i, j, gb, wb)
			}
		}
	}
}

func TestLoadMissing(t *testing.T) {
	msgs, err := Load(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || msgs != nil {
		t.Errorf("Load(missing) = (%v,%v), want (nil,nil)", msgs, err)
	}
}

func TestLoadToleratesPartialLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	good := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hi")}}
	if err := Append(path, []llm.Message{good}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Simulate a crash mid-write: a truncated JSON line with no newline.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"role":"assistant","content":[{"type":"text","te`)
	f.Close()

	msgs, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content[0].Text != "hi" {
		t.Errorf("Load = %+v, want only the one valid message", msgs)
	}
}

func TestAppendEmptyPath(t *testing.T) {
	if err := Append("", []llm.Message{{Role: llm.RoleUser}}); err != nil {
		t.Errorf("Append(\"\") = %v, want nil (no-op)", err)
	}
}

// TestListAndMeta checks that List enumerates sessions with cwd (from SaveMeta)
// and the last user message, newest first, excluding the current session.
func TestListAndMeta(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	os.Unsetenv("XDG_RUNTIME_DIR")

	write := func(id, dir, lastMsg string) {
		p, err := Path(id)
		if err != nil {
			t.Fatal(err)
		}
		msgs := []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("first")}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock(lastMsg)}},
		}
		if err := Append(p, msgs); err != nil {
			t.Fatal(err)
		}
		if err := SaveMeta(id, dir); err != nil {
			t.Fatal(err)
		}
	}
	write("aaa", "/home/u/proj", "deploy the thing")
	write("bbb", "/tmp/scratch", "what is 2+2")
	write("cur", "/x", "should be excluded")

	infos, err := List("cur")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List returned %d sessions, want 2 (cur excluded)", len(infos))
	}
	// Both present, each with cwd + last user message.
	byID := map[string]Info{}
	for _, in := range infos {
		byID[in.ID] = in
	}
	if got := byID["aaa"]; got.Cwd != "/home/u/proj" || got.LastUser != "deploy the thing" || got.Turns != 3 {
		t.Errorf("aaa = %+v, want cwd/proj, last 'deploy the thing', 3 turns", got)
	}
	if got := byID["bbb"]; got.Cwd != "/tmp/scratch" || got.LastUser != "what is 2+2" {
		t.Errorf("bbb = %+v, want cwd/scratch, last 'what is 2+2'", got)
	}
	if _, ok := byID["cur"]; ok {
		t.Error("current session should be excluded")
	}
}

func TestSaveMetaWriteOnce(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	os.Unsetenv("XDG_RUNTIME_DIR")
	if err := SaveMeta("z", "/first"); err != nil {
		t.Fatal(err)
	}
	if err := SaveMeta("z", "/second"); err != nil { // must not overwrite
		t.Fatal(err)
	}
	dir, _ := Dir()
	b, _ := os.ReadFile(filepath.Join(dir, "z.meta"))
	if want := `{"cwd":"/first"}`; string(b) != want {
		t.Errorf("meta = %s, want %s (first write wins)", b, want)
	}
}

// itoa avoids importing strconv just for the path-assembly assertion.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
