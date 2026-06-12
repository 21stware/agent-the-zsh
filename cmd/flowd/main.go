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

	"github.com/oboo/terflow/internal/daemon"
	"github.com/oboo/terflow/internal/llm"
	"github.com/oboo/terflow/internal/translate"
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

	// Enable the NL path (mode A) only when an API key is present. Without it,
	// the daemon stays in pure-classification mode and NL verdicts degrade to
	// accept — flow remains a transparent zsh. The key is read from the
	// environment and never logged.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		var opts []llm.Option
		// Test/advanced hook: redirect the API endpoint (e.g. to a local mock).
		if base := os.Getenv("FLOW_ANTHROPIC_BASE_URL"); base != "" {
			opts = append(opts, llm.WithBaseURL(base))
		}
		client := llm.New(key, opts...)
		srv.SetTranslator(translate.New(client, llm.ModelFast))
		log.Printf("flowd: NL translation enabled (model %s)", llm.ModelFast)
	} else {
		log.Printf("flowd: ANTHROPIC_API_KEY unset — NL translation disabled, NL verdicts will accept")
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
