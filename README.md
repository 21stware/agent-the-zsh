# flow

A zsh smart input layer. You type in your own zsh as usual; on Enter:

- **Shell command** → runs immediately, zero latency, exactly as if flow were
  not installed.
- **Natural language** → (step 3+) translated to a command and written back
  into your input line for you to confirm.

flow deliberately does **not** emulate a terminal. It only takes over the
judgment and rewriting of a single input line.

## Status

- **Step 1 — offline classifier + accuracy baseline** ✅
  Stage-0 rule cascade (`internal/classify`). Baseline on nl2bash + tldr +
  hand-built adversarials: **98.5% overall, 0% dangerous (command→NL) errors.**
- **Step 2 — widget + daemon + zero-latency passthrough** ✅
  `flowd` daemon classifies each line over a unix socket; the zsh widget
  (`shell/flow.zsh`) accepts commands immediately and degrades to plain zsh if
  the daemon is unavailable. Local round-trip ~70µs.
- **Step 3 — NL→command translation (mode A)** ✅
  Self-built Anthropic client (`internal/llm`: raw HTTP/JSON + SSE, no SDK).
  `internal/translate` turns NL into one shell command via a fast model,
  streaming, and classifies its blast radius (read-only vs side-effect). The
  daemon returns `action=replace` for NL; the widget writes the command back to
  the buffer and waits for the user's Enter (never auto-runs). No API key →
  NL degrades to `accept`; the command path never touches the network.
- Step 4 — Esc Esc undo, command_not_found fallback, side-effect confirmation. _next_

## Architecture

```
zsh widget (thin client)  ──unix socket, JSON line──▶  flowd (Go daemon)
  intercept Enter                                       classify CMD vs NL
  CMD: accept-line (0 latency)                          (no network on CMD path)
  daemon down/slow: degrade to plain accept-line
```

Design constraints (non-negotiable):
1. **Command path zero latency** — a CMD verdict accepts immediately; nothing
   touches the network.
2. **Bias to command** — when ambiguous, treat as a command. Misrouting a
   command to NL is far worse than the reverse.
3. **Reversible mistakes** — translated NL is written back and waits at
   end-of-line; never auto-runs a side-effecting command.
4. **Fail to degrade** — if the daemon is missing/slow/erroring, the widget
   falls back to plain zsh. It never bricks the terminal.

## Build & run

```sh
go build -o flowd ./cmd/flowd
./flowd &                      # starts the daemon (socket under $TMPDIR/flow-<uid>)
source shell/flow.zsh          # in your ~/.zshrc, after the daemon is up
```

Environment:
- `ANTHROPIC_API_KEY` — read from env (step 3+); never hardcoded.
- `FLOW_SOCKET` — override the socket path (client and daemon must agree).
- `FLOW_TIMEOUT` — widget reply timeout in seconds (default 0.4); on timeout the
  widget degrades to plain accept-line.

## Test

```sh
go test ./...                  # classifier accuracy + daemon round-trip + latency
```
