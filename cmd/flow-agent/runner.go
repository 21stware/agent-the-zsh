package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/21stware/agent-the-zsh/internal/agent"
	"github.com/21stware/agent-the-zsh/internal/session"
)

// runner renders the agent run on the TTY: an animated spinner while waiting,
// dimmed streaming "thinking", markdown-rendered narration, tool call/result
// lines, and the approval prompt. It buffers assistant text so markdown is
// rendered a line at a time.
type runner struct {
	mu       sync.Mutex
	spinning bool
	stopCh   chan struct{}
	frame    int

	textBuf     strings.Builder // assistant text awaiting a newline to render
	thinkOpen   bool            // currently showing a thinking block
	anyOutput   bool            // produced visible output since last spinner start
	inCodeBlock  bool
	codeLang     string
	codeBuf      strings.Builder
	codePrinted  int      // terminal lines printed while streaming code (for rewind)
	tableLines   []string // buffered table rows awaiting complete render

	ctx       context.Context // set before loop.Run; used to cancel blocking reads
	sessionID string          // FLOW_SESSION_ID, for persisting review-level overrides
}

func newRunner(level agent.ReviewLevel, sessionID string) *runner {
	_ = level
	return &runner{sessionID: sessionID}
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
var spinnerWords = []string{"thinking", "routing", "scaffolding", "working", "reasoning"}

// startSpinner begins the waiting animation on its own line. It is a no-op if
// already running or if NO_COLOR/non-tty (we still animate; it's cheap).
func (r *runner) startSpinner() {
	r.mu.Lock()
	if r.spinning {
		r.mu.Unlock()
		return
	}
	r.spinning = true
	r.stopCh = make(chan struct{})
	stop := r.stopCh
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(90 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.mu.Lock()
				f := spinnerFrames[r.frame%len(spinnerFrames)]
				w := spinnerWords[(r.frame/12)%len(spinnerWords)] // rotate ~1.1s
				r.frame++
				// \r returns to line start; trailing spaces clear leftovers.
				fmt.Printf("\r%s%s %s…%s   ", cCyan, f, w, cReset)
				r.mu.Unlock()
			}
		}
	}()
}

// stopSpinner halts the animation and clears its line.
func (r *runner) stopSpinner() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.spinning {
		return
	}
	close(r.stopCh)
	r.spinning = false
	fmt.Print("\r\033[K") // carriage return + clear to end of line
}

// pauseForOutput stops the spinner (clearing its line) so real output can be
// printed cleanly. Called from the streaming callbacks, which hold no lock.
func (r *runner) pauseForOutput() {
	r.stopSpinner()
}

// onThinking streams the model's reasoning, dimmed, under a "thinking" header.
func (r *runner) onThinking(s string) {
	r.pauseForOutput()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.thinkOpen {
		fmt.Printf("%s🧠 thinking%s\n%s", cDim, cReset, cDim)
		r.thinkOpen = true
	}
	fmt.Print(dimText(s))
	r.anyOutput = true
}

// onText buffers narration and renders complete markdown lines as they arrive.
func (r *runner) onText(s string) {
	r.pauseForOutput()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinkingLocked()
	r.textBuf.WriteString(s)
	r.renderBufferedLinesLocked()
	r.anyOutput = true
}

// flushText renders any trailing buffered text (called at the end of the run).
func (r *runner) flushText() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeThinkingLocked()
	// Render any buffered table that was never flushed.
	if len(r.tableLines) > 0 {
		fmt.Println(renderMarkdown(strings.Join(r.tableLines, "\n")))
		r.tableLines = nil
	}
	// Unclosed code block: rewind yellow lines and highlight.
	if r.inCodeBlock && r.codeBuf.Len() > 0 {
		clearLines(r.codePrinted)
		fmt.Println(highlightCode(r.codeLang, r.codeBuf.String()))
		r.inCodeBlock = false
		r.codeBuf.Reset()
		r.codePrinted = 0
	}
	rest := r.textBuf.String()
	r.textBuf.Reset()
	if strings.TrimSpace(rest) != "" {
		fmt.Println(renderMarkdown(rest))
	}
}

