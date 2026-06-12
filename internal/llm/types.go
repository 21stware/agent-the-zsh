// Package llm is a self-built client for the Anthropic Messages API. It speaks
// the raw HTTP/JSON + SSE wire protocol directly — no official SDK — so flow
// owns every detail of multi-turn conversation, tool use, and streaming.
//
// Design:
//   - Only the NL path ever reaches this package. The command path in the
//     daemon never constructs an llm.Client request (constraint 1: command
//     input never goes to the network).
//   - The full message/content-block/tool model is defined here from the start,
//     even though mode A (single-command translation) uses no tools. Mode B
//     (the agent loop) reuses these types without a protocol change.
//   - The API key is read from the environment by the caller and passed in;
//     it is never hardcoded or logged.
package llm

import "encoding/json"

// Model IDs. Mode A translation is latency-sensitive and simple -> a fast model.
// Mode B agent work is correctness-sensitive -> the most capable model.
const (
	ModelFast    = "claude-haiku-4-5" // mode A: NL -> one shell command
	ModelCapable = "claude-opus-4-8"  // mode B: agent loop
)

// anthropicVersion is the required API version header value.
const anthropicVersion = "2023-06-01"

// defaultBaseURL is the Messages API endpoint root.
const defaultBaseURL = "https://api.anthropic.com"

// Role is a message author role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn in the conversation. Content is a list of blocks so a
// single turn can carry text, tool_use, and tool_result blocks together.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is a single piece of message content. The Anthropic API uses a
// tagged union keyed on "type"; we model it as one struct with a Type tag and
// only-the-relevant fields populated, with custom (un)marshaling to keep the
// wire JSON clean (no empty sibling fields leaking out).
type ContentBlock struct {
	Type string // "text" | "tool_use" | "tool_result"

	// type == "text"
	Text string

	// type == "tool_use" (assistant asks to call a tool)
	ID    string          // tool_use id, echoed back in the matching tool_result
	Name  string          // tool name
	Input json.RawMessage // tool arguments as raw JSON object

	// type == "tool_result" (we return a tool's output)
	ToolUseID string // the tool_use id this result answers
	Content   string // the result payload (text)
	IsError   bool   // true if the tool failed
}

// TextBlock is a convenience constructor for a user/assistant text block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

// ToolResultBlock builds a tool_result block answering a prior tool_use.
func ToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{
		Type:      "tool_result",
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   isError,
	}
}

// MarshalJSON emits the API's tagged-union shape, including only the fields that
// belong to the block's Type.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "text":
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"text", b.Text})
	case "tool_use":
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{"tool_use", b.ID, b.Name, b.Input})
	case "tool_result":
		return json.Marshal(struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
			IsError   bool   `json:"is_error,omitempty"`
		}{"tool_result", b.ToolUseID, b.Content, b.IsError})
	default:
		return nil, &json.UnsupportedValueError{}
	}
}

// UnmarshalJSON parses the API's tagged-union shape back into the flat struct.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	b.Type = probe.Type
	b.Text = probe.Text
	b.ID = probe.ID
	b.Name = probe.Name
	b.Input = probe.Input
	b.ToolUseID = probe.ToolUseID
	b.IsError = probe.IsError
	// tool_result content can be a string or an array of blocks; we only need
	// text here, so accept a bare string and ignore richer shapes.
	if len(probe.Content) > 0 {
		var s string
		if json.Unmarshal(probe.Content, &s) == nil {
			b.Content = s
		}
	}
	return nil
}

// Tool is a tool definition advertised to the model (mode B). InputSchema is a
// JSON Schema object describing the tool's arguments.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// Request is the body of POST /v1/messages.
type Request struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system,omitempty"`
	Messages  []Message      `json:"messages"`
	Tools     []Tool         `json:"tools,omitempty"`
	Stream    bool           `json:"stream,omitempty"`
	Thinking  *ThinkingParam `json:"thinking,omitempty"`
}

// ThinkingParam enables extended/adaptive thinking. For Claude 4.6+ use
// {Type:"adaptive", Display:"summarized"} to receive visible reasoning.
type ThinkingParam struct {
	Type    string `json:"type"`              // "adaptive" | "enabled" | "disabled"
	Display string `json:"display,omitempty"` // "summarized" | "omitted"
}

// AdaptiveThinking returns an adaptive-thinking param with summarized display,
// so the model streams visible reasoning (thinking_delta events).
func AdaptiveThinking() *ThinkingParam {
	return &ThinkingParam{Type: "adaptive", Display: "summarized"}
}

// StopReason mirrors the API's response stop_reason values we care about.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
)

// Response is the assembled result of a (streamed or non-streamed) message.
type Response struct {
	Content    []ContentBlock
	StopReason StopReason
	Model      string
	Usage      Usage
}

// Usage carries token accounting from the final message_delta / message_start.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Text returns the concatenation of all text blocks in the response.
func (r *Response) Text() string {
	var s string
	for _, b := range r.Content {
		if b.Type == "text" {
			s += b.Text
		}
	}
	return s
}

// ToolCalls returns the tool_use blocks in the response, if any.
func (r *Response) ToolCalls() []ContentBlock {
	var out []ContentBlock
	for _, b := range r.Content {
		if b.Type == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}
