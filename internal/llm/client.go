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

// Client is a self-built LLM API client. It speaks the Anthropic Messages
// wire protocol natively, and can also target OpenAI-compatible Chat
// Completions endpoints (OpenAI, DeepSeek, GLM, etc.) via the OpenAI adapter.
type Client struct {
	apiKey     string // x-api-key auth (first-party)
	authToken  string // Authorization: Bearer auth (compatible proxies)
	baseURL    string
	provider   string // "anthropic" (default) or "openai"
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the API endpoint (used by tests to point at a fixture
// server, and to target compatible proxies).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithAuthToken sets Authorization: Bearer auth, used by Anthropic-compatible
// proxies. When set, it takes precedence over x-api-key.
func WithAuthToken(t string) Option { return func(c *Client) { c.authToken = t } }

// WithHTTPClient overrides the underlying http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithProvider sets the API protocol: "anthropic" (default) or "openai".
// When "openai", Stream and ListModels use the OpenAI Chat Completions wire
// format instead of the Anthropic Messages format.
func WithProvider(p string) Option { return func(c *Client) { c.provider = p } }

// New constructs a Client. apiKey may be empty when WithAuthToken is used. The
// credential is read from config by the caller and must not be logged.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		// No overall timeout here: streaming responses are long-lived and the
		// caller controls cancellation via context.
		httpClient: &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// maxRetries is the number of retry attempts for retryable errors (429 / 5xx).
const maxRetries = 2

// Stream issues a streaming POST /v1/messages. It forwards live deltas to
// onEvent (may be nil) and returns the fully assembled Response. The request is
// cancelable via ctx. The API key is sent in the x-api-key header and never
// logged.
//
// Retryable errors (429 / 5xx) are retried with exponential backoff up to
// maxRetries times. Retries only happen before any SSE events are forwarded to
// the caller — once streaming content begins, the response is committed and
// mid-stream errors are returned as-is.
func (c *Client) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (*Response, error) {
	if c.provider == "openai" {
		return c.streamOpenAI(ctx, req, onEvent)
	}
	return c.streamAnthropic(ctx, req, onEvent)
}

// streamAnthropic issues a streaming POST /v1/messages using the Anthropic
// Messages wire protocol, with retry on retryable errors.
func (c *Client) streamAnthropic(ctx context.Context, req Request, onEvent func(StreamEvent)) (*Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		resp, committed, err := c.doStream(ctx, req, onEvent)
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

// doStream performs a single Anthropic streaming attempt. committed reports
// whether any SSE events were forwarded to onEvent — when true, the caller
// must not retry.
func (c *Client) doStream(ctx context.Context, req Request, onEvent func(StreamEvent)) (resp *Response, committed bool, err error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, false, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("accept", "text/event-stream")
	if c.authToken != "" {
		httpReq.Header.Set("authorization", "Bearer "+c.authToken)
	} else {
		httpReq.Header.Set("x-api-key", c.apiKey)
	}

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
		return asm.handle(ev)
	}
	if err := parseSSE(httpResp.Body, wrappedHandle); err != nil {
		return nil, committed, err
	}
	out := asm.finalize()
	return &out, committed, nil
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

// Model is one entry from the provider's /v1/models listing.
type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// ListModels queries GET /v1/models. Anthropic and compatible proxies expose
// this; flowd uses it to auto-select a model when none is configured, so the
// same build works against GLM/DeepSeek/gateway endpoints without manual setup.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	if c.provider == "openai" {
		return c.listModelsOpenAI(ctx)
	}
	return c.listModelsAnthropic(ctx)
}

func (c *Client) listModelsAnthropic(ctx context.Context) ([]Model, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if c.authToken != "" {
		httpReq.Header.Set("authorization", "Bearer "+c.authToken)
	} else {
		httpReq.Header.Set("x-api-key", c.apiKey)
	}
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
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	return doc.Data, nil
}
