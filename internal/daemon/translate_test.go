package daemon

import (
	"bufio"
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/oboo/terflow/internal/protocol"
	"github.com/oboo/terflow/internal/translate"
)

// fakeTranslator returns a canned result (or error) without touching the network.
type fakeTranslator struct {
	result *translate.Result
	err    error
	delay  time.Duration
	gotNL  string
	gotCtx translate.Context
}

func (f *fakeTranslator) Translate(ctx context.Context, nl string, tc translate.Context, _ func(string)) (*translate.Result, error) {
	f.gotNL = nl
	f.gotCtx = tc
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.result, f.err
}

// TestTwoPhaseNLOverSocket verifies the live wire behavior: an NL line with a
// translator yields a "pending" line first, then a "replace" line — even when
// translation takes longer than the (notional) command-path timeout. This is
// the regression guard for the UAT bug where NL was misrun as a command because
// the widget gave up before the ~2s translation finished.
func TestTwoPhaseNLOverSocket(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "flowd.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New()
	srv.Logf = func(string, ...any) {}
	// Translation deliberately slower than a command would ever take.
	srv.SetTranslator(&fakeTranslator{
		result: &translate.Result{Command: "git status", Effect: translate.EffectReadOnly},
		delay:  300 * time.Millisecond,
	})
	go srv.Serve(ln)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteJSONLine(conn, protocol.Request{Buffer: "帮我看看 git 状态", Proto: protocol.CurrentProto}); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)

	// Phase 1 must arrive quickly and be "pending".
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	p1, err := protocol.ReadResponse(r)
	if err != nil {
		t.Fatalf("phase-1 read: %v", err)
	}
	if p1.Action != protocol.ActionPending {
		t.Errorf("phase-1 action = %q, want pending", p1.Action)
	}
	if p1.Verdict != protocol.VerdictNL {
		t.Errorf("phase-1 verdict = %q, want NL", p1.Verdict)
	}

	// Phase 2 arrives after translation completes, with the command.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	p2, err := protocol.ReadResponse(r)
	if err != nil {
		t.Fatalf("phase-2 read: %v", err)
	}
	if p2.Action != protocol.ActionReplace {
		t.Errorf("phase-2 action = %q, want replace", p2.Action)
	}
	if p2.Text != "git status" {
		t.Errorf("phase-2 text = %q, want %q", p2.Text, "git status")
	}
	if p2.Effect != protocol.EffectReadOnly {
		t.Errorf("phase-2 effect = %q", p2.Effect)
	}
}

// TestCommandStaysSinglePhase confirms a command verdict still sends exactly one
// line (accept) — no pending — so the command path is never delayed.
func TestCommandStaysSinglePhase(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "flowd.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetTranslator(&fakeTranslator{result: &translate.Result{Command: "x"}})
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
	// No second line should come; a short read should hit EOF (conn closed by daemon).
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := protocol.ReadResponse(r); err == nil {
		t.Error("command path sent a second line; want single-phase")
	}
}

func TestDecideCommandNeverTranslates(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	ft := &fakeTranslator{result: &translate.Result{Command: "should not be used"}}
	srv.SetTranslator(ft)

	resp := srv.Decide(context.Background(), &protocol.Request{Buffer: "git status", Cwd: "/tmp"})
	if resp.Action != protocol.ActionAccept {
		t.Errorf("CMD action = %q, want accept", resp.Action)
	}
	if resp.Verdict != protocol.VerdictCMD {
		t.Errorf("verdict = %q, want CMD", resp.Verdict)
	}
	if ft.gotNL != "" {
		t.Errorf("translator was called for a command (%q) — command path must never hit the network", ft.gotNL)
	}
}

func TestDecideNLReplaceReadOnly(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	ft := &fakeTranslator{result: &translate.Result{Command: "git status", Effect: translate.EffectReadOnly}}
	srv.SetTranslator(ft)

	resp := srv.Decide(context.Background(), &protocol.Request{
		Buffer: "帮我看看 git 状态", Cwd: "/tmp/p", History: []string{"cd /tmp/p"},
	})
	if resp.Action != protocol.ActionReplace {
		t.Fatalf("action = %q, want replace", resp.Action)
	}
	if resp.Text != "git status" {
		t.Errorf("text = %q, want %q", resp.Text, "git status")
	}
	if resp.Effect != protocol.EffectReadOnly {
		t.Errorf("effect = %q, want read-only", resp.Effect)
	}
	if ft.gotNL != "帮我看看 git 状态" {
		t.Errorf("translator got nl = %q", ft.gotNL)
	}
	if ft.gotCtx.CWD != "/tmp/p" || len(ft.gotCtx.History) != 1 {
		t.Errorf("translator got ctx = %+v", ft.gotCtx)
	}
}

