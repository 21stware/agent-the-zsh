// Package protocol defines the wire format between the flow zsh widget (thin
// client) and the flowd daemon. The transport is a unix domain socket; the
// encoding is one JSON object per line ("json line" / JSONL), request and
// response.
//
// Design notes tied to the project constraints:
//   - The request never leaves the machine. The daemon must not put user input
//     on the network for the CMD path (constraint 1 & "command path never goes
//     to network").
//   - The response is intentionally small and synchronous for step 2. In step 3
//     the daemon will stream translation chunks; that will be a separate
//     response variant, not a change to this request shape.
package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// Action is what the widget should do with the current input line.
type Action string

const (
	// ActionAccept: run the buffer as-is (accept-line). This is the zero-latency
	// command path and, in step 2, also the fallback for NL (translation lands
	// in step 3).
	ActionAccept Action = "accept"

	// ActionReplace: replace the buffer with Text and stop at end-of-line for the
	// user to confirm. Reserved for step 3 (mode A translation). Defined here so
	// the widget can be written against the final protocol now.
	ActionReplace Action = "replace"

	// ActionPending is the first line of a two-phase NL reply: the daemon has
	// accepted the request and is translating. The widget shows a progress
	// indicator and reads a second line (the real accept/replace) with a longer
	// timeout. This keeps the command path bounded by the short first-phase
	// timeout while letting NL translation take seconds.
	ActionPending Action = "pending"

	// ActionAgent routes the request to mode B (the agent). The widget hands the
	// original NL task off to the foreground flow-agent, which takes over the
	// terminal. Text carries the task to run.
	ActionAgent Action = "agent"
)

// Verdict is the classifier's decision, surfaced for logging/telemetry and for
// the widget to optionally annotate. It does not by itself dictate Action.
type Verdict string

const (
	VerdictCMD Verdict = "CMD"
	VerdictNL  Verdict = "NL"
)

// Request is sent by the widget on each accept (Enter).
type Request struct {
	// Buffer is the raw current input line (ZLE $BUFFER).
	Buffer string `json:"buffer"`
	// Cwd is the widget's current working directory, for context and for binding
	// any later command execution to the right directory.
	Cwd string `json:"cwd"`
	// History is the most recent shell history lines (oldest first), bounded by
	// the client. Used as context by the classifier/translator. Never required.
	History []string `json:"history,omitempty"`
	// Proto lets the daemon reject mismatched clients. Bumped on breaking changes.
	Proto int `json:"proto"`
	// Clear, when true, resets the daemon's NL conversation session and the
	// daemon replies immediately without classifying. Triggered by `flowclear`.
	Clear bool `json:"clear,omitempty"`
}

// Response is returned by the daemon for a Request.
type Response struct {
	// Action tells the widget what to do. In step 2 this is always "accept".
	Action Action `json:"action"`
	// Verdict is the classification result (CMD/NL) for logging/annotation.
	Verdict Verdict `json:"verdict"`
	// Reason is the human-readable classifier reason, for debugging.
	Reason string `json:"reason,omitempty"`
	// Text is the replacement buffer when Action == "replace" (step 3).
	Text string `json:"text,omitempty"`
	// Effect classifies a translated command's blast radius: "read-only" or
	// "side-effect". Set when Action == "replace". The widget uses it in step 4
	// to decide whether to require an explicit confirmation keystroke.
	Effect string `json:"effect,omitempty"`
	// Err carries a daemon-side error string. The widget treats any error as a
	// reason to fall back to plain accept-line (never brick the terminal).
	Err string `json:"err,omitempty"`
}

// Effect values for Response.Effect.
const (
	EffectReadOnly   = "read-only"
	EffectSideEffect = "side-effect"
)

// CurrentProto is the protocol version this build speaks.
const CurrentProto = 1

// WriteJSONLine encodes v as a single JSON object followed by '\n'. HTML
// escaping is disabled so shell metacharacters in command text (`<`, `>`, `&`)
// are emitted literally instead of as </>/& — the zsh widget's
// JSON reader doesn't decode \uXXXX, and a translated command must round-trip
// verbatim.
func WriteJSONLine(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // Encode appends a trailing '\n'
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// ReadRequest reads and decodes one JSONL request from r.
func ReadRequest(r *bufio.Reader) (*Request, error) {
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	var req Request
	if e := json.Unmarshal(trimNewline(line), &req); e != nil {
		return nil, e
	}
	return &req, nil
}

// ReadResponse reads and decodes one JSONL response from r.
func ReadResponse(r *bufio.Reader) (*Response, error) {
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	var resp Response
	if e := json.Unmarshal(trimNewline(line), &resp); e != nil {
		return nil, e
	}
	return &resp, nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
