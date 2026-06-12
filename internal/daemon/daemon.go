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

	// session is the recent NL conversation, for follow-up context. Guarded by
	// mu. Bounded to sessionMax turns.
	session []translate.SessionTurn

	// Logf is where verdicts and errors go. Defaults to the standard logger.
	Logf func(format string, args ...any)
}

// sessionMax bounds how many recent NL turns are kept for follow-up context.
const sessionMax = 6

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

// handle processes a single connection. The reply is two-phase so the command
// path stays bounded by the widget's short first-phase timeout while NL
// translation may take seconds:
//   - CMD (or NL with no translator): one line — accept — sent immediately.
//   - NL with a translator: a "pending" line immediately, then a second line
//     (replace/accept) once translation completes.
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

	// flowclear: reset the NL conversation and acknowledge immediately.
	if req.Clear {
		s.mu.Lock()
		s.session = nil
		s.mu.Unlock()
		s.Logf("flowd: session cleared")
		_ = protocol.WriteJSONLine(conn, protocol.Response{
			Action: protocol.ActionAccept, Reason: "session-cleared",
		})
		return
	}

	s.mu.RLock()
	cls := s.cls
	tr := s.translator
	timeout := s.TranslateTimeout
	s.mu.RUnlock()

	res := cls.Classify(req.Buffer)

	// Command path: pure, zero-latency, never touches the network.
	if res.Label == classify.CMD {
		s.Logf("flowd: verdict=CMD reason=%s buffer=%q", res.Reason, req.Buffer)
		_ = protocol.WriteJSONLine(conn, protocol.Response{
			Action: protocol.ActionAccept, Verdict: protocol.VerdictCMD, Reason: res.Reason,
		})
		return
	}

	s.Logf("flowd: verdict=NL reason=%s cwd=%q buffer=%q", res.Reason, req.Cwd, req.Buffer)

	// NL with no translator configured: degrade to accept in one phase.
	if tr == nil {
		_ = protocol.WriteJSONLine(conn, protocol.Response{
			Action: protocol.ActionAccept, Verdict: protocol.VerdictNL,
			Reason: res.Reason + "+no-translator",
		})
		return
	}

	// Phase 1: tell the widget we're translating, so it can show progress and
	// switch to a longer read timeout. A write failure here means the client
	// already gave up; stop.
	if err := protocol.WriteJSONLine(conn, protocol.Response{
		Action: protocol.ActionPending, Verdict: protocol.VerdictNL, Reason: res.Reason,
	}); err != nil {
		return
	}

	// Phase 2: translate and send the real reply on the same connection.
	resp := s.translateReply(context.Background(), tr, req, res.Reason, timeout)
	if err := protocol.WriteJSONLine(conn, resp); err != nil {
		s.Logf("flowd: write phase-2 response: %v", err)
	}
}

// translateReply runs the translator and builds the phase-2 response. Any
// failure degrades to accept so the user is never blocked.
func (s *Server) translateReply(ctx context.Context, tr Translator, req *protocol.Request, reason string, timeout time.Duration) protocol.Response {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := tr.Translate(tctx, req.Buffer, translate.Context{
		CWD:     req.Cwd,
		History: req.History,
		Session: s.snapshotSession(),
	}, nil)
	if err != nil {
		s.Logf("flowd: translate error: %v", err)
		return protocol.Response{
			Action: protocol.ActionAccept, Verdict: protocol.VerdictNL,
			Reason: reason, Err: "translate: " + err.Error(),
		}
	}
	if result.Untranslatable {
		return protocol.Response{
			Action: protocol.ActionAccept, Verdict: protocol.VerdictNL,
			Reason: reason + "+untranslatable",
		}
	}
	if result.Agent {
		// Route to mode B: the widget hands the original NL task to flow-agent.
		// Record the turn so a later NL follow-up has context.
		s.recordSession(req.Buffer, "[handed to the agent]")
		s.Logf("flowd: routed to agent: %q", req.Buffer)
		return protocol.Response{
			Action: protocol.ActionAgent, Verdict: protocol.VerdictNL,
			Reason: reason + "+agent", Text: req.Buffer,
		}
	}

	effect := protocol.EffectReadOnly
	if result.Effect == translate.EffectSideEffect {
		effect = protocol.EffectSideEffect
	}
	s.recordSession(req.Buffer, result.Command)
	s.Logf("flowd: translated effect=%s -> %q", effect, result.Command)
	return protocol.Response{
		Action: protocol.ActionReplace, Verdict: protocol.VerdictNL,
		Reason: reason, Text: result.Command, Effect: effect,
	}
}

// snapshotSession returns a copy of the current NL conversation turns.
func (s *Server) snapshotSession() []translate.SessionTurn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.session) == 0 {
		return nil
	}
	out := make([]translate.SessionTurn, len(s.session))
	copy(out, s.session)
	return out
}

// recordSession appends an NL turn, trimming to the most recent sessionMax.
func (s *Server) recordSession(request, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = append(s.session, translate.SessionTurn{Request: request, Result: result})
	if len(s.session) > sessionMax {
		s.session = s.session[len(s.session)-sessionMax:]
	}
}

// Decide classifies a request and produces a single response (no streaming
// phases). It is retained for unit tests and callers that want the final
// decision in one call; the live server uses the two-phase handle path above.
func (s *Server) Decide(ctx context.Context, req *protocol.Request) protocol.Response {
	s.mu.RLock()
	cls := s.cls
	tr := s.translator
	timeout := s.TranslateTimeout
	s.mu.RUnlock()

	res := cls.Classify(req.Buffer)

	if res.Label == classify.CMD {
		s.Logf("flowd: verdict=CMD reason=%s buffer=%q", res.Reason, req.Buffer)
		return protocol.Response{
			Action:  protocol.ActionAccept,
			Verdict: protocol.VerdictCMD,
			Reason:  res.Reason,
		}
	}

	s.Logf("flowd: verdict=NL reason=%s cwd=%q buffer=%q", res.Reason, req.Cwd, req.Buffer)

	if tr == nil {
		return protocol.Response{
			Action:  protocol.ActionAccept,
			Verdict: protocol.VerdictNL,
			Reason:  res.Reason + "+no-translator",
		}
	}

	return s.translateReply(ctx, tr, req, res.Reason, timeout)
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
