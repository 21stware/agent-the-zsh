package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oboo/terflow/internal/llm"
)

// systemPrompt steers mode B: a hands-on agent that completes a task using the
// tools, then stops. It is told the working directory and to be concise.
const systemPromptTmpl = `You are flow's agent: a hands-on coding/shell assistant operating inside the user's terminal, in the working directory %s.

You complete the user's task by using the provided tools (bash, read_file, write_file, edit, grep). Guidelines:
- Take real actions with tools; don't just describe what to do.
- Work in small, verifiable steps. Read before you edit. After changes, verify (build/test/inspect) when relevant.
- Prefer the dedicated file tools (read_file/write_file/edit/grep) over bash for file work.
- Some tool calls require user approval; if one is rejected, adapt — do not retry it verbatim.
- Be concise in your narration. When the task is done, give a one or two sentence summary and stop.
- If the task is actually just a question, answer it directly without tools.`

// PromptFunc is the permission gate. The loop calls it before every tool call
// the policy flags as DecideAsk. It returns the user's decision. The host
// (CLI) implements this against the TTY; tests supply a stub. The level pointer
// lets the host mutate the active review level mid-task (e.g. hotkey to yolo).
type PromptFunc func(call ToolCall) Approval

// Approval is the outcome of a permission prompt.
type Approval int

const (
	ApproveOnce  Approval = iota // run this one call
	Reject                       // skip this call; tell the model it was denied
	ApproveAll                   // run this and stop asking (switch to yolo)
	SwitchStrict                 // run nothing now; reject and raise level to strict
)

// ToolCall describes a pending tool invocation passed to the gate and emitter.
type ToolCall struct {
	Name    string
	Args    json.RawMessage
	Risk    Risk
	Summary string
}

// Events let the host render the run. All are optional.
type Events struct {
	// OnText streams assistant narration text deltas.
	OnText func(string)
	// OnToolStart fires when a tool is about to run (after approval).
	OnToolStart func(call ToolCall)
	// OnToolResult fires after a tool runs, with its (possibly truncated) output.
	OnToolResult func(call ToolCall, result string, isErr bool)
	// OnRejected fires when a call was rejected by the gate.
	OnRejected func(call ToolCall)
}

// Loop runs the mode-B agent.
type Loop struct {
	client *llm.Client
	model  string
	cwd    string
	tools  map[string]*Tool

	level  ReviewLevel
	prompt PromptFunc
	events Events

	maxTurns int
}

// Config configures a Loop.
type Config struct {
	Client   *llm.Client
	Model    string // capable model id
	Cwd      string
	Level    ReviewLevel
	Prompt   PromptFunc // permission gate (required when any side effect may occur)
	Events   Events
	MaxTurns int // safety cap on tool-use iterations (default 40)
}

// New builds a Loop.
func New(cfg Config) *Loop {
	max := cfg.MaxTurns
	if max <= 0 {
		max = 40
	}
	return &Loop{
		client:   cfg.Client,
		model:    cfg.Model,
		cwd:      cfg.Cwd,
		tools:    DefaultTools(),
		level:    cfg.Level,
		prompt:   cfg.Prompt,
		events:   cfg.Events,
		maxTurns: max,
	}
}

// Run executes the task to completion (end_turn), the max-turn cap, or ctx
// cancellation. It returns the final assistant text and any fatal error.
func (l *Loop) Run(ctx context.Context, task string) (string, error) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock(task)}},
	}
	system := fmt.Sprintf(systemPromptTmpl, l.cwd)
	defs := Defs(l.tools)

	var lastText string
	for turn := 0; turn < l.maxTurns; turn++ {
		req := llm.Request{
			Model:     l.model,
			MaxTokens: 8192,
			System:    system,
			Messages:  msgs,
			Tools:     defs,
		}
		resp, err := l.client.Stream(ctx, req, func(ev llm.StreamEvent) {
			if ev.Kind == "text" && l.events.OnText != nil {
				l.events.OnText(ev.Text)
			}
		})
		if err != nil {
			return lastText, err
		}
		if t := resp.Text(); t != "" {
			lastText = t
		}

		// Append the assistant turn (with any tool_use blocks) to history.
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: resp.Content})

		calls := resp.ToolCalls()
		if len(calls) == 0 || resp.StopReason != llm.StopToolUse {
			// No tools requested -> the agent is done (end_turn) or stopped.
			return lastText, nil
		}

		// Execute each requested tool, gated by permission, collecting results
		// into a single user turn.
		var results []llm.ContentBlock
		for _, c := range calls {
			tool := l.tools[c.Name]
			if tool == nil {
				results = append(results, llm.ToolResultBlock(c.ID,
					"unknown tool: "+c.Name, true))
				continue
			}
			risk := tool.Risk(c.Input)
			call := ToolCall{Name: c.Name, Args: c.Input, Risk: risk, Summary: tool.Summary(c.Input)}

			if Decide(l.level, risk) == DecideAsk {
				switch l.askApproval(call) {
				case Reject:
					if l.events.OnRejected != nil {
						l.events.OnRejected(call)
					}
					results = append(results, llm.ToolResultBlock(c.ID,
						"user rejected this action; do not retry it. Consider an alternative or ask the user.", true))
					continue
				case SwitchStrict:
					l.level = ReviewStrict
					if l.events.OnRejected != nil {
						l.events.OnRejected(call)
					}
					results = append(results, llm.ToolResultBlock(c.ID,
						"user rejected this action and raised the review level. Reconsider before acting.", true))
					continue
				case ApproveAll:
					l.level = ReviewYolo // stop asking for the rest of the task
				case ApproveOnce:
					// proceed
				}
			}

			if l.events.OnToolStart != nil {
				l.events.OnToolStart(call)
			}
			out, isErr := tool.Run(ctx, l.cwd, c.Input)
			if l.events.OnToolResult != nil {
				l.events.OnToolResult(call, out, isErr)
			}
			results = append(results, llm.ToolResultBlock(c.ID, out, isErr))
		}

		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: results})
	}
	return lastText, fmt.Errorf("reached max turns (%d) without completing", l.maxTurns)
}

// askApproval consults the gate; with no gate configured, any ask becomes a
// rejection (fail safe — never run an unapproved side effect silently).
func (l *Loop) askApproval(call ToolCall) Approval {
	if l.prompt == nil {
		return Reject
	}
	return l.prompt(call)
}
