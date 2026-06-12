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
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oboo/terflow/internal/config"
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

	// Resolve the LLM provider (first-party API or an Anthropic-compatible
	// proxy: GLM, DeepSeek, a gateway). Config comes from the process env, then
	// ~/.claude/settings.json. The NL path is enabled only when a credential is
	// present; otherwise NL verdicts degrade to accept and flow stays a
	// transparent zsh. Credentials are never logged.
	cfg := config.Load()
	if cfg.Enabled() {
		opts := []llm.Option{llm.WithBaseURL(cfg.BaseURL)}
		if cfg.AuthToken != "" {
			opts = append(opts, llm.WithAuthToken(cfg.AuthToken))
		}
		client := llm.New(cfg.APIKey, opts...)

		fastModel := cfg.FastModel
		if fastModel == "" {
			fastModel = cfg.Model
		}
		if fastModel == "" {
			// No model configured: discover one from the provider so the same
			// build works against any compatible endpoint (GLM/DeepSeek/gateway).
			if m, err := pickFastModel(client); err != nil {
				log.Printf("flowd: model auto-discovery failed (%v); set ANTHROPIC_SMALL_FAST_MODEL or ANTHROPIC_MODEL", err)
			} else {
				fastModel = m
				log.Printf("flowd: auto-selected model %q from %s/v1/models", fastModel, cfg.BaseURL)
			}
		}
		if fastModel == "" {
			log.Printf("flowd: no usable model — NL translation disabled")
		} else {
			srv.SetTranslator(translate.New(client, fastModel))
			log.Printf("flowd: NL translation enabled — endpoint=%s auth=%s model=%q",
				cfg.BaseURL, cfg.Source, fastModel)
		}
	} else {
		log.Printf("flowd: no LLM credential (ANTHROPIC_AUTH_TOKEN/API_KEY) — NL translation disabled, NL verdicts will accept")
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

// pickFastModel discovers a model from the provider when none is configured. It
// prefers names that signal a small/fast model (the right tier for one-line
// command translation), and otherwise returns the first listed model. Works
// across Anthropic-compatible providers (haiku, mini, flash, small, lite…).
func pickFastModel(client *llm.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := client.ListModels(ctx)
	if err != nil {
		return "", err
	}
	if len(models) == 0 {
		return "", fmt.Errorf("provider returned no models")
	}
	// Preference order of substrings signaling a fast/small tier.
	prefer := []string{"haiku", "mini", "flash", "small", "lite", "fast", "air"}
	for _, p := range prefer {
		for _, m := range models {
			if strings.Contains(strings.ToLower(m.ID), p) {
				return m.ID, nil
			}
		}
	}
	return models[0].ID, nil
}
