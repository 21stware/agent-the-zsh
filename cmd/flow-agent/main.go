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

	"github.com/21stware/agent-the-zsh/internal/agent"
	"github.com/21stware/agent-the-zsh/internal/config"
	"github.com/21stware/agent-the-zsh/internal/llm"
	"github.com/21stware/agent-the-zsh/internal/session"
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

// isTTY is true when stdout is an interactive terminal (cursor control is safe).
var isTTY = func() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}()

// clearLines moves the cursor up n lines, erasing each one.
// Used to redraw live-streaming table/code blocks in place.
func clearLines(n int) {
	if !isTTY || n <= 0 {
		return
	}
	for i := 0; i < n; i++ {
		fmt.Print("\033[1A\033[2K")
	}
}

func init() {
	if os.Getenv("NO_COLOR") != "" {
		cReset, cDim, cBold, cYellow, cRed, cGreen, cCyan = "", "", "", "", "", "", ""
	}
}

func main() {
	// Subcommand: render the interactive resume picker and print the chosen
	// session id to stdout (consumed by the `flowrsm` shell function). All UI is
	// drawn on stderr/the TTY so stdout carries only the result.
	if len(os.Args) > 1 && os.Args[1] == "--resume-picker" {
		runResumePicker()
		return
	}

	task := strings.TrimSpace(strings.Join(os.Args[1:], " "))
	if task == "" {
		// The widget passes the task via FLOW_TASK so the command line doesn't
		// echo a long quoted argument. Consume and clear it.
		task = strings.TrimSpace(os.Getenv("FLOW_TASK"))
		os.Unsetenv("FLOW_TASK")
	}
	if task == "" {
		// Fall back to stdin (e.g. piped).
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

	// Per-shell conversation transcript. FLOW_SESSION_ID keys the file (one shell
	// window = one continuous conversation), so prior turns in THIS session are
	// loaded by default. FLOW_FRESH=1 forces a one-off fresh turn; `flowclear`
	// truncates the transcript to start the session's conversation over.
	sessionFile, _ := session.Path(os.Getenv("FLOW_SESSION_ID"))
	resume := os.Getenv("FLOW_FRESH") != "1"
	// Cancel on Ctrl-C.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigc; fmt.Print("\n" + cDim + "interrupted" + cReset + "\n"); cancel() }()

	r := newRunner(level)

	fmt.Printf("%s flow %s  %sdir %s · review %s%s\n\n",
		cBold+cCyan, cReset, cDim, cwd, level, cReset)

	loop := agent.New(agent.Config{
		Client: client, Model: model, Cwd: cwd, Level: level,
		SessionFile: sessionFile, SessionID: os.Getenv("FLOW_SESSION_ID"), Resume: resume,
		Prompt: r.prompt,
		Events: agent.Events{
			OnText:       r.onText,
			OnThinking:   r.onThinking,
			OnToolStart:  r.onToolStart,
			OnToolResult: r.onToolResult,
			OnRejected:   r.onRejected,
		},
	})

	r.startSpinner() // animate while the first model turn streams
	final, err := loop.Run(ctx, task)
	r.stopSpinner()
	r.flushText()
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%sflow-agent: %v%s\n", cRed, err, cReset)
		os.Exit(1)
	}
	_ = final // already streamed via OnText
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
