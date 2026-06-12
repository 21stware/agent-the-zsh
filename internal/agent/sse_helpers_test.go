package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newSSEServer starts a test server whose /v1/messages replies with the SSE
// body returned by next() on each POST. Returns the base URL.
func newSSEServer(t *testing.T, next func() string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = http.MaxBytesReader(w, r.Body, 1<<20).Read(make([]byte, 0))
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(next()))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// toolUseSSE builds an SSE body for an assistant turn that calls one tool. The
// argsJSON must be a JSON object with backslash-escaped quotes (it is embedded
// in an input_json_delta string).
func toolUseSSE(id, name, argsJSON string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":5}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"` + id + `","name":"` + name + `"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"` + argsJSON + `"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":10}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

// textEndSSE builds an SSE body for an assistant turn that emits text and ends
// the turn (no tool use).
func textEndSSE(text string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":5}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}
