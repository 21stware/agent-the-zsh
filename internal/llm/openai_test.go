package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// openAITextSSE is a realistic OpenAI Chat Completions streaming fixture
// that streams "git status" as two content deltas.
const openAITextSSE = `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"git "},"finish_reason":null}]}

data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"status"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

// TestOpenAIStreamText verifies the OpenAI adapter streams text correctly.
func TestOpenAIStreamText(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = buf
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(openAITextSSE))
	}))
	defer srv.Close()

	c := New("sk-test", WithBaseURL(srv.URL), WithProvider("openai"))
	var streamed strings.Builder
	resp, err := c.Stream(context.Background(), Request{
		Model: "gpt-4o", MaxTokens: 256,
		System:   "You are helpful.",
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("show git status")}}},
	}, func(ev StreamEvent) {
		if ev.Kind == "text" {
			streamed.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := resp.Text(); got != "git status" {
		t.Errorf("text = %q, want %q", got, "git status")
	}
	if streamed.String() != "git status" {
		t.Errorf("streamed = %q", streamed.String())
	}
	if resp.StopReason != StopEndTurn {
		t.Errorf("stop = %q, want end_turn", resp.StopReason)
	}

	// Verify the request was translated to OpenAI format.
	var sent openaiChatRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sent.Model != "gpt-4o" {
		t.Errorf("model = %q", sent.Model)
	}
	if !sent.Stream {
		t.Error("stream not set")
	}
	// First message should be system, second should be user.
	if len(sent.Messages) < 2 {
		t.Fatalf("messages = %d, want >= 2", len(sent.Messages))
	}
	if sent.Messages[0].Role != "system" || sent.Messages[0].Content != "You are helpful." {
		t.Errorf("system msg = %+v", sent.Messages[0])
	}
	if sent.Messages[1].Role != "user" || sent.Messages[1].Content != "show git status" {
		t.Errorf("user msg = %+v", sent.Messages[1])
	}
}

// TestOpenAIStreamHeaders verifies Bearer auth is used for OpenAI.
func TestOpenAIStreamHeaders(t *testing.T) {
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(openAITextSSE))
	}))
	defer srv.Close()

	c := New("sk-test-key", WithBaseURL(srv.URL), WithProvider("openai"))
	_, err := c.Stream(context.Background(), Request{
		Model: "gpt-4o", MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := hdr.Get("authorization"); got != "Bearer sk-test-key" {
		t.Errorf("auth = %q, want Bearer sk-test-key", got)
	}
	if hdr.Get("anthropic-version") != "" {
		t.Errorf("anthropic-version should not be set for openai provider")
	}
}

// TestOpenAIStreamRetry verifies retry works with the OpenAI adapter.
func TestOpenAIStreamRetry(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow"}}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(openAITextSSE))
	}))
	defer srv.Close()

	c := New("sk-test", WithBaseURL(srv.URL), WithProvider("openai"))
	resp, err := c.Stream(context.Background(), Request{
		Model: "gpt-4o", MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Text() != "git status" {
		t.Errorf("text = %q", resp.Text())
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

// TestOpenAIListModels verifies model listing on an OpenAI endpoint.
func TestOpenAIListModels(t *testing.T) {
	body := `{"data":[{"id":"gpt-4o","object":"model"},{"id":"gpt-4o-mini","object":"model"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer sk-test" {
			t.Errorf("auth = %q", r.Header.Get("authorization"))
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New("sk-test", WithBaseURL(srv.URL), WithProvider("openai"))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "gpt-4o" {
		t.Errorf("models = %+v", models)
	}
}

// TestTranslateToOpenAI verifies the Anthropic-to-OpenAI message translation,
// including tool_use and tool_result blocks.
func TestTranslateToOpenAI(t *testing.T) {
	req := Request{
		Model:   "gpt-4o",
		System:  "be helpful",
		MaxTokens: 1024,
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("list files")}},
			{Role: RoleAssistant, Content: []ContentBlock{
				TextBlock("Let me list the files."),
				{Type: "tool_use", ID: "call_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
			}},
			{Role: RoleUser, Content: []ContentBlock{
				ToolResultBlock("call_1", "file1\nfile2", false),
			}},
		},
		Tools: []Tool{
			{Name: "bash", Description: "run a command", InputSchema: map[string]any{"type": "object"}},
		},
	}

	oai := translateToOpenAI(req)

	// system + user + assistant + tool = 4 messages
	if len(oai.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(oai.Messages))
	}
	if oai.Messages[0].Role != "system" {
		t.Errorf("msg[0] role = %q, want system", oai.Messages[0].Role)
	}
	if oai.Messages[1].Role != "user" || oai.Messages[1].Content != "list files" {
		t.Errorf("msg[1] = %+v", oai.Messages[1])
	}
	if oai.Messages[2].Role != "assistant" {
		t.Errorf("msg[2] role = %q", oai.Messages[2].Role)
	}
	if len(oai.Messages[2].ToolCalls) != 1 || oai.Messages[2].ToolCalls[0].ID != "call_1" {
		t.Errorf("msg[2] tool_calls = %+v", oai.Messages[2].ToolCalls)
	}
	if oai.Messages[3].Role != "tool" || oai.Messages[3].ToolCallID != "call_1" {
		t.Errorf("msg[3] = %+v", oai.Messages[3])
	}
	if oai.Messages[3].Content != "file1\nfile2" {
		t.Errorf("msg[3] content = %q", oai.Messages[3].Content)
	}

	// Tools translated.
	if len(oai.Tools) != 1 || oai.Tools[0].Type != "function" || oai.Tools[0].Function.Name != "bash" {
		t.Errorf("tools = %+v", oai.Tools)
	}
}
