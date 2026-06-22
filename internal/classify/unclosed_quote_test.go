package classify

import "testing"

// TestUnclosedQuoteCommands verifies that commands with unclosed quotes are
// classified as CMD, not NL. This prevents incomplete commands from being
// misrouted to the agent.
func TestUnclosedQuoteCommands(t *testing.T) {
	c := New(knownCommandsForTest)

	cases := []struct {
		input  string
		reason string
	}{
		// The exact case the user reported
		{`git add -A && git commit -m "feat: add model configuration hints`, "git commit unclosed dquote"},
		{`git add -A && git commit -m "feat: add model configuration hints when no model or credential is configured`, "git commit long unclosed dquote"},
		// Single unclosed double quote
		{`git commit -m "fix the bug`, "git commit unclosed dquote"},
		{`echo "hello world`, "echo unclosed dquote"},
		{`docker run --name "my container`, "docker unclosed dquote"},
		// Single unclosed single quote
		{`git commit -m 'fix the bug`, "git commit unclosed squote"},
		{`sed 's/old/new`, "sed unclosed squote"},
		// NL signal word before the command but command is known
		{`please git commit -m "unclosed`, "NL word before known command with unclosed quote"},
		// Grayzone verb with unclosed quote and command-like args
		{`find . -name "test`, "find unclosed dquote"},
		{`echo -n "hello`, "echo with flag unclosed dquote"},
	}

	for _, tc := range cases {
		got := c.Classify(tc.input)
		if got.Label != CMD {
			t.Errorf("[%s] %q: got %s, want CMD (reason=%s)", tc.reason, tc.input, got.Label, got.Reason)
		}
	}
}

// TestUnclosedQuoteNLWithApostrophe verifies that NL with apostrophes (which
// cause parse failures) is still correctly classified as NL.
func TestUnclosedQuoteNLWithApostrophe(t *testing.T) {
	c := New(knownCommandsForTest)

	cases := []struct {
		input  string
		reason string
	}{
		{"what's the problem", "NL with apostrophe"},
		{"how's it going", "NL with apostrophe"},
		{"what's up", "NL with apostrophe"},
	}

	for _, tc := range cases {
		got := c.Classify(tc.input)
		if got.Label != NL {
			t.Errorf("[%s] %q: got %s, want NL (reason=%s)", tc.reason, tc.input, got.Label, got.Reason)
		}
	}
}

// TestHasUnclosedQuote verifies the hasUnclosedQuote helper.
func TestHasUnclosedQuote(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{`echo "hello"`, false},
		{`echo "hello`, true},
		{`echo 'hello'`, false},
		{`echo 'hello`, true},
		{`git commit -m "fix"`, false},
		{`git commit -m "fix`, true},
		{`echo "it's"`, false}, // balanced: dquote + squote inside + dquote
		{`what's up`, true},    // odd single quotes
		{`echo "hello\"world"`, false}, // escaped dquote inside
		{`echo "hello`, true},
		{``, false},
		{`no quotes here`, false},
	}
	for _, tc := range cases {
		got := hasUnclosedQuote(tc.input)
		if got != tc.want {
			t.Errorf("hasUnclosedQuote(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// TestFirstToken verifies the firstToken helper.
func TestFirstToken(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"git status", "git"},
		{"  git status", "git"},
		{`"git" status`, "git"},
		{`'echo' hello`, "echo"},
		{"single", "single"},
		{"", ""},
		{"   ", ""},
		{`"unclosed`, "unclosed"},
	}
	for _, tc := range cases {
		got := firstToken(tc.input)
		if got != tc.want {
			t.Errorf("firstToken(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
