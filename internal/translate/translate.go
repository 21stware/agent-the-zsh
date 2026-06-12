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
const systemPrompt = `You route a natural-language request for an interactive zsh session.

Decide between two outcomes and output ONLY one of them, nothing else:

1. If the request can be accomplished with a SINGLE shell command, output ONLY that command.
   - No explanation, no markdown, no code fences, no leading "$".
   - One command line; pipes, &&, ; are fine but keep it one logical line.
   - Do NOT wrap in quotes or add comments.

2. If the request needs MULTIPLE steps, exploration, reading/editing files, or hands-on work that one command cannot do, output EXACTLY this single line and nothing else:
   ## AGENT

Examples:
- "list go files" -> find . -name '*.go'
- "what's using port 8080" -> lsof -i :8080
- "fix all the failing tests" -> ## AGENT
- "refactor this module to use channels" -> ## AGENT
- "add error handling to main.go and run the build" -> ## AGENT

If the request is a pure question that is neither a command nor a task (e.g. "what is SQL"), output exactly: # cannot translate

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
