# flow

A zsh smart input layer. You type in your own zsh as usual; on Enter:

- **Shell command** → runs immediately, zero latency, exactly as if flow were
  not installed.
- **Natural language** → your typed text stays on its line and the agent runs
  inline below it: it translates+runs a single command, does multi-step work, or
  answers a question — streaming thinking and output after the prompt.

flow deliberately does **not** emulate a terminal. It only takes over the
judgment of a single input line: run it as a command, or hand it to the agent.

## Status

- **Step 1 — offline classifier + accuracy baseline** ✅
  Stage-0 rule cascade (`internal/classify`). Baseline on nl2bash + tldr +
  hand-built adversarials: **98.5% overall, 0% dangerous (command→NL) errors.**
- **Step 2 — widget + daemon + zero-latency passthrough** ✅
  `flowd` daemon classifies each line over a unix socket; the zsh widget
  (`shell/flow.zsh`) accepts commands immediately and degrades to plain zsh if
  the daemon is unavailable. Local round-trip ~70µs.
- **Step 3 — NL handling** ✅
  Self-built Anthropic client (`internal/llm`: raw HTTP/JSON + SSE, no SDK).
  Natural language is handed to the agent (mode B); the agent decides whether to
  run one command, do multi-step work, or answer a question. No API key →
  NL degrades to running the line as-is; the command path never touches the
  network.
- Step 4 — agent permission gate ✅
  The agent's per-tool-call gate enforces the review level: read-only tools run
  optimistically; side-effecting ones are gated (focused asks only on high-risk;
  strict asks on everything; yolo never asks). `command_not_found` is plain zsh.
- Mode B — self-built agent loop ✅
  The daemon does only instant CMD-vs-NL classification (zero latency, no
  network). A command runs as-is; natural language is handed to the agent. The
  agent (`cmd/flow-agent`, foreground TTY) runs **inline below your typed line**
  — translation is just one of its moves: it may run a single command, do
  multi-step work, or answer a question, streaming thinking + output after the
  prompt. Tool-use loop (bash/read_file/write_file/edit/grep) in the current
  directory, with a per-tool-call permission gate: reads are optimistic,
  side-effecting calls are gated by a review level (strict / focused / yolo;
  default focused asks only on high-risk: rm, git push, sudo, writes outside the
  tree, …). At an approval prompt: y=run, n=reject, a=allow-all-this-task,
  s=switch-to-strict. `FLOW_REVIEW` sets the level; `FLOW_AGENT_CMD` points the
  widget at the agent binary.

## Try it (UAT)

```sh
./shell/flow-uat
```

Builds flowd, starts a throwaway daemon (using your env or `~/.claude/settings.json`
provider config), and drops you into an interactive zsh with the widget loaded and
isolated history. Type a command (runs as usual) or natural language (translated to
the input line — press Enter to run, Esc Esc to restore your text). `exit` to quit;
the daemon and socket are torn down automatically.

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
- Provider config is resolved from the process env, then `~/.claude/settings.json`'s
  `env` block (same convention as Claude Code), so an existing setup just works:
  - `ANTHROPIC_BASE_URL` — endpoint root (default `https://api.anthropic.com`).
    Point it at any Anthropic-compatible proxy (GLM, DeepSeek, a gateway).
  - `ANTHROPIC_AUTH_TOKEN` — `Authorization: Bearer` token (compatible proxies).
  - `ANTHROPIC_API_KEY` — `x-api-key` (first-party API). Read from env, never logged.
  - `ANTHROPIC_MODEL` / `ANTHROPIC_SMALL_FAST_MODEL` — model names. If unset, flowd
    auto-discovers a fast model from the provider's `/v1/models`.
- `FLOW_SOCKET` — override the socket path (client and daemon must agree).
- `FLOW_TIMEOUT` — widget reply timeout in seconds (default 0.4); on timeout the
  widget degrades to plain accept-line.

Verified end-to-end against a live Anthropic-compatible proxy: Bearer auth,
model auto-discovery, NL→command translation, command path stays at 0ms.

## Test

```sh
go test ./...                  # classifier accuracy + daemon round-trip + latency
```