func TestDecideNLReplaceSideEffect(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetTranslator(&fakeTranslator{result: &translate.Result{
		Command: "rm -rf node_modules", Effect: translate.EffectSideEffect,
	}})
	resp := srv.Decide(context.Background(), &protocol.Request{Buffer: "delete node_modules"})
	if resp.Action != protocol.ActionReplace {
		t.Fatalf("action = %q, want replace", resp.Action)
	}
	if resp.Effect != protocol.EffectSideEffect {
		t.Errorf("effect = %q, want side-effect", resp.Effect)
	}
}

func TestDecideNLNoTranslatorDegrades(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	// no translator set
	resp := srv.Decide(context.Background(), &protocol.Request{Buffer: "please list all the files"})
	if resp.Action != protocol.ActionAccept {
		t.Errorf("action = %q, want accept (degrade when no translator)", resp.Action)
	}
	if resp.Verdict != protocol.VerdictNL {
		t.Errorf("verdict = %q, want NL", resp.Verdict)
	}
}

func TestDecideNLTranslateErrorDegrades(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetTranslator(&fakeTranslator{err: errors.New("boom")})
	resp := srv.Decide(context.Background(), &protocol.Request{Buffer: "please list all the files"})
	if resp.Action != protocol.ActionAccept {
		t.Errorf("action = %q, want accept (degrade on translate error)", resp.Action)
	}
	if resp.Err == "" {
		t.Error("expected Err to be populated on translate failure")
	}
}

func TestDecideNLUntranslatableDegrades(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetTranslator(&fakeTranslator{result: &translate.Result{Untranslatable: true}})
	resp := srv.Decide(context.Background(), &protocol.Request{Buffer: "what is the meaning of life"})
	if resp.Action != protocol.ActionAccept {
		t.Errorf("action = %q, want accept (degrade when untranslatable)", resp.Action)
	}
}

func TestDecideNLRoutesToAgent(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetTranslator(&fakeTranslator{result: &translate.Result{Agent: true}})
	resp := srv.Decide(context.Background(), &protocol.Request{Buffer: "refactor the auth module and run tests"})
	if resp.Action != protocol.ActionAgent {
		t.Fatalf("action = %q, want agent", resp.Action)
	}
	if resp.Text != "refactor the auth module and run tests" {
		t.Errorf("agent text = %q, want the original task", resp.Text)
	}
}

// TestSessionHistoryRecordedAndPassed verifies the daemon records NL turns and
// passes prior turns as context to the next translation (follow-up support).
func TestSessionHistoryRecordedAndPassed(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	ft := &fakeTranslator{result: &translate.Result{Command: "ls -la", Effect: translate.EffectReadOnly}}
	srv.SetTranslator(ft)

	// First NL turn: no prior session. Use clearly-NL (Chinese) inputs so the
	// classifier routes them to the translator.
	srv.Decide(context.Background(), &protocol.Request{Buffer: "帮我列出所有文件"})
	if len(ft.gotCtx.Session) != 0 {
		t.Errorf("first turn should have empty session, got %d", len(ft.gotCtx.Session))
	}
	// Second NL turn: the first turn should now be in the session context.
	srv.Decide(context.Background(), &protocol.Request{Buffer: "帮我把它改成递归"})
	if len(ft.gotCtx.Session) != 1 {
		t.Fatalf("second turn session len = %d, want 1", len(ft.gotCtx.Session))
	}
	if ft.gotCtx.Session[0].Request != "帮我列出所有文件" || ft.gotCtx.Session[0].Result != "ls -la" {
		t.Errorf("session turn = %+v, want {帮我列出所有文件 -> ls -la}", ft.gotCtx.Session[0])
	}
}

func TestSessionClearViaHandle(t *testing.T) {
	srv := New()
	srv.Logf = func(string, ...any) {}
	srv.SetTranslator(&fakeTranslator{result: &translate.Result{Command: "ls"}})
	srv.Decide(context.Background(), &protocol.Request{Buffer: "帮我列出所有文件"})
	if len(srv.snapshotSession()) != 1 {
		t.Fatalf("expected 1 session turn before clear")
	}
	// Simulate the clear path (handle sets s.session = nil on req.Clear).
	srv.mu.Lock()
	srv.session = nil
	srv.mu.Unlock()
	if len(srv.snapshotSession()) != 0 {
		t.Errorf("session not cleared")
	}
}
