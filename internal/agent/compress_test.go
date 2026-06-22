package agent

import (
	"strings"
	"testing"

	"github.com/21stware/agent-the-zsh/internal/llm"
)

// TestCompressHistoryNoOp verifies that small conversations are not compressed.
func TestCompressHistoryNoOp(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("hello")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("hi there")}},
	}
	out := compressHistory(msgs, 10000)
	if len(out) != len(msgs) {
		t.Errorf("compressed %d msgs to %d, expected no change", len(msgs), len(out))
	}
}

// TestCompressHistoryTruncatesOldToolResults verifies that old tool_result
// blocks are truncated when the budget is exceeded.
func TestCompressHistoryTruncatesOldToolResults(t *testing.T) {
	bigResult := strings.Repeat("x", 5000)
	msgs := []llm.Message{
		// Old user turn with a tool_result
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultBlock("tool_1", bigResult, false),
		}},
		// Old assistant turn
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}},
		// Another old tool result
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultBlock("tool_2", bigResult, false),
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}},
		// Recent turns (kept intact)
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("recent question")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("recent answer")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("another recent")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("another answer")}},
	}

	out := compressHistory(msgs, 2000)
	if len(out) != len(msgs) {
		t.Fatalf("message count changed: %d -> %d", len(msgs), len(out))
	}

	// Old tool results should be truncated (only msgs before the keepRecent window).
	old1 := findToolResult(out[0])
	if old1 == nil {
		t.Fatal("expected tool_result in msg[0]")
	}
	if len(old1.Content) >= len(bigResult) {
		t.Errorf("old tool_result not truncated: len=%d", len(old1.Content))
	}
	if !strings.Contains(old1.Content, "compressed") {
		t.Errorf("expected compression marker in old tool_result, got: %q", old1.Content[:min(100, len(old1.Content))])
	}

	// msg[2] is within the keepRecent window (8 msgs, keepRecent=6, cutoff=2)
	// so it should NOT be truncated.
	old2 := findToolResult(out[2])
	if old2 == nil {
		t.Fatal("expected tool_result in msg[2]")
	}
	if old2.Content != bigResult {
		t.Errorf("msg[2] should be preserved (within keepRecent), but was modified: len=%d", len(old2.Content))
	}

	// Recent text should be intact.
	recentText := out[5].Content[0].Text
	if recentText != "recent answer" {
		t.Errorf("recent text = %q, want %q", recentText, "recent answer")
	}
}

// TestCompressHistoryPreservesRecent verifies that the last keepRecent
// messages are never truncated.
func TestCompressHistoryPreservesRecent(t *testing.T) {
	bigResult := strings.Repeat("y", 5000)
	// Create exactly keepRecent+2 messages so some are old.
	msgs := make([]llm.Message, 8)
	for i := range msgs {
		msgs[i] = llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				llm.ToolResultBlock("t", bigResult, false),
			},
		}
	}

	out := compressHistory(msgs, 1000)

	// Last 6 should be intact.
	for i := len(out) - 6; i < len(out); i++ {
		tr := findToolResult(out[i])
		if tr == nil {
			t.Errorf("msg[%d] missing tool_result", i)
			continue
		}
		if tr.Content != bigResult {
			t.Errorf("recent msg[%d] was truncated: len=%d, want %d", i, len(tr.Content), len(bigResult))
		}
	}
}

// TestCompressHistoryDoesNotMutateOriginal verifies the original slice is
// not modified.
func TestCompressHistoryDoesNotMutateOriginal(t *testing.T) {
	bigResult := strings.Repeat("z", 5000)
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultBlock("t", bigResult, false),
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("q")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("a")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("q2")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("a2")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("q3")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("a3")}},
	}

	originalContent := msgs[0].Content[0].Content
	_ = compressHistory(msgs, 1000)
	if msgs[0].Content[0].Content != originalContent {
		t.Errorf("compressHistory mutated the original slice")
	}
}

// TestCompressHistorySmallBudget verifies compression triggers even with
// a tiny budget.
func TestCompressHistorySmallBudget(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultBlock("t", "short result", false),
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("q")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("a")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("q2")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("a2")}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("q3")}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("a3")}},
	}

	out := compressHistory(msgs, 10)
	// Short result should not be truncated (it's below truncLen=500).
	tr := findToolResult(out[0])
	if tr == nil {
		t.Fatal("expected tool_result")
	}
	if tr.Content != "short result" {
		t.Errorf("short result was modified: %q", tr.Content)
	}
}

// TestTruncateMid verifies the truncateMid helper.
func TestTruncateMid(t *testing.T) {
	s := strings.Repeat("A", 1000)
	got := truncateMid(s, 100)
	if len(got) >= 1000 {
		t.Errorf("not truncated: len=%d", len(got))
	}
	if !strings.Contains(got, "compressed") {
		t.Errorf("missing compression marker")
	}
	// Small string should be returned as-is.
	short := "hello"
	if got := truncateMid(short, 100); got != short {
		t.Errorf("short string = %q, want %q", got, short)
	}
}

// TestMessageSize verifies the messageSize helper.
func TestMessageSize(t *testing.T) {
	m := llm.Message{
		Content: []llm.ContentBlock{
			llm.TextBlock("hello"),
			llm.ToolResultBlock("t", "result", false),
		},
	}
	size := messageSize(m)
	if size != len("hello")+len("result") {
		t.Errorf("size = %d, want %d", size, len("hello")+len("result"))
	}
}

// findToolResult returns the first tool_result block in a message, or nil.
func findToolResult(m llm.Message) *llm.ContentBlock {
	for i := range m.Content {
		if m.Content[i].Type == "tool_result" {
			return &m.Content[i]
		}
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
