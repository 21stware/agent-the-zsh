package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestStreamRetryOn429 verifies that a 429 response is retried and succeeds
// on the second attempt.
func TestStreamRetryOn429(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"too many requests"}}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(textStreamSSE))
	}))
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	resp, err := c.Stream(context.Background(), Request{
		Model: ModelFast, MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream after retry: %v", err)
	}
	if resp.Text() != "git status" {
		t.Errorf("text = %q, want %q", resp.Text(), "git status")
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// TestStreamRetryOn500 verifies that a 500 response is retried.
func TestStreamRetryOn500(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"internal"}}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(textStreamSSE))
	}))
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	resp, err := c.Stream(context.Background(), Request{
		Model: ModelFast, MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream after retry: %v", err)
	}
	if resp.Text() != "git status" {
		t.Errorf("text = %q", resp.Text())
	}
}

// TestStreamNoRetryOn400 verifies that a 400 (non-retryable) is not retried.
func TestStreamNoRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"type":"bad_request","message":"nope"}}`))
	}))
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	_, err := c.Stream(context.Background(), Request{
		Model: ModelFast, MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}, nil)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry)", got)
	}
}

// TestStreamRetryExhausted verifies that after maxRetries+1 attempts all
// returning 429, the error is returned.
func TestStreamRetryExhausted(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	_, err := c.Stream(context.Background(), Request{
		Model: ModelFast, MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}, nil)
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if got := attempts.Load(); got != int32(maxRetries+1) {
		t.Errorf("attempts = %d, want %d", got, maxRetries+1)
	}
}

// TestStreamRetryPreservesContextCancel verifies that context cancellation
// during the backoff wait aborts the retry loop.
func TestStreamRetryContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow"}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c := New("k", WithBaseURL(srv.URL))
	_, err := c.Stream(ctx, Request{
		Model: ModelFast, MaxTokens: 16,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
	}, nil)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}
