// Package daemon is the flowd server: it listens on a unix socket and answers
// one classification request per connection. It holds the classifier (built
// from the live environment) and, for the NL path, a translator backed by the
// self-built llm client.
//
// Step 3 scope: command verdicts still return action=accept with zero network
// involvement. NL verdicts are translated to a single shell command (mode A)
// and returned as action=replace, for the widget to write back to the buffer
// and await the user's confirmation. If translation is unconfigured (no API
// key) or fails, the daemon degrades to action=accept — never bricking, never
// blocking the command path.
package daemon

import (
	"bufio"
	"context"
	"log"
	"net"
	"sync"
	"time"

	"github.com/oboo/terflow/internal/classify"
	"github.com/oboo/terflow/internal/env"
	"github.com/oboo/terflow/internal/protocol"
	"github.com/oboo/terflow/internal/translate"
)

// Translator is the NL→command interface the daemon depends on. The concrete
// implementation is *translate.Translator; the interface keeps the daemon
// unit-testable without the network.
type Translator interface {
	Translate(ctx context.Context, nl string, tc translate.Context, onDelta func(string)) (*translate.Result, error)
}

// Server holds daemon state. The classifier is read-only after construction, so
// concurrent connections can share it without locking; mu guards refreshes. The
// translator is set once at construction (or left nil to disable the NL path).
type Server struct {
	mu  sync.RWMutex
	cls *classify.Classifier

	// translator handles NL verdicts. Nil disables translation (degrade to
	// accept), e.g. when ANTHROPIC_API_KEY is unset.
	translator Translator

	// TranslateTimeout bounds a single NL translation. Zero uses a default.
	TranslateTimeout time.Duration

	// Logf is where verdicts and errors go. Defaults to the standard logger.
	Logf func(format string, args ...any)
}

// New builds a Server with a classifier derived from the current environment and
// no translator (NL path disabled). Use SetTranslator to enable mode A.
func New() *Server {
	known := env.Known()
	return &Server{
		cls:              classify.New(env.Keys(known)),
		TranslateTimeout: 30 * time.Second,
		Logf:             log.Printf,
	}
}

// SetTranslator enables the NL path with the given translator.
func (s *Server) SetTranslator(t Translator) {
	s.mu.Lock()
	s.translator = t
	s.mu.Unlock()
}

// Serve accepts connections on ln until it is closed. Each connection is
// handled in its own goroutine. Serve returns when ln returns a permanent error
// (e.g. closed listener).
func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

// handle processes a single connection: read one request, decide, reply.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)

	req, err := protocol.ReadRequest(r)
	if err != nil {
		// Malformed/empty request: tell the client to accept (never brick).
		_ = protocol.WriteJSONLine(conn, protocol.Response{
			Action: protocol.ActionAccept,
			Err:    "bad request: " + err.Error(),
		})
		return
	}

	resp := s.Decide(context.Background(), req)
	if err := protocol.WriteJSONLine(conn, resp); err != nil {
		s.Logf("flowd: write response: %v", err)
	}
}

// Decide classifies a request and produces the response. The command path is
// pure and never touches the network. The NL path calls the translator (if
// configured) and returns action=replace; any failure degrades to accept.
func (s *Server) Decide(ctx context.Context, req *protocol.Request) protocol.Response {
	s.mu.RLock()
	cls := s.cls
	tr := s.translator
	timeout := s.TranslateTimeout
	s.mu.RUnlock()

	res := cls.Classify(req.Buffer)

	if res.Label == classify.CMD {
		// Zero-latency command path: accept immediately, nothing on the network.
		s.Logf("flowd: verdict=CMD reason=%s buffer=%q", res.Reason, req.Buffer)
		return protocol.Response{
			Action:  protocol.ActionAccept,
			Verdict: protocol.VerdictCMD,
			Reason:  res.Reason,
		}
	}

	// NL path.
	s.Logf("flowd: verdict=NL reason=%s cwd=%q buffer=%q", res.Reason, req.Cwd, req.Buffer)

	if tr == nil {
		// No translator configured: degrade to accept (step-2 behavior).
		return protocol.Response{
			Action:  protocol.ActionAccept,
			Verdict: protocol.VerdictNL,
			Reason:  res.Reason + "+no-translator",
		}
	}

	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := tr.Translate(tctx, req.Buffer, translate.Context{
		CWD:     req.Cwd,
		History: req.History,
	}, nil)
	if err != nil {
		// Translation failed: degrade to accept so the user can still run their
		// line (it'll likely hit command_not_found_handle in step 4) — never block.
		s.Logf("flowd: translate error: %v", err)
		return protocol.Response{
			Action:  protocol.ActionAccept,
			Verdict: protocol.VerdictNL,
			Reason:  res.Reason,
			Err:     "translate: " + err.Error(),
		}
	}
	if result.Untranslatable {
		// The model could not produce a command: accept the original line.
		return protocol.Response{
			Action:  protocol.ActionAccept,
			Verdict: protocol.VerdictNL,
			Reason:  res.Reason + "+untranslatable",
		}
	}

	effect := protocol.EffectReadOnly
	if result.Effect == translate.EffectSideEffect {
		effect = protocol.EffectSideEffect
	}
	s.Logf("flowd: translated effect=%s -> %q", effect, result.Command)

	return protocol.Response{
		Action:  protocol.ActionReplace,
		Verdict: protocol.VerdictNL,
		Reason:  res.Reason,
		Text:    result.Command,
		Effect:  effect,
	}
}

// Refresh rebuilds the classifier from the current environment plus the given
// client-reported aliases. Safe to call concurrently with Serve.
func (s *Server) Refresh(aliases []string) {
	known := env.Known()
	env.AddAliases(known, aliases)
	cls := classify.New(env.Keys(known))
	s.mu.Lock()
	s.cls = cls
	s.mu.Unlock()
}
