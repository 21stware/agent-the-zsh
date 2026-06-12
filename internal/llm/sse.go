package llm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// StreamEvent is a decoded SSE event surfaced to callers that want live deltas
// (e.g. mode A streaming a command back to the buffer as it is generated).
type StreamEvent struct {
	// Kind is one of: "text", "tool_input", "tool_start", "done", "error".
	Kind string
	// Text is the incremental text delta (Kind == "text").
	Text string
	// PartialJSON is the incremental tool-input JSON fragment (Kind=="tool_input").
	PartialJSON string
	// ToolName / ToolID are set on "tool_start".
	ToolName string
	ToolID   string
	// Err is set on "error".
	Err error
}

// sseEvent is one raw Server-Sent Event: an event type plus a JSON data line.
type sseEvent struct {
	event string
	data  []byte
}

// parseSSE reads an SSE stream and calls fn for each complete event. It returns
// when the stream ends or fn returns a non-nil error. SSE framing: lines
// "event: X" and "data: Y", events separated by a blank line. The Anthropic
// stream also sends ": ping" comment lines and periodic "ping" events.
func parseSSE(r io.Reader, fn func(sseEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			// blank line: dispatch the accumulated event, if any.
			if cur.event != "" || len(cur.data) > 0 {
				if err := fn(cur); err != nil {
					return err
				}
				cur = sseEvent{}
			}
		case strings.HasPrefix(line, ":"):
			// comment / heartbeat, ignore.
		case strings.HasPrefix(line, "event:"):
			cur.event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			cur.data = append(cur.data, strings.TrimSpace(line[len("data:"):])...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// Flush a trailing event with no terminating blank line.
	if cur.event != "" || len(cur.data) > 0 {
		return fn(cur)
	}
	return nil
}

// streamAssembler consumes Anthropic Messages SSE events, assembles the full
// Response, and forwards live StreamEvents to onEvent (may be nil). It tracks
// per-index content blocks because text and tool_use deltas are addressed by a
// content_block index.
type streamAssembler struct {
	resp    Response
	blocks  map[int]*blockBuilder
	onEvent func(StreamEvent)
}

type blockBuilder struct {
	typ       string
	text      strings.Builder
	toolID    string
	toolName  string
	inputJSON strings.Builder // accumulated input_json_delta fragments
}

func newAssembler(onEvent func(StreamEvent)) *streamAssembler {
	return &streamAssembler{
		blocks:  map[int]*blockBuilder{},
		onEvent: onEvent,
	}
}

func (a *streamAssembler) emit(ev StreamEvent) {
	if a.onEvent != nil {
		a.onEvent(ev)
	}
}

// handle processes one decoded SSE event, mutating the in-progress Response.
func (a *streamAssembler) handle(ev sseEvent) error {
	switch ev.event {
	case "message_start":
		var p struct {
			Message struct {
				Model string `json:"model"`
				Usage Usage  `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(ev.data, &p); err != nil {
			return fmt.Errorf("message_start: %w", err)
		}
		a.resp.Model = p.Message.Model
		a.resp.Usage.InputTokens = p.Message.Usage.InputTokens

	case "content_block_start":
		var p struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(ev.data, &p); err != nil {
			return fmt.Errorf("content_block_start: %w", err)
		}
		bb := &blockBuilder{typ: p.ContentBlock.Type}
		if p.ContentBlock.Type == "text" {
			bb.text.WriteString(p.ContentBlock.Text)
		} else if p.ContentBlock.Type == "tool_use" {
			bb.toolID = p.ContentBlock.ID
			bb.toolName = p.ContentBlock.Name
			a.emit(StreamEvent{Kind: "tool_start", ToolID: bb.toolID, ToolName: bb.toolName})
		}
		a.blocks[p.Index] = bb

	case "content_block_delta":
		var p struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(ev.data, &p); err != nil {
			return fmt.Errorf("content_block_delta: %w", err)
		}
		bb := a.blocks[p.Index]
		if bb == nil {
			return nil // delta for an unknown block; ignore defensively
		}
		switch p.Delta.Type {
		case "text_delta":
			bb.text.WriteString(p.Delta.Text)
			a.emit(StreamEvent{Kind: "text", Text: p.Delta.Text})
		case "input_json_delta":
			bb.inputJSON.WriteString(p.Delta.PartialJSON)
			a.emit(StreamEvent{Kind: "tool_input", PartialJSON: p.Delta.PartialJSON})
		case "thinking_delta":
			a.emit(StreamEvent{Kind: "thinking", Text: p.Delta.Thinking})
		}

	case "content_block_stop":
		// Block complete; nothing to do — finalize() reads accumulated state.

	case "message_delta":
		var p struct {
			Delta struct {
				StopReason StopReason `json:"stop_reason"`
			} `json:"delta"`
			Usage Usage `json:"usage"`
		}
		if err := json.Unmarshal(ev.data, &p); err != nil {
			return fmt.Errorf("message_delta: %w", err)
		}
		if p.Delta.StopReason != "" {
			a.resp.StopReason = p.Delta.StopReason
		}
		if p.Usage.OutputTokens > 0 {
			a.resp.Usage.OutputTokens = p.Usage.OutputTokens
		}

	case "message_stop":
		// Stream finished.

	case "error":
		var p struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(ev.data, &p)
		err := fmt.Errorf("api stream error: %s: %s", p.Error.Type, p.Error.Message)
		a.emit(StreamEvent{Kind: "error", Err: err})
		return err
	}
	return nil
}

// finalize assembles the ordered content blocks into the Response.
func (a *streamAssembler) finalize() Response {
	// Blocks are keyed by index; emit in index order.
	maxIdx := -1
	for i := range a.blocks {
		if i > maxIdx {
			maxIdx = i
		}
	}
	for i := 0; i <= maxIdx; i++ {
		bb := a.blocks[i]
		if bb == nil {
			continue
		}
		switch bb.typ {
		case "text":
			a.resp.Content = append(a.resp.Content, ContentBlock{
				Type: "text", Text: bb.text.String(),
			})
		case "tool_use":
			input := bb.inputJSON.String()
			if input == "" {
				input = "{}"
			}
			a.resp.Content = append(a.resp.Content, ContentBlock{
				Type:  "tool_use",
				ID:    bb.toolID,
				Name:  bb.toolName,
				Input: json.RawMessage(input),
			})
		}
	}
	return a.resp
}
