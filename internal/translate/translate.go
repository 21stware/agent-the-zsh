// Package translate implements mode A: turning a natural-language request into a
// single shell command, using the self-built llm client. It is the default NL
// path — reversible, no tools, no file-system effects. The translated command is
// written back to the user's input line for confirmation; it is never executed
// by flow itself.
package translate

import (
	"context"
	"strings"

	"github.com/oboo/terflow/internal/llm"
)

// systemPrompt instructs the model to either emit exactly one shell command
// (mode A) or, when the request needs multi-step hands-on work, route to the
// agent (mode B) by emitting a single sentinel line. The constraints matter:
// any prose, fences, or explanation would be written verbatim into the buffer.
const systemPrompt = `You route a natural-language request typed into an interactive zsh session. Output EXACTLY ONE of these three forms and ABSOLUTELY NOTHING ELSE — no explanation, no apology, no clarifying question, no prose:

1. A SINGLE shell command — when the request maps to one command.
   - Output only the command. No markdown, no code fences, no leading "$", no quotes around the whole thing, no comments.
   - One logical line; pipes, &&, ; are fine.

2. The exact line  ## AGENT  — when the request needs MULTIPLE steps, exploration, reading/analyzing/editing files, or is a QUESTION that requires inspecting the project/files/system to answer (e.g. "what language is this project", "explain this code", "how do I undo the last commit in this repo"). The agent will read files and answer or do the work.

3. The exact line  # cannot translate  — ONLY for requests that are neither a command nor a task nor answerable by inspecting the machine (e.g. "what is the meaning of life").

CRITICAL: You must NEVER write a sentence of explanation. If you are tempted to explain, clarify, or say something is ambiguous, output  ## AGENT  instead so the agent can handle it. Your entire output is fed directly into the user's shell input line, so anything other than the three forms above is a bug.

Examples:
- "list go files" -> find . -name '*.go'
- "what's using port 8080" -> lsof -i :8080
- "give me my ip" -> ifconfig | grep "inet "
- "what language is this project" -> ## AGENT
- "tell me about this project" -> ## AGENT
- "fix all the failing tests" -> ## AGENT
- "refactor this module to use channels" -> ## AGENT
- "what is the meaning of life" -> # cannot translate

Use the provided current directory and recent history as context.`

// CannotTranslate is the sentinel the model emits when no command fits.
const CannotTranslate = "# cannot translate"

// AgentSentinel is the line the model emits to route a request to mode B.
const AgentSentinel = "## AGENT"

// Context carries the situational inputs for a translation.
type Context struct {
	CWD     string
	History []string // recent commands, oldest first
}

// Translator wraps an llm.Client with the mode-A configuration.
type Translator struct {
	client *llm.Client
	model  string
}

// New builds a Translator over the given client. model defaults to the fast
// model when empty.
func New(client *llm.Client, model string) *Translator {
	if model == "" {
		model = llm.ModelFast
	}
	return &Translator{client: client, model: model}
}

// Result is a completed translation.
type Result struct {
	Command        string // the translated command (trimmed), or "" if untranslatable
	Untranslatable bool
	Agent          bool   // true when the request should route to mode B (the agent)
	Effect         Effect // side-effect classification of the command
}

// Translate turns nl into one shell command. onDelta (may be nil) receives text
// fragments as they stream, so the daemon can forward partial output. The
// returned Result holds the final, trimmed command.
func (t *Translator) Translate(ctx context.Context, nl string, tc Context, onDelta func(string)) (*Result, error) {
	user := buildUserMessage(nl, tc)
	req := llm.Request{
		Model:     t.model,
		MaxTokens: 512,
		System:    systemPrompt,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock(user)}},
		},
	}

	resp, err := t.client.Stream(ctx, req, func(ev llm.StreamEvent) {
		if ev.Kind == "text" && onDelta != nil {
			onDelta(ev.Text)
		}
	})
	if err != nil {
		return nil, err
	}

	cmd := sanitize(resp.Text())
	if cmd == "" || strings.HasPrefix(cmd, CannotTranslate) {
		return &Result{Untranslatable: true}, nil
	}
	if cmd == AgentSentinel || strings.HasPrefix(cmd, AgentSentinel) {
		return &Result{Agent: true}, nil
	}
	// Defense in depth: despite the system prompt, the model may emit a sentence
	// of explanation or a clarifying question instead of a command. Never write
	// prose into the user's input line — route it to the agent, which can ask or
	// inspect as needed.
	if looksLikeProse(cmd) {
		return &Result{Agent: true}, nil
	}
	return &Result{Command: cmd, Effect: Classify(cmd)}, nil
}

