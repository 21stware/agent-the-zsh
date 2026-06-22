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
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/21stware/agent-the-zsh/internal/config"
	"github.com/21stware/agent-the-zsh/internal/daemon"
	"github.com/21stware/agent-the-zsh/internal/llm"
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
	// proxy: GLM, DeepSeek, a gateway). Config comes from ~/.flow/settings.json
	// (highest priority), then the process env, then ~/.claude/settings.json.
	// The daemon does only instant CMD-vs-NL classification. NL is handed to
	// flow-agent (which does translation/routing/answering itself), so the
	// daemon needs no LLM client — just whether a credential is configured, to
	// decide if the agent path is available. The model name is cosmetic (shown
	// in the prompt). Credentials are never logged.
	cfg := config.Load()
	if cfg.Enabled() {
		model := cfg.Model
		if model == "" {
			model = cfg.FastModel
		}
		srv.SetAgentEnabled(true, model)
		log.Printf("flowd: agent (NL) path enabled — provider=%s endpoint=%s auth=%s model=%q",
			cfg.Provider, cfg.BaseURL, cfg.Source, model)
		// If no model is configured, discover one in the background (don't block
		// startup) so the prompt can show a real name.
		if model == "" {
			log.Printf("flowd: no model configured — set \"model\" in ~/.flow/settings.json or ANTHROPIC_MODEL env to avoid startup discovery delay")
			go func() {
				if m := discoverModel(cfg); m != "" {
					srv.SetAgentEnabled(true, m)
					log.Printf("flowd: discovered model %q from %s", m, cfg.BaseURL)
				}
			}()
		}
	} else {
		log.Printf("flowd: no LLM credential (ANTHROPIC_AUTH_TOKEN/API_KEY) — NL degrades to running the line as-is")
		log.Printf("flowd: to enable, configure ~/.flow/settings.json or set ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY; run flow-doctor for diagnostics")
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

// discoverModel asks the provider's /v1/models for a model name to display when
// none is configured. Best-effort and cosmetic; returns "" on any failure.
// Prefers a capable tier for the display name.
func discoverModel(cfg *config.Config) string {
	opts := []llm.Option{llm.WithBaseURL(cfg.BaseURL)}
	if cfg.AuthToken != "" {
		opts = append(opts, llm.WithAuthToken(cfg.AuthToken))
	}
	client := llm.New(cfg.APIKey, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := client.ListModels(ctx)
	if err != nil || len(models) == 0 {
		return ""
	}
	prefer := []string{"opus", "sonnet", "gpt-5", "gpt-4", "pro", "haiku", "mini", "flash"}
	for _, p := range prefer {
		for _, m := range models {
			if strings.Contains(strings.ToLower(m.ID), p) {
				return m.ID
			}
		}
	}
	return models[0].ID
}
