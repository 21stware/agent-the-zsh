// Package daemon is the flowd server: it listens on a unix socket and answers
// one classification request per connection. It holds the classifier (built
// from the live environment) and, in later steps, the Claude session.
//
// Step 2 scope: every request is classified and answered with action=accept.
// The verdict (CMD/NL) is logged and returned for annotation, but NL is NOT yet
// translated — that is step 3. This keeps the tool fully transparent: the user
// experiences a plain zsh, with the daemon observing in the background.
package daemon

import (
	"bufio"
	"log"
	"net"
	"sync"

	"github.com/oboo/terflow/internal/classify"
	"github.com/oboo/terflow/internal/env"
	"github.com/oboo/terflow/internal/protocol"
)

// Server holds daemon state. The classifier is read-only after construction, so
// concurrent connections can share it without locking; mu guards refreshes.
type Server struct {
	mu  sync.RWMutex
	cls *classify.Classifier

	// Logf is where verdicts and errors go. Defaults to the standard logger.
	Logf func(format string, args ...any)
}

// New builds a Server with a classifier derived from the current environment.
func New() *Server {
	known := env.Known()
	return &Server{
		cls:  classify.New(env.Keys(known)),
		Logf: log.Printf,
	}
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

// handle processes a single connection: read one request, classify, reply.
// Connections are one-shot in step 2 (request/response then close), which keeps
// the widget simple and avoids any cross-request state on the hot path.
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

	resp := s.Decide(req)
	if err := protocol.WriteJSONLine(conn, resp); err != nil {
		s.Logf("flowd: write response: %v", err)
	}
}

// Decide runs the classifier for a request and produces the step-2 response.
// Split out from handle so it is unit-testable without a socket.
func (s *Server) Decide(req *protocol.Request) protocol.Response {
	s.mu.RLock()
	cls := s.cls
	s.mu.RUnlock()

	res := cls.Classify(req.Buffer)
	verdict := protocol.VerdictCMD
	if res.Label == classify.NL {
		verdict = protocol.VerdictNL
	}

	s.Logf("flowd: verdict=%s reason=%s cwd=%q buffer=%q",
		verdict, res.Reason, req.Cwd, req.Buffer)

	// Step 2: always accept. NL translation (action=replace) arrives in step 3.
	return protocol.Response{
		Action:  protocol.ActionAccept,
		Verdict: verdict,
		Reason:  res.Reason,
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