// buildUserMessage assembles the prompt body with context.
func buildUserMessage(nl string, tc Context) string {
	var b strings.Builder
	if tc.CWD != "" {
		b.WriteString("Current directory: ")
		b.WriteString(tc.CWD)
		b.WriteString("\n")
	}
	if len(tc.History) > 0 {
		b.WriteString("Recent commands:\n")
		// cap to the last 10 for prompt economy
		h := tc.History
		if len(h) > 10 {
			h = h[len(h)-10:]
		}
		for _, line := range h {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nRequest: ")
	b.WriteString(nl)
	return b.String()
}

// sanitize strips fences, a leading prompt sigil, surrounding whitespace, and
// collapses to the first non-empty line — defense against the model adding
// formatting despite the system prompt.
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	// strip a ```...``` fence if present
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	// take the first non-empty line
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// keep the cannot-translate sentinel intact
		if strings.HasPrefix(line, CannotTranslate) {
			return CannotTranslate
		}
		line = strings.TrimPrefix(line, "$ ")
		return strings.TrimSpace(line)
	}
	return ""
}

// looksLikeProse reports whether a candidate "command" is actually a sentence of
// natural language (an explanation, apology, or clarifying question) that the
// model emitted in violation of the routing contract. Such text must never be
// written into the user's input line. Heuristics, tuned to avoid flagging real
// commands (which are short, rarely end in '.'/'?'/'!', and don't read as
// sentences):
//   - contains a well-known apology/clarification phrase;
//   - is long AND ends with sentence punctuation;
//   - contains multiple sentences (". " / "? " / "! " followed by a letter).
func looksLikeProse(s string) bool {
	lc := strings.ToLower(s)
	for _, p := range proseMarkers {
		if strings.Contains(lc, p) {
			return true
		}
	}
	// Multiple sentences: a sentence terminator followed by a space and an
	// UPPERCASE letter (a sentence boundary). Commands like `find . -name` have
	// ". " but never ". X" with a capital, so this avoids false positives.
	if hasSentenceBoundary(s) {
		return true
	}
	// A long line that ends like a sentence is prose, not a command.
	if len(s) > 80 {
		switch s[len(s)-1] {
		case '.', '?', '!':
			return true
		}
	}
	return false
}

// hasSentenceBoundary reports whether s contains a ". "/"? "/"! " followed by an
// uppercase letter, or a CJK sentence terminator — a strong sign of prose.
func hasSentenceBoundary(s string) bool {
	rs := []rune(s)
	for i := 0; i+2 < len(rs); i++ {
		if (rs[i] == '.' || rs[i] == '?' || rs[i] == '!') && rs[i+1] == ' ' {
			n := rs[i+2]
			if n >= 'A' && n <= 'Z' {
				return true
			}
		}
	}
	for _, sep := range []string{"。", "？", "！"} {
		if strings.Contains(s, sep) {
			return true
		}
	}
	return false
}

// proseMarkers are phrases that only appear when the model is explaining or
// asking rather than emitting a command.
var proseMarkers = []string{
	"i can't", "i cannot", "i'm not", "i am not", "sorry", "could mean",
	"ambiguous", "please clarify", "do you mean", "i'd need", "i would need",
	"it could", "not sure", "unclear", "无法", "请明确", "你是指", "不清楚", "不确定",
}
