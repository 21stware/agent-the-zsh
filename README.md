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

## Install

Requires **Go 1.25+** and **zsh** (with the `zsh/net/socket` module — standard on
macOS and most Linux zsh builds).

### From source (local machine)

```sh
make install            # builds, installs to ~/.local, wires ~/.zshrc + autostart
exec zsh                # or open a new terminal
```

`make install` puts `flowd` and `flow-agent` in `~/.local/bin`, the widget in
`~/.local/share/flow`, and adds a small block to your `~/.zshrc` that prepends
that bin dir to `PATH`, starts `flowd` once, and sources the widget. Override the
location with `make install PREFIX=/usr/local`.

### From a package (another machine)

```sh
make dist                                   # on a build machine → dist/flow-<os>-<arch>.tar.gz
# copy the tarball to the target, then:
tar xzf flow-<os>-<arch>.tar.gz
cd flow && ./install.sh                     # PREFIX=/usr/local ./install.sh to change location
exec zsh
```

The tarball is self-contained (binaries + widget + `flow-doctor` + installer).
Build it for the target's OS/arch (`make dist` builds for the host;
cross-compile with `GOOS=linux GOARCH=amd64 make dist`).

### Configure a provider

flow speaks the Anthropic Messages protocol; the endpoint can be the first-party
API or any compatible proxy (GLM, DeepSeek, a gateway). Config is read from the
process env first, then `~/.claude/settings.json`'s `env` block (same convention
as Claude Code), so an existing Claude Code setup just works. Set **one** of:

```sh
# compatible proxy (Bearer auth)
export ANTHROPIC_BASE_URL="https://your-proxy.example"
export ANTHROPIC_AUTH_TOKEN="sk-..."

# or first-party API (x-api-key)
export ANTHROPIC_API_KEY="sk-ant-..."
```

Optional: `ANTHROPIC_MODEL` / `ANTHROPIC_SMALL_FAST_MODEL` pin the model
(otherwise flow auto-discovers one from the provider's `/v1/models`). Credentials
are read from the environment and never logged.

Check everything resolved correctly (prints credential fingerprints, never the
secret, and probes the endpoint):

```sh
~/.local/share/flow/flow-doctor
```

### Uninstall

```sh
make uninstall          # removes binaries, share dir, and the ~/.zshrc block
pkill flowd             # stop a running daemon
```

## Using it

Open a zsh prompt and type as usual:

- a **shell command** (`git status`, `ls -la`) runs instantly, unchanged;
- **natural language** (`这个目录是什么项目`, `delete the build dir`) keeps your
  typed text on its line and runs the agent inline below it — it translates and
  runs one command, does multi-step work, or answers, streaming its thinking and
  output after the prompt;
- the configured model shows on the right of the prompt (disable with
  `FLOW_RPROMPT=0`);
- `flowclear` resets flow state.

Review level (how much the agent confirms before side-effecting actions):

```sh
export FLOW_REVIEW=focused   # default: ask only on high-risk (rm, git push, …)
export FLOW_REVIEW=strict    # ask before every side effect
export FLOW_REVIEW=yolo      # never ask
```

At an approval prompt: `y` run · `n` reject · `a` allow all (this task) · `s`
switch to strict.

## Try it without installing (UAT)

```sh
./shell/flow-uat
```

Builds the binaries, starts a throwaway daemon (using your env or
`~/.claude/settings.json`), and drops you into an interactive zsh with the widget
loaded and isolated history. `exit` tears it all down. Useful for trying changes
without touching your `~/.zshrc`.

## Architecture

```
zsh widget (thin client) ──unix socket, JSON line──▶ flowd (Go daemon)
  intercept Enter                                      classify CMD vs NL (offline)
  CMD: accept-line (0 latency, no network)             NL: action=agent
  NL: keep typed text, run flow-agent inline below     (no LLM call in the daemon)
  daemon down/slow: degrade to plain accept-line

flow-agent (foreground CLI, has the TTY)
  self-built Anthropic client (raw HTTP/JSON + SSE, no SDK)
  tool-use loop: bash / read_file / write_file / edit / grep, in the cwd
  per-tool permission gate keyed to FLOW_REVIEW
```

Design constraints (non-negotiable):
1. **Command path zero latency** — a CMD verdict accepts immediately; the daemon
   never touches the network for it.
2. **Bias to command** — when ambiguous, treat as a command. Misrouting a
   command to NL is worse than the reverse.
3. **Typed text preserved** — natural language is never silently rewritten into
   the input line; the agent runs below it.
4. **Fail to degrade** — if the daemon is missing/slow/erroring, the widget falls
   back to plain zsh. It never bricks the terminal.
5. **Side effects are gated** — the agent asks before destructive actions per the
   review level; reads run optimistically.

## Develop & test

```sh
make build     # bin/flowd + bin/flow-agent
make test      # go test ./...
make fmt vet   # format + vet
./shell/flow-uat   # interactive smoke test
```

