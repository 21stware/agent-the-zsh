package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/oboo/terflow/internal/protocol"
	"github.com/oboo/terflow/internal/translate"
)

// fakeTranslator returns a canned result (or error) without touching the network.
type fakeTranslator struct {
	result *translate.Result
	err    error
	gotNL  string
	gotCtx translate.Context
}

func (f *fakeTranslator) Translate(_ context.Context, nl string, tc translate.Context, _ func(string)) (*translate.Result, error) {
	f.gotNL = nl
	f.gotCtx = tc
	return f.result, f.err
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
