package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer returns an httptest server that replies to /v1/messages with the
// given raw SSE body, and records the request it received.
func sseServer(t *testing.T, status int, sseBody string) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var gotReq *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Clone(context.Background())
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = buf
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(sseBody))
	}))
	t.Cleanup(srv.Close)
	return srv, gotReq, &gotBody
}

// A realistic text-streaming fixture: "git status" produced as two text deltas.
const textStreamSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-haiku-4-5","usage":{"input_tokens":42,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"git "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"status"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`

func TestStreamText(t *testing.T) {
	srv, _, gotBody := sseServer(t, 200, textStreamSSE)
	c := New("test-key", WithBaseURL(srv.URL))

	var streamed strings.Builder
	resp, err := c.Stream(context.Background(), Request{
		Model:     ModelFast,
		MaxTokens: 256,
		Messages:  []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("show git status")}}},
	}, func(ev StreamEvent) {
		if ev.Kind == "text" {
			streamed.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := resp.Text(); got != "git status" {
		t.Errorf("assembled text = %q, want %q", got, "git status")
	}
	if got := streamed.String(); got != "git status" {
		t.Errorf("streamed text = %q, want %q", got, "git status")
	}
	if resp.StopReason != StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want in=42 out=3", resp.Usage)
	}
	if resp.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q", resp.Model)
	}

	// Verify the request body had stream:true and the right shape.
	var sent Request
	if err := json.Unmarshal(*gotBody, &sent); err != nil {
		t.Fatalf("unmarshal sent body: %v", err)
	}
	if !sent.Stream {
		t.Error("request did not set stream:true")
	}
	if sent.Model != ModelFast {
		t.Errorf("sent model = %q", sent.Model)
	}
}

func TestStreamHeaders(t *testing.T) {
	srv, _, _ := sseServer(t, 200, textStreamSSE)
	var hdr http.Header
	// wrap to capture headers
	wrap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(textStreamSSE))
	}))
	defer wrap.Close()
	_ = srv

	c := New("secret-key-123", WithBaseURL(wrap.URL))
	if _, err := c.Stream(context.Background(), Request{Model: ModelFast, MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}}}, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if hdr.Get("x-api-key") != "secret-key-123" {
		t.Errorf("x-api-key = %q", hdr.Get("x-api-key"))
	}
	if hdr.Get("anthropic-version") != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", hdr.Get("anthropic-version"), anthropicVersion)
	}
	if hdr.Get("content-type") != "application/json" {
		t.Errorf("content-type = %q", hdr.Get("content-type"))
	}
}

// A tool-use streaming fixture: the model calls bash with {"command":"ls -la"},
// the input arriving as two input_json_delta fragments.
const toolStreamSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_2","model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"bash"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" -la\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`

func TestStreamToolUse(t *testing.T) {
	srv, _, _ := sseServer(t, 200, toolStreamSSE)
	c := New("k", WithBaseURL(srv.URL))

	var partials strings.Builder
	var sawStart bool
	resp, err := c.Stream(context.Background(), Request{
		Model: ModelCapable, MaxTokens: 1024,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("list files")}}},
		Tools: []Tool{{Name: "bash", Description: "run a command",
			InputSchema: map[string]any{"type": "object"}}},
	}, func(ev StreamEvent) {
		switch ev.Kind {
		case "tool_start":
			sawStart = true
			if ev.ToolName != "bash" || ev.ToolID != "toolu_9" {
				t.Errorf("tool_start = %q/%q", ev.ToolName, ev.ToolID)
			}
		case "tool_input":
			partials.WriteString(ev.PartialJSON)
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !sawStart {
		t.Error("did not see tool_start event")
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop reason = %q, want tool_use", resp.StopReason)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(calls[0].Input, &args); err != nil {
		t.Fatalf("unmarshal tool input: %v (raw=%s)", err, calls[0].Input)
	}
	if args.Command != "ls -la" {
		t.Errorf("assembled tool input command = %q, want %q", args.Command, "ls -la")
	}
	if partials.String() != `{"command":"ls -la"}` {
		t.Errorf("streamed partial json = %q", partials.String())
	}
}

func TestStreamAPIError(t *testing.T) {
	body := `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`
	srv, _, _ := sseServer(t, 401, body)
	c := New("bad", WithBaseURL(srv.URL))
	_, err := c.Stream(context.Background(), Request{Model: ModelFast, MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("x")}}}}, nil)
	if err == nil {
		t.Fatal("expected error on 401")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Status != 401 || apiErr.Type != "authentication_error" {
		t.Errorf("APIError = %+v", apiErr)
	}
	if apiErr.Retryable() {
		t.Error("401 should not be retryable")
	}
}

// Mid-stream error events must surface as an error.
func TestStreamMidStreamError(t *testing.T) {
	const midErr = `event: message_start
data: {"type":"message_start","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":1}}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}

`
	srv, _, _ := sseServer(t, 200, midErr)
	c := New("k", WithBaseURL(srv.URL))
	_, err := c.Stream(context.Background(), Request{Model: ModelFast, MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("x")}}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("expected overloaded stream error, got %v", err)
	}
}

// Round-trip the ContentBlock union through JSON to lock the wire shape.
func TestContentBlockMarshalRoundTrip(t *testing.T) {
	cases := []ContentBlock{
		TextBlock("hello"),
		{Type: "tool_use", ID: "toolu_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		ToolResultBlock("toolu_1", "file1\nfile2", false),
		ToolResultBlock("toolu_2", "boom", true),
	}
	for _, in := range cases {
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal %+v: %v", in, err)
		}
		var out ContentBlock
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", data, err)
		}
		if out.Type != in.Type {
			t.Errorf("type round-trip: %q -> %q", in.Type, out.Type)
		}
		switch in.Type {
		case "text":
			if out.Text != in.Text {
				t.Errorf("text: %q -> %q", in.Text, out.Text)
			}
		case "tool_use":
			if out.ID != in.ID || out.Name != in.Name || string(out.Input) != string(in.Input) {
				t.Errorf("tool_use round-trip mismatch: %+v -> %+v", in, out)
			}
		case "tool_result":
			if out.ToolUseID != in.ToolUseID || out.Content != in.Content || out.IsError != in.IsError {
				t.Errorf("tool_result round-trip mismatch: %+v -> %+v", in, out)
			}
		}
	}
}
