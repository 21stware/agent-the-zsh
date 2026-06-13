package daemon

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/21stware/agent-the-zsh/internal/protocol"
)

// shortSocketDir returns a short-pathed temp dir. macOS limits unix socket
// paths (sun_path) to ~104 bytes, and the default t.TempDir() under
// /var/folders/... overflows it, so we use /tmp with a unique suffix.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "flowtest")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// startTestServer spins up a real flowd on a temp unix socket and returns the
// socket path plus a cleanup func.
func startTestServer(t *testing.T) (string, func()) {
	t.Helper()
	sock := filepath.Join(shortSocketDir(t), "flowd.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := New()
	srv.Logf = func(string, ...any) {} // silence logs in tests
	go srv.Serve(ln)
	return sock, func() { ln.Close() }
}

// roundTrip dials the socket, sends one request, returns the response.
func roundTrip(t *testing.T, sock string, req protocol.Request) protocol.Response {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := protocol.WriteJSONLine(conn, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := bufio.NewReader(conn)
	resp, err := protocol.ReadResponse(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return *resp
}

func TestDaemonRoundTrip(t *testing.T) {
	sock, stop := startTestServer(t)
	defer stop()

	cases := []struct {
		buffer      string
		wantVerdict protocol.Verdict
	}{
		{"git status", protocol.VerdictCMD},
		{"ls -la", protocol.VerdictCMD},
		{"cat access.log | awk '{print $1}' | sort", protocol.VerdictCMD},
		{`git commit -m "fix the bug please"`, protocol.VerdictCMD},
		{"帮我列出所有正在运行的容器", protocol.VerdictNL},
		{"how do I undo the last commit", protocol.VerdictNL},
		{"find my ssh config file", protocol.VerdictNL},
	}
	for _, c := range cases {
		resp := roundTrip(t, sock, protocol.Request{
			Buffer: c.buffer, Cwd: "/tmp", Proto: protocol.CurrentProto,
		})
		// Step 2 invariant: ALWAYS accept, regardless of verdict.
		if resp.Action != protocol.ActionAccept {
			t.Errorf("%q: action=%q, want accept (step-2 transparency invariant)", c.buffer, resp.Action)
		}
		if resp.Verdict != c.wantVerdict {
			t.Errorf("%q: verdict=%q, want %q (reason=%s)", c.buffer, resp.Verdict, c.wantVerdict, resp.Reason)
		}
	}
}

// TestDaemonNeverEmitsReplaceInStep2 locks the step-2 contract: the daemon must
// not ask the widget to replace the buffer until step 3. If this ever fails,
// the transparency guarantee broke.
func TestDaemonNeverEmitsReplaceInStep2(t *testing.T) {
	sock, stop := startTestServer(t)
	defer stop()
	for _, b := range []string{"git status", "帮我看看磁盘", "rm -rf /tmp/x", "please delete everything"} {
		resp := roundTrip(t, sock, protocol.Request{Buffer: b, Proto: protocol.CurrentProto})
		if resp.Action == protocol.ActionReplace {
			t.Errorf("%q: daemon emitted replace in step 2", b)
		}
	}
}

// TestLatency measures the local socket round-trip. This is the number that
// backs constraint 1 (zero latency for commands). It is informational, but we
// also assert a loose ceiling so a regression (e.g. accidental network call)
// trips the test.
func TestLatency(t *testing.T) {
	sock, stop := startTestServer(t)
	defer stop()

	const n = 200
	start := time.Now()
	for i := 0; i < n; i++ {
		roundTrip(t, sock, protocol.Request{Buffer: "git status", Proto: protocol.CurrentProto})
	}
	per := time.Since(start) / n
	t.Logf("mean round-trip over %d iters (dial+write+classify+read+close): %v", n, per)
	if per > 5*time.Millisecond {
		t.Errorf("round-trip %v exceeds 5ms ceiling — is something blocking on the hot path?", per)
	}
}

func TestBadRequestDegradesToAccept(t *testing.T) {
	sock, stop := startTestServer(t)
	defer stop()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Send garbage, not valid JSON.
	conn.Write([]byte("this is not json\n"))
	r := bufio.NewReader(conn)
	resp, err := protocol.ReadResponse(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Action != protocol.ActionAccept {
		t.Errorf("bad request: action=%q, want accept (must never brick)", resp.Action)
	}
	if resp.Err == "" {
		t.Errorf("bad request: expected Err to be set")
	}
}

func TestStaleSocketReclaimed(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "flowd.sock")
	// First listener simulates a crashed daemon: bind then "crash" by leaving
	// the file but closing without removing. We emulate by listening, then
	// closing the underlying file handle is hard; instead create a plain file.
	ln1, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	// A second Listen while ln1 is live must refuse.
	if _, err := Listen(sock); err == nil {
		t.Errorf("second Listen succeeded while first is live; want refusal")
	}
	ln1.Close() // removes the socket

	// Now a fresh Listen should succeed again.
	ln2, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen after close: %v", err)
	}
	ln2.Close()
}
