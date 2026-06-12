package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a self-built Anthropic Messages API client.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the API endpoint (used by tests to point at a fixture
// server).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient overrides the underlying http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// New constructs a Client. apiKey must be non-empty; callers read it from
// ANTHROPIC_API_KEY and must not log it.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		// No overall timeout here: streaming responses are long-lived and the
		// caller controls cancellation via context. A dial/idle-safe transport
		// is the default http.Transport's job.
		httpClient: &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Stream issues a streaming POST /v1/messages. It forwards live deltas to
// onEvent (may be nil) and returns the fully assembled Response. The request is
// cancelable via ctx. The API key is sent in the x-api-key header and never
// logged.
func (c *Client) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (*Response, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("accept", "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}

	asm := newAssembler(onEvent)
	if err := parseSSE(resp.Body, asm.handle); err != nil {
		return nil, err
	}
	out := asm.finalize()
	return &out, nil
}

// APIError is a structured non-2xx error from the API.
type APIError struct {
	Status  int
	Type    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic api %d: %s: %s", e.Status, e.Type, e.Message)
}

// Retryable reports whether the error is worth retrying (429 / 5xx / 529).
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

func parseAPIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var p struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &p)
	return &APIError{
		Status:  resp.StatusCode,
		Type:    p.Error.Type,
		Message: p.Error.Message,
	}
}

// streamTimeout is a sane upper bound a caller can apply via context for a
// single mode-A translation (which should be quick). Exposed for callers.
const StreamTimeout = 30 * time.Second