// renderBufferedLinesLocked emits every complete (newline-terminated) line.
// Table rows are buffered and rendered as a block via glamour when the table
// ends. Code blocks stream immediately in yellow, then rewind+highlight on close.
func (r *runner) renderBufferedLinesLocked() {
	s := r.textBuf.String()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		line := s[:i]
		s = s[i+1:]

		if strings.HasPrefix(line, "```") {
			// Flush any buffered table before starting/ending a code block.
			if len(r.tableLines) > 0 {
				fmt.Println(renderMarkdown(strings.Join(r.tableLines, "\n")))
				r.tableLines = nil
			}
			if !r.inCodeBlock {
				r.inCodeBlock = true
				r.codeLang = strings.TrimSpace(strings.TrimPrefix(line, "```"))
				r.codeBuf.Reset()
				r.codePrinted = 0
				if r.codeLang != "" {
					fmt.Println(cDim + "[ " + r.codeLang + " ]" + cReset)
				}
			} else {
				r.inCodeBlock = false
				clearLines(r.codePrinted)
				fmt.Println(highlightCode(r.codeLang, r.codeBuf.String()))
				r.codeBuf.Reset()
				r.codePrinted = 0
			}
			continue
		}

		if r.inCodeBlock {
			r.codeBuf.WriteString(line + "\n")
			fmt.Println(cYellow + line + cReset)
			r.codePrinted++
			continue
		}

		if isTableLine(line) {
			r.tableLines = append(r.tableLines, line)
			continue
		}

		// Leaving table context — flush buffered table via glamour.
		if len(r.tableLines) > 0 {
			fmt.Println(renderMarkdown(strings.Join(r.tableLines, "\n")))
			r.tableLines = nil
		}

		fmt.Println(renderMarkdownLine(line))
	}
	r.textBuf.Reset()
	r.textBuf.WriteString(s)
}

func (r *runner) closeThinkingLocked() {
	if r.thinkOpen {
		fmt.Print(cReset + "\n")
		r.thinkOpen = false
	}
}

func (r *runner) onToolStart(c agent.ToolCall) {
	r.pauseForOutput()
	r.mu.Lock()
	r.closeThinkingLocked()
	r.mu.Unlock()
	fmt.Printf("%s● %s%s\n", cDim, c.Summary, cReset)
}

func (r *runner) onToolResult(c agent.ToolCall, result string, isErr bool) {
	mark, col := "✓", cGreen
	if isErr {
		mark, col = "✗", cRed
	}
	fmt.Printf("%s  %s%s %s\n", col, mark, cReset, dimIndent(previewResult(result, isErr)))
	// Resume the spinner: the next model turn is about to stream.
	r.startSpinner()
}

func (r *runner) onRejected(c agent.ToolCall) {
	r.pauseForOutput()
	fmt.Printf("%s✗ rejected: %s%s\n", cRed, c.Summary, cReset)
	r.startSpinner()
}

