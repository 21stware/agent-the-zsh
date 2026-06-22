package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openaiChatRequest is the OpenAI Chat Completions request body.
type openaiChatRequest struct {
	Model     string          `json:"model"`
	Messages  []openaiMessage `json:"messages"`
	Tools     []openaiTool    `json:"tools,omitempty"`
	Stream    bool            `json:"stream"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

// openaiMessage is one message in the OpenAI Chat format.
type openaiMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openaiTool defines a tool the model can call.
type openaiTool struct {
	Type     string             `json:"type"` // always "function"
	Function openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// openaiToolCall is a tool call requested by the assistant.
type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"` // always "function"
	Function openaiToolFunction `json:"function"`
}

// streamOpenAI issues a streaming POST /v1/chat/completions using the OpenAI
// Chat Completions wire protocol, translating between the internal Anthropic-
// style types and the OpenAI format. Includes retry on retryable errors.
func (c *Client) streamOpenAI(ctx context.Context, req Request, onEvent func(StreamEvent)) (*Response, error) {
	oaiReq := translateToOpenAI(req)
	oaiReq.Stream = true
	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		resp, committed, err := c.doStreamOpenAI(ctx, body, onEvent)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if committed {
			return nil, err
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() {
			return nil, err
		}
	}
	return nil, lastErr
}

// doStreamOpenAI performs a single OpenAI streaming attempt.
func (c *Client) doStreamOpenAI(ctx context.Context, body []byte, onEvent func(StreamEvent)) (resp *Response, committed bool, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	// OpenAI always uses Bearer auth.
	token := c.authToken
	if token == "" {
		token = c.apiKey
	}
	httpReq.Header.Set("authorization", "Bearer "+token)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, false, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		return nil, false, parseAPIError(httpResp)
	}

	asm := newAssembler(onEvent)
	committed = false
	wrappedHandle := func(ev sseEvent) error {
		committed = true
		return handleOpenAISSE(ev, asm)
	}
	if err := parseSSE(httpResp.Body, wrappedHandle); err != nil {
		return nil, committed, err
	}
	out := asm.finalize()
	return &out, committed, nil
}

