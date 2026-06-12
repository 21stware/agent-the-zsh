package daemon

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/oboo/terflow/internal/protocol"
)

// TestDecideCommand: a command verdict accepts and is never routed to the agent.
func TestDecideCommand(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetAgentEnabled(true)
	resp := srv.Decide(context.Background(), &protocol.Request{Buffer: "git status", Cwd: "/tmp"})
	if resp.Action != protocol.ActionAccept {
		t.Errorf("CMD action = %q, want accept", resp.Action)
	}
	if resp.Verdict != protocol.VerdictCMD {
		t.Errorf("verdict = %q, want CMD", resp.Verdict)
	}
}

// TestDecideNLRoutesToAgent: an NL verdict routes to the agent, carrying the
// original input as the task.
func TestDecideNLRoutesToAgent(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetAgentEnabled(true)
	resp := srv.Decide(context.Background(), &protocol.Request{Buffer: "帮我看看 git 状态"})
	if resp.Action != protocol.ActionAgent {
		t.Fatalf("action = %q, want agent", resp.Action)
	}
	if resp.Verdict != protocol.VerdictNL {
		t.Errorf("verdict = %q, want NL", resp.Verdict)
	}
	if resp.Text != "帮我看看 git 状态" {
		t.Errorf("agent text = %q, want the original input", resp.Text)
	}
}

// TestDecideNLNoAgentDegrades: with the agent disabled (no credential), NL
// degrades to accept so the line runs as-is.
func TestDecideNLNoAgentDegrades(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	// agent NOT enabled
	resp := srv.Decide(context.Background(), &protocol.Request{Buffer: "please list all the files"})
	if resp.Action != protocol.ActionAccept {
		t.Errorf("action = %q, want accept (degrade when agent disabled)", resp.Action)
	}
	if resp.Verdict != protocol.VerdictNL {
		t.Errorf("verdict = %q, want NL", resp.Verdict)
	}
}

// TestHandleCommandSinglePhase: over the socket, a command gets exactly one
// accept reply and the connection closes (no extra lines).
func TestHandleCommandSinglePhase(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "flowd.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetAgentEnabled(true)
	go srv.Serve(ln)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	protocol.WriteJSONLine(conn, protocol.Request{Buffer: "git status", Proto: protocol.CurrentProto})
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	p1, err := protocol.ReadResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Action != protocol.ActionAccept || p1.Verdict != protocol.VerdictCMD {
		t.Errorf("command reply = action %q verdict %q, want accept/CMD", p1.Action, p1.Verdict)
	}
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := protocol.ReadResponse(r); err == nil {
		t.Error("command path sent a second line; want single-phase")
	}
}

// TestHandleNLAgentSinglePhase: over the socket, an NL line gets exactly one
// agent reply (no pending phase anymore).
func TestHandleNLAgentSinglePhase(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "flowd.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetAgentEnabled(true)
	go srv.Serve(ln)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	protocol.WriteJSONLine(conn, protocol.Request{Buffer: "帮我重构这个模块", Proto: protocol.CurrentProto})
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	p1, err := protocol.ReadResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Action != protocol.ActionAgent {
		t.Errorf("NL reply action = %q, want agent", p1.Action)
	}
	if p1.Text != "帮我重构这个模块" {
		t.Errorf("agent text = %q", p1.Text)
	}
}

// TestHandleClearAcknowledged: flowclear is recognized and acknowledged.
func TestHandleClearAcknowledged(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "flowd.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New()
	srv.Logf = func(string, ...any) {}
	go srv.Serve(ln)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	protocol.WriteJSONLine(conn, protocol.Request{Clear: true, Proto: protocol.CurrentProto})
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	resp, err := protocol.ReadResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Action != protocol.ActionAccept {
		t.Errorf("clear reply action = %q, want accept", resp.Action)
	}
}
