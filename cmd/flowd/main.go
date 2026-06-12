// Command flowd is the flow daemon: a single Go binary that listens on a unix
// socket and classifies each input line the zsh widget sends it.
//
// Usage:
//
//	flowd                 # run in foreground, log to stderr
//	flowd -socket PATH     # override the socket path
//	FLOW_SOCKET=PATH flowd # same, via env
//
// On SIGINT/SIGTERM it closes the listener (removing the socket) and exits.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/oboo/terflow/internal/config"
	"github.com/oboo/terflow/internal/daemon"
)

func main() {
	socketFlag := flag.String("socket", "", "unix socket path (default: auto via FLOW_SOCKET/XDG_RUNTIME_DIR/TMPDIR)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("")

	path := *socketFlag
	if path == "" {
		p, err := daemon.SocketPath()
		if err != nil {
			log.Fatalf("flowd: resolve socket path: %v", err)
		}
		path = p
	}

	ln, err := daemon.Listen(path)
	if err != nil {
		log.Fatalf("flowd: %v", err)
	}
	log.Printf("flowd: listening on %s", path)

	srv := daemon.New()

	// Resolve the LLM provider (first-party API or an Anthropic-compatible
	// proxy: GLM, DeepSeek, a gateway). Config comes from the process env, then
	// ~/.claude/settings.json. The NL path is enabled only when a credential is
	// present; otherwise NL verdicts degrade to accept and flow stays a
	// transparent zsh. Credentials are never logged.
	// The daemon does only instant CMD-vs-NL classification. NL is handed to
	// flow-agent (which does translation/routing/answering itself), so the
	// daemon needs no LLM client or model — just whether a credential is
	// configured, to decide if the agent path is available. Credentials are
	// never logged.
	cfg := config.Load()
	if cfg.Enabled() {
		srv.SetAgentEnabled(true)
		log.Printf("flowd: agent (NL) path enabled — endpoint=%s auth=%s", cfg.BaseURL, cfg.Source)
	} else {
		log.Printf("flowd: no LLM credential (ANTHROPIC_AUTH_TOKEN/API_KEY) — NL degrades to running the line as-is")
	}

	// Graceful shutdown: closing the unix listener removes the socket file.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigc
		log.Printf("flowd: received %v, shutting down", s)
		ln.Close()
	}()

	if err := srv.Serve(ln); err != nil {
		// Accept error after Close() is expected on shutdown.
		if ne, ok := err.(net.Error); ok && !ne.Timeout() {
			// closed listener: normal exit
		}
		log.Printf("flowd: serve stopped: %v", err)
	}
}