// handleOpenAISSE translates an OpenAI SSE event into the internal assembler's
// expected format. OpenAI sends "data: {json}" chunks where each chunk has
// choices[0].delta with content and/or tool_calls.
func handleOpenAISSE(ev sseEvent, asm *streamAssembler) error {
	// OpenAI uses "data: [DONE]" to signal end of stream.
	if ev.event == "" && string(ev.data) == "[DONE]" {
		return nil
	}

	var chunk struct {
		Choices []struct {
			Delta struct {
				Role      string           `json:"role"`
				Content   string           `json:"content"`
				ToolCalls []openaiToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(ev.data, &chunk); err != nil {
		return nil // skip unparseable chunks
	}

	if len(chunk.Choices) == 0 {
		return nil
	}
	choice := chunk.Choices[0]

	// Initialize the assembler with a message_start on the first chunk.
	if !asm.started {
		asm.handle(sseEvent{
			event: "message_start",
			data: mustMarshal(map[string]any{
				"message": map[string]any{
					"model": chunk.Model,
					"usage": map[string]any{"input_tokens": 0},
				},
			}),
		})
		asm.started = true
	}

	// Text content delta.
	if choice.Delta.Content != "" {
		// Ensure a text block exists at index 0.
		if !asm.textBlockOpen {
			asm.handle(sseEvent{
				event: "content_block_start",
				data: mustMarshal(map[string]any{
					"index":          0,
					"content_block": map[string]any{"type": "text", "text": ""},
				}),
			})
			asm.textBlockOpen = true
		}
		asm.handle(sseEvent{
			event: "content_block_delta",
			data: mustMarshal(map[string]any{
				"index":  0,
				"delta": map[string]any{"type": "text_delta", "text": choice.Delta.Content},
			}),
		})
	}

	// Tool calls.
	for _, tc := range choice.Delta.ToolCalls {
		idx := asm.nextToolIndex
		// If no text block was opened, tools start at index 0.
		if !asm.textBlockOpen && idx == 1 {
			idx = 0
			asm.nextToolIndex = 0
		}
		asm.handle(sseEvent{
			event: "content_block_start",
			data: mustMarshal(map[string]any{
				"index": idx,
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   tc.ID,
					"name": tc.Function.Name,
				},
			}),
		})
		// Feed the tool arguments as input_json_delta.
		if tc.Function.Parameters != nil || tc.Function.Name != "" {
			inputJSON := mustMarshal(tc.Function.Parameters)
			asm.handle(sseEvent{
				event: "content_block_delta",
				data: mustMarshal(map[string]any{
					"index":  idx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": string(inputJSON)},
				}),
			})
		}
		asm.nextToolIndex++
	}

	// Finish reason maps to stop reason.
	if choice.FinishReason != "" {
		// Close any open text block.
		if asm.textBlockOpen {
			asm.handle(sseEvent{
				event: "content_block_stop",
				data:  mustMarshal(map[string]any{"index": 0}),
			})
		}
		switch choice.FinishReason {
		case "stop":
			asm.handle(sseEvent{
				event: "message_delta",
				data:  mustMarshal(map[string]any{"delta": map[string]any{"stop_reason": "end_turn"}}),
			})
		case "tool_calls":
			asm.handle(sseEvent{
				event: "message_delta",
				data:  mustMarshal(map[string]any{"delta": map[string]any{"stop_reason": "tool_use"}}),
			})
		case "length":
			asm.handle(sseEvent{
				event: "message_delta",
				data:  mustMarshal(map[string]any{"delta": map[string]any{"stop_reason": "max_tokens"}}),
			})
		}
	}

	return nil
}

// mustMarshal is a helper that panics on marshal failure (only used with
// static maps where failure is impossible).
func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// translateToOpenAI converts an internal (Anthropic-style) Request to an
// OpenAI Chat Completions request.
func translateToOpenAI(req Request) openaiChatRequest {
	var msgs []openaiMessage

	// System prompt becomes a system message.
	if req.System != "" {
		msgs = append(msgs, openaiMessage{Role: "system", Content: req.System})
	}

	for _, m := range req.Messages {
		oaiMsg := openaiMessage{Role: string(m.Role)}

		// Collect text blocks as content, tool_use as tool_calls, tool_result
		// as separate messages with role "tool".
		var textParts []string
		var toolCalls []openaiToolCall
		for _, blk := range m.Content {
			switch blk.Type {
			case "text":
				textParts = append(textParts, blk.Text)
			case "tool_use":
				var input map[string]any
				if len(blk.Input) > 0 {
					_ = json.Unmarshal(blk.Input, &input)
				}
				toolCalls = append(toolCalls, openaiToolCall{
					ID:   blk.ID,
					Type: "function",
					Function: openaiToolFunction{
						Name:        blk.Name,
						Description: "",
						Parameters:  input,
					},
				})
			case "tool_result":
				// Tool results become separate "tool" role messages.
				msgs = append(msgs, openaiMessage{
					Role:       "tool",
					Content:    blk.Content,
					ToolCallID: blk.ToolUseID,
				})
			}
		}

		if len(toolCalls) > 0 {
			oaiMsg.ToolCalls = toolCalls
		}
		oaiMsg.Content = strings.Join(textParts, "\n")
		if oaiMsg.Content != "" || len(toolCalls) > 0 || oaiMsg.Role == "tool" {
			// Skip empty messages that aren't tool results.
			if oaiMsg.Role != "tool" {
				msgs = append(msgs, oaiMsg)
			}
		}
	}

	// Translate tools.
	var tools []openaiTool
	for _, t := range req.Tools {
		tools = append(tools, openaiTool{
			Type: "function",
			Function: openaiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	return openaiChatRequest{
		Model:     req.Model,
		Messages:  msgs,
		Tools:     tools,
		MaxTokens: req.MaxTokens,
	}
}

// listModelsOpenAI queries GET /v1/models on an OpenAI-compatible endpoint.
func (c *Client) listModelsOpenAI(ctx context.Context) ([]Model, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	token := c.authToken
	if token == "" {
		token = c.apiKey
	}
	httpReq.Header.Set("authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}
	var doc struct {
		Data []Model `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&doc); err != nil {
		return nil, err
	}
	return doc.Data, nil
}
