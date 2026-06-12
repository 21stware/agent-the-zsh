// Command flow-agent runs flow's mode B: an interactive, hands-on agent in the
// foreground of the user's shell. It has a real TTY, so it can stream the
// agent's narration, run tools in the current directory, and prompt for
// approval of risky operations.
//
// Usage:
//
//	flow-agent "fix the failing tests"
//	flow-agent            # reads the task from stdin
//
// Review level comes from FLOW_REVIEW (strict|focused|yolo, default focused)
// and can be changed mid-task at an approval prompt (y/n/a/s).
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oboo/terflow/internal/agent"
	"github.com/oboo/terflow/internal/config"
	"github.com/oboo/terflow/internal/llm"
)

// ANSI helpers (kept tiny; respect NO_COLOR).
var (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cBold   = "\033[1m"
	cYellow = "\033[33m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cCyan   = "\033[36m"
)

func init() {
	if os.Getenv("NO_COLOR") != "" {
		cReset, cDim, cBold, cYellow, cRed, cGreen, cCyan = "", "", "", "", "", "", ""
	}
}

func main() {
	task := strings.TrimSpace(strings.Join(os.Args[1:], " "))
	if task == "" {
		// Read the task from stdin (e.g. piped from the widget).
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		if sc.Scan() {
			task = strings.TrimSpace(sc.Text())
		}
	}
	if task == "" {
		fmt.Fprintln(os.Stderr, "flow-agent: no task given")
		os.Exit(2)
	}

	cfg := config.Load()
	if !cfg.Enabled() {
		fmt.Fprintln(os.Stderr, "flow-agent: no LLM credential configured (ANTHROPIC_AUTH_TOKEN/API_KEY). See flow-doctor.")
		os.Exit(1)
	}
	opts := []llm.Option{llm.WithBaseURL(cfg.BaseURL)}
	if cfg.AuthToken != "" {
		opts = append(opts, llm.WithAuthToken(cfg.AuthToken))
	}
	client := llm.New(cfg.APIKey, opts...)

	model := cfg.Model // prefer the capable model for agent work
	if model == "" {
		model = cfg.FastModel
	}
	if model == "" {
		// No model configured: discover one from the provider, preferring a
		// capable tier (opus/sonnet) since agent work is correctness-sensitive.
		m, err := pickCapableModel(client)
		if err != nil {
			fmt.Fprintf(os.Stderr, "flow-agent: no model configured and discovery failed (%v); set ANTHROPIC_MODEL.\n", err)
			os.Exit(1)
		}
		model = m
	}

	cwd, _ := os.Getwd()
	level := agent.ParseReviewLevel(os.Getenv("FLOW_REVIEW"))

	// Cancel on Ctrl-C.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigc; fmt.Print("\n" + cDim + "interrupted" + cReset + "\n"); cancel() }()

	r := &runner{level: level}

	fmt.Printf("%s flow agent%s  %sdir %s · review %s%s\n",
		cBold+cCyan, cReset, cDim, cwd, level, cReset)
	fmt.Printf("%s task: %s%s\n\n", cDim, task, cReset)

	loop := agent.New(agent.Config{
		Client: client, Model: model, Cwd: cwd, Level: level,
		Prompt: r.prompt,
		Events: agent.Events{
			OnText:       r.onText,
			OnToolStart:  r.onToolStart,
			OnToolResult: r.onToolResult,
			OnRejected:   r.onRejected,
		},
	})

	final, err := loop.Run(ctx, task)
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%sflow-agent: %v%s\n", cRed, err, cReset)
		os.Exit(1)
	}
	_ = final // already streamed via OnText
}

// runner holds the TTY-facing rendering + approval state.
type runner struct {
	level   agent.ReviewLevel
	midText bool // whether we're mid assistant-text line (for spacing)
}

func (r *runner) onText(s string) {
	fmt.Print(s)
	r.midText = !strings.HasSuffix(s, "\n")
}

func (r *runner) nl() {
	if r.midText {
		fmt.Println()
		r.midText = false
	}
}

func (r *runner) onToolStart(c agent.ToolCall) {
	r.nl()
	fmt.Printf("%s● %s%s\n", cDim, c.Summary, cReset)
}

func (r *runner) onToolResult(c agent.ToolCall, result string, isErr bool) {
	mark, col := "✓", cGreen
	if isErr {
		mark, col = "✗", cRed
	}
	// Show a short preview of the result, indented.
	preview := result
	if len(preview) > 600 {
		preview = preview[:600] + "…"
	}
	fmt.Printf("%s  %s%s %s\n", col, mark, cReset, dimIndent(preview))
}

func (r *runner) onRejected(c agent.ToolCall) {
	r.nl()
	fmt.Printf("%s✗ rejected: %s%s\n", cRed, c.Summary, cReset)
}

// prompt is the permission gate: it renders the pending call and reads a single
// keystroke decision from the TTY. y=allow once, n=reject, a=allow all (yolo),
// s=switch to strict (and reject this one).
func (r *runner) prompt(c agent.ToolCall) agent.Approval {
	r.nl()
	riskCol := cYellow
	if c.Risk == agent.RiskHigh {
		riskCol = cRed
	}
	fmt.Printf("\n%s⚠ approve %s[%s]%s  %s%s%s\n",
		riskCol, "", c.Risk, cReset, cBold, c.Summary, cReset)
	fmt.Printf("%s  [y] run  [n] reject  [a] allow all (this task)  [s] strict mode%s\n", cDim, cReset)
	fmt.Printf("%s  > %s", cDim, cReset)

	ans := readKey()
	fmt.Println(ans)
	switch ans {
	case "y", "Y", "":
		return agent.ApproveOnce
	case "a", "A":
		fmt.Printf("%s  (allowing all further actions this task)%s\n", cDim, cReset)
		return agent.ApproveAll
	case "s", "S":
		fmt.Printf("%s  (switched to strict review)%s\n", cDim, cReset)
		return agent.SwitchStrict
	default:
		return agent.Reject
	}
}

// readKey reads one line of input from the TTY (simplest portable approach;
// single-keypress raw mode is a future refinement). Defaults to "n" on EOF.
func readKey() string {
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return "n"
}

// dimIndent indents multi-line tool output under the result marker.
func dimIndent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = cDim + "    " + ln + cReset
	}
	return strings.Join(lines, "\n")
}

// pickCapableModel discovers a model when none is configured, preferring a
// capable tier (opus > sonnet) for agent work; falls back to any model.
func pickCapableModel(client *llm.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := client.ListModels(ctx)
	if err != nil {
		return "", err
	}
	if len(models) == 0 {
		return "", fmt.Errorf("provider returned no models")
	}
	prefer := []string{"opus", "sonnet", "gpt-5", "gpt-4", "max", "pro"}
	for _, p := range prefer {
		for _, m := range models {
			if strings.Contains(strings.ToLower(m.ID), p) {
				return m.ID, nil
			}
		}
	}
	return models[0].ID, nil
}