// prompt is the permission gate: render the pending call and read a y/n/a/s
// decision from the TTY. A terminal bell (\a) and OSC 777 notification are
// emitted so the user is alerted even when the terminal is not focused.
func (r *runner) prompt(c agent.ToolCall) agent.Approval {
	r.pauseForOutput()
	r.mu.Lock()
	r.closeThinkingLocked()
	r.mu.Unlock()
	riskCol := cYellow
	if c.Risk == agent.RiskHigh {
		riskCol = cRed
	}
	// Terminal notification: BEL + OSC 777 (supported by tmux, screen, etc.).
	fmt.Print("\a")
	fmt.Printf("\033]777;notify;flow-agent;Approval needed: %s\007", c.Summary)
	fmt.Printf("\n%s⚠ approve [%s]%s  %s%s%s\n",
		riskCol, c.Risk, cReset, cBold, c.Summary, cReset)
	fmt.Printf("%s  [y] run  [n] reject  [a] allow all (this task)  [s] strict mode%s\n", cDim, cReset)
	fmt.Printf("%s  > %s", cDim, cReset)

	ans := r.readKey()
	switch ans {
	case "y", "Y", "":
		r.startSpinner()
		return agent.ApproveOnce
	case "a", "A":
		fmt.Printf("%s  (allowing all further actions this task)%s\n", cDim, cReset)
		_ = session.SaveLevel(r.sessionID, "yolo")
		r.startSpinner()
		return agent.ApproveAll
	case "s", "S":
		fmt.Printf("%s  (switched to strict review)%s\n", cDim, cReset)
		r.startSpinner()
		return agent.SwitchStrict
	default:
		r.startSpinner()
		return agent.Reject
	}
}

// readKey reads one line from the controlling terminal (/dev/tty), falling
// back to stdin when /dev/tty is unavailable. It is context-aware: if the
// run is cancelled (e.g. Ctrl-C) while waiting for input, it returns "n"
// immediately instead of hanging forever.
//
// When the agent is launched from the zsh widget (zle), the terminal is
// typically in raw mode (no echo, no line buffering, Enter sends \r not \n).
// We save the terminal state, switch to cooked mode (echo + icanon) so the
// user can see what they type and Enter submits the line, then restore the
// original state after reading.
func (r *runner) readKey() string {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		f = os.Stdin // fallback (e.g. piped/non-interactive)
	} else {
		defer f.Close()
	}

	// Save terminal state and switch to cooked mode for interactive input.
	// sttyState/stty are defined in picker.go and use the stty command.
	var saved string
	if f != os.Stdin {
		if s, err := sttyState(f); err == nil {
			saved = s
			_ = stty(f, "echo", "icanon")
		}
	}
	defer func() {
		if saved != "" {
			_ = stty(f, saved)
		}
	}()

	ch := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(f)
		if sc.Scan() {
			ch <- strings.TrimSpace(sc.Text())
		} else {
			ch <- "n"
		}
	}()

	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case ans := <-ch:
		return ans
	case <-ctx.Done():
		return "n"
	}
}

// dimText wraps a string in the dim color (no reset, so a thinking block stays
// dim across deltas; the block is closed explicitly).
func dimText(s string) string { return s }

// Preview budgets (in bytes) for tool output shown to the user. The model
// always receives the full output; these only bound what we print to the TTY.
const (
	previewMax = 600 // success: show the head only
	errHeadMax = 400 // error: bytes kept from the start
	errTailMax = 600 // error: bytes kept from the end (where the real error lives)
)

// previewResult bounds tool output for display. Success output is truncated
// head-only (the interesting part is usually first). Error output keeps BOTH a
// head and a tail, because tool failures put the actual diagnostic — a stack
// trace, a compiler/test message, the "[command failed (exit N)]" marker — at
// the END. A plain head-only cut would swallow exactly the line the user needs.
// All cuts land on UTF-8 rune boundaries so multibyte text (e.g. Chinese error
// messages) is never corrupted at the seam.
func previewResult(s string, isErr bool) string {
	if !isErr {
		if len(s) <= previewMax {
			return s
		}
		return headBytes(s, previewMax) + "…"
	}
	if len(s) <= errHeadMax+errTailMax {
		return s
	}
	head := headBytes(s, errHeadMax)
	tail := tailBytes(s, errTailMax)
	return head + "\n…\n" + tail
}

// headBytes returns the longest prefix of s that fits in n bytes without
// splitting a UTF-8 rune.
func headBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// tailBytes returns the longest suffix of s that fits in n bytes without
// splitting a UTF-8 rune.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// dimIndent indents multi-line tool output under the result marker.
func dimIndent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = cDim + "    " + ln + cReset
	}
	return strings.Join(lines, "\n")
}
