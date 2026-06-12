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

	"github.com/oboo/terflow/internal/classify"
	"github.com/oboo/terflow/internal/env"
	"github.com/oboo/terflow/internal/protocol"
)

// Server holds daemon state. The classifier is read-only after construction, so
// concurrent connections can share it without locking; mu guards refreshes.
//
// The daemon's only job is the instant, offline CMD-vs-NL classification:
// CMD -> accept (zero latency, never touches the network); NL -> hand to the
// agent (flow-agent does all translation/routing/answering itself, below the
// prompt, so the input line is never blocked).
type Server struct {
	mu  sync.RWMutex
	cls *classify.Classifier

	// agentEnabled reports whether mode B is available (a credential is
	// configured). When false, NL degrades to accept (run the line as-is).
	agentEnabled bool

	// model is the configured/discovered model name, surfaced to the widget via
	// an Info request (shown in the prompt). Cosmetic only.
	model string

	// Logf is where verdicts and errors go. Defaults to the standard logger.
	Logf func(format string, args ...any)
}

// New builds a Server with a classifier derived from the current environment.
// The agent (NL) path is disabled until SetAgentEnabled(true).
func New() *Server {
	known := env.Known()
	return &Server{
		cls:  classify.New(env.Keys(known)),
		Logf: log.Printf,
	}
}

// SetAgentEnabled marks whether the NL→agent path is available, and records the
// model name to surface to the widget (cosmetic).
func (s *Server) SetAgentEnabled(on bool, model string) {
	s.mu.Lock()
	s.agentEnabled = on
	s.model = model
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
// handle processes a single connection: classify the input and reply once.
// CMD -> accept; NL -> agent (or accept if the agent is unavailable);
// flowclear -> reset and acknowledge.
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

	// flowclear: acknowledge immediately. (Conversation state now lives in the
	// agent process, which is single-shot per invocation, so there is no daemon
	// session to reset — this stays as a recognized no-op for the widget.)
	if req.Clear {
		s.Logf("flowd: flowclear")
		_ = protocol.WriteJSONLine(conn, protocol.Response{
			Action: protocol.ActionAccept, Reason: "session-cleared",
		})
		return
	}

	// info: report status (model name, agent enabled) without classifying.
	if req.Info {
		s.mu.RLock()
		model, agent := s.model, s.agentEnabled
		s.mu.RUnlock()
		_ = protocol.WriteJSONLine(conn, protocol.Response{
			Action: protocol.ActionAccept, Model: model, Agent: agent,
		})
		return
	}

	s.mu.RLock()
	cls := s.cls
	agentEnabled := s.agentEnabled
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

	// NL with no agent available (no credential): degrade to accept so the line
	// runs as-is rather than blocking.
	if !agentEnabled {
		_ = protocol.WriteJSONLine(conn, protocol.Response{
			Action: protocol.ActionAccept, Verdict: protocol.VerdictNL,
			Reason: res.Reason + "+no-agent",
		})
		return
	}

	// NL -> hand the whole request to the agent. Translation, routing, and
	// answering all happen inside flow-agent (with its own animation/output
	// below the prompt), so the daemon does no network work here — the widget's
	// input line is never blocked.
	_ = protocol.WriteJSONLine(conn, protocol.Response{
		Action: protocol.ActionAgent, Verdict: protocol.VerdictNL,
		Reason: res.Reason, Text: req.Buffer,
	})
}

// translateReply runs the translator and builds the phase-2 response. Any
// failure degrades to accept so the user is never blocked.
// Decide classifies a request and produces the single response the live handler
// would send (minus flowclear, which handle() processes). Retained for unit
// tests: CMD -> accept; NL -> agent (or accept when the agent is disabled).
func (s *Server) Decide(ctx context.Context, req *protocol.Request) protocol.Response {
	s.mu.RLock()
	cls := s.cls
	agentEnabled := s.agentEnabled
	s.mu.RUnlock()

	res := cls.Classify(req.Buffer)
	if res.Label == classify.CMD {
		return protocol.Response{
			Action: protocol.ActionAccept, Verdict: protocol.VerdictCMD, Reason: res.Reason,
		}
	}
	if !agentEnabled {
		return protocol.Response{
			Action: protocol.ActionAccept, Verdict: protocol.VerdictNL,
			Reason: res.Reason + "+no-agent",
		}
	}
	return protocol.Response{
		Action: protocol.ActionAgent, Verdict: protocol.VerdictNL,
		Reason: res.Reason, Text: req.Buffer,
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
