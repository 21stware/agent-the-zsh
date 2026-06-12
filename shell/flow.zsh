# flow.zsh — the flow thin client: a ZLE widget that intercepts Enter, asks the
# flowd daemon whether the current line is a shell command or natural language,
# and acts on the verdict.
#
# Design constraints honored here (see project spec):
#   1. Zero latency for commands: the local unix-socket round-trip is sub-ms;
#      on a CMD verdict we accept-line immediately. Nothing touches the network.
#   3. Reversible: a translated NL command is written to the buffer and waits at
#      end-of-line — never auto-run. Esc Esc restores the original input.
#   4. Fail to degrade: if the daemon is missing, slow, or errors, we fall back
#      to a plain accept-line. flow never bricks the terminal. A non-zero
#      $FLOW_TIMEOUT bounds the worst case.
#
# NL verdicts are translated to one shell command (action=replace) and written
# back to the buffer. Side-effecting translations are tagged with a ⚠ marker
# below the line (visual only; Enter still runs it — per the "mark, don't block"
# choice). Esc Esc restores the original natural-language input.
#
# Install: source this file from ~/.zshrc, after starting flowd.
#   source /path/to/flow.zsh

# --- configuration (override before sourcing) -------------------------------
: ${FLOW_SOCKET:="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}/flow-${UID}}/flow/flowd.sock"}
: ${FLOW_TIMEOUT:=0.4}            # phase-1 read: bounds the command path + dead daemon
: ${FLOW_TRANSLATE_TIMEOUT:=12}   # phase-2 read: how long to wait for NL translation
: ${FLOW_HISTORY_LINES:=10} # recent history lines sent as context
: ${FLOW_PROTO:=1}
: ${FLOW_REVIEW:=focused}   # strict | focused | yolo — how much to confirm before running
: ${FLOW_DEBUG:=0}          # 1 = append per-interaction trace to $FLOW_DEBUG_LOG
: ${FLOW_DEBUG_LOG:="${TMPDIR:-/tmp}/flow-widget.log"}
: ${FLOW_MARK_READONLY:="✓ flow"}        # POSTDISPLAY tag for a read-only translation
: ${FLOW_MARK_SIDEEFFECT:="⚠ flow: side-effect — review before Enter"}
: ${FLOW_MARK_PENDING:="…"}

# Spinner frames and rotating status words shown while the daemon works. Purely
# cosmetic — the words rotate to suggest what flow is doing.
FLOW_SPIN_FRAMES=("⠋" "⠙" "⠹" "⠸" "⠼" "⠴" "⠦" "⠧" "⠇" "⠏")
FLOW_SPIN_WORDS=("routing" "thinking" "clarifying" "scaffolding" "bypassing")

zmodload zsh/net/socket 2>/dev/null || return 0  # no socket module -> stay plain
zmodload zsh/system 2>/dev/null || return 0
zmodload zsh/datetime 2>/dev/null || return 0    # for EPOCHREALTIME read deadline

# _flow_dbg appends a timestamped line to the debug log when FLOW_DEBUG=1. Used
# to trace a UAT session without guessing — set FLOW_DEBUG=1 before sourcing.
_flow_dbg() {
  [[ $FLOW_DEBUG == 1 ]] || return 0
  print -r -- "$(strftime '%H:%M:%S' $EPOCHSECONDS) $*" >> "$FLOW_DEBUG_LOG" 2>/dev/null
}

# _flow_socket_path resolves the socket path, matching daemon.SocketPath():
# FLOW_SOCKET wins; else XDG_RUNTIME_DIR/flow; else TMPDIR/flow-<uid>/flow.
_flow_socket_path() {
  if [[ -n $FLOW_SOCKET ]]; then
    print -r -- "$FLOW_SOCKET"
    return
  fi
  if [[ -n $XDG_RUNTIME_DIR ]]; then
    print -r -- "$XDG_RUNTIME_DIR/flow/flowd.sock"
  else
    print -r -- "${TMPDIR:-/tmp}/flow-${UID}/flowd.sock"
  fi
}

# _flow_json_escape escapes a string for embedding in a JSON string literal.
# Handles backslash, double-quote, and control chars (newline/tab/cr).
_flow_json_escape() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}
  s=${s//$'\t'/\\t}
  s=${s//$'\r'/\\r}
  print -r -- "$s"
}

# _flow_build_request emits the JSON line request for the current buffer.
_flow_build_request() {
  local buf=$(_flow_json_escape "$BUFFER")
  local cwd=$(_flow_json_escape "$PWD")
  # recent history, newest FLOW_HISTORY_LINES, as a JSON array
  local -a hist
  local i line
  for i in {1..$FLOW_HISTORY_LINES}; do
    line=$(fc -ln -$i -$i 2>/dev/null) || continue
    [[ -z $line ]] && continue
    hist+=("\"$(_flow_json_escape "$line")\"")
  done
  local histjson="${(j:,:)hist}"
  print -r -- "{\"buffer\":\"$buf\",\"cwd\":\"$cwd\",\"history\":[$histjson],\"proto\":$FLOW_PROTO}"
}

# _flow_clear_session tells the daemon to reset the NL conversation context.
# Best-effort: silently does nothing if the daemon is unreachable.
_flow_clear_session() {
  local sock=$(_flow_socket_path)
  [[ -S $sock ]] || return 0
  zsocket "$sock" 2>/dev/null || return 0
  local fd=$REPLY
  print -u $fd -r -- "{\"clear\":true,\"proto\":$FLOW_PROTO}" 2>/dev/null
  # read+discard the ack with a short timeout, then close
  local junk
  sysread -t 0.4 -i $fd junk 2>/dev/null
  exec {fd}>&- 2>/dev/null
}

# _flow_open connects to the daemon and sends the request for the current
# buffer. On success it sets the global FLOW_FD to the open socket fd and
# returns 0. On any failure (no socket, connect, write) it returns non-zero —
# the caller degrades to plain accept-line.
_flow_open() {
  local sock=$(_flow_socket_path)
  [[ -S $sock ]] || return 1   # no socket -> daemon not running -> degrade

  zsocket "$sock" 2>/dev/null || return 1
  typeset -g FLOW_FD=$REPLY

  local req=$(_flow_build_request)
  if ! print -u $FLOW_FD -r -- "$req" 2>/dev/null; then
    _flow_close
    return 1
  fi
  return 0
}

# _flow_close closes the daemon socket fd, if open.
_flow_close() {
  [[ -n $FLOW_FD ]] && exec {FLOW_FD}>&- 2>/dev/null
  unset FLOW_FD
}

# _flow_read_line reads one newline-terminated JSON reply from FLOW_FD, waiting
# at most $1 seconds. Prints the line (without newline) on success; returns
# non-zero on timeout/EOF/error. A single sysread may return a partial read, so
# it accumulates until a newline arrives or the deadline passes.
_flow_read_line() {
  local budget=$1
  local reply="" chunk
  local deadline=$(( EPOCHREALTIME + budget ))
  while true; do
    local remain=$(( deadline - EPOCHREALTIME ))
    (( remain <= 0 )) && return 1
    chunk=""
    if ! sysread -t $remain -i $FLOW_FD chunk 2>/dev/null; then
      break   # timeout or EOF
    fi
    reply+=$chunk
    [[ $reply == *$'\n'* ]] && break
  done
  reply=${reply%%$'\n'*}
  [[ -z $reply ]] && return 1
  print -r -- "$reply"
}

# _flow_read_line_animated reads a reply like _flow_read_line, but while waiting
# it animates an in-line spinner + rotating status word via POSTDISPLAY. zsh is
# single-threaded inside a widget, so instead of one long blocking read we poll
# in short slices: each slice does a brief sysread; on a timeout slice we advance
# one animation frame and redraw with `zle -R`. Net effect is a live animation
# without any background process.
_flow_read_line_animated() {
  local budget=$1
  local reply="" chunk
  local deadline=$(( EPOCHREALTIME + budget ))
  local frame=0 word=0 slice=0
  while true; do
    local remain=$(( deadline - EPOCHREALTIME ))
    (( remain <= 0 )) && break
    # Cap each read slice so the animation advances ~8x/sec.
    local step=0.12
    (( remain < step )) && step=$remain
    chunk=""
    if sysread -t $step -i $FLOW_FD chunk 2>/dev/null; then
      reply+=$chunk
      [[ $reply == *$'\n'* ]] && break
      continue
    fi
    # Timeout slice (no data yet): advance the animation.
    local f=${FLOW_SPIN_FRAMES[$(( frame % ${#FLOW_SPIN_FRAMES[@]} + 1 ))]}
    # rotate the status word roughly every ~1s (8 frames)
    (( slice++ ))
    (( slice % 8 == 0 )) && (( word++ ))
    local w=${FLOW_SPIN_WORDS[$(( word % ${#FLOW_SPIN_WORDS[@]} + 1 ))]}
    POSTDISPLAY=$'\n'"$f $w…"
    typeset -g FLOW_MARK_ACTIVE=1
    zle -R 2>/dev/null
    (( frame++ ))
  done
  reply=${reply%%$'\n'*}
  [[ -z $reply ]] && return 1
  print -r -- "$reply"
}
# object. Good enough for our small, daemon-produced replies (no nesting). For
# string fields it returns the unescaped-ish value; for our purposes the values
# (accept/replace/CMD/NL) contain no escapes.
_flow_json_field() {
  local json=$1 key=$2
  # match "key":"value"  or  "key":value
  if [[ $json =~ "\"$key\"[[:space:]]*:[[:space:]]*\"([^\"]*)\"" ]]; then
    print -r -- "$match[1]"
  elif [[ $json =~ "\"$key\"[[:space:]]*:[[:space:]]*([^,}\"]+)" ]]; then
    print -r -- "$match[1]"
  fi
}

# _flow_json_string extracts a string field's RAW (still JSON-escaped) value,
# correctly handling embedded \" and \\ — unlike _flow_json_field, which stops
# at the first quote. Used for the "text" field (a shell command that may
# contain quotes). Returns the escaped value; pass it through _flow_json_unescape.
_flow_json_string() {
  local json=$1 key=$2
  local marker="\"$key\":\""
  local start=${json[(i)$marker]}
  (( start > ${#json} )) && return 1
  local i=$(( start + ${#marker} ))
  local out="" ch
  while (( i <= ${#json} )); do
    ch=${json[i]}
    if [[ $ch == "\\" ]]; then
      # keep the backslash and the next char verbatim (still escaped)
      out+=${json[i,i+1]}
      (( i += 2 ))
      continue
    fi
    [[ $ch == "\"" ]] && break   # unescaped quote ends the value
    out+=$ch
    (( i++ ))
  done
  print -r -- "$out"
}

# _flow_json_unescape turns JSON string escapes back into literal characters.
_flow_json_unescape() {
  local s=$1
  s=${s//\\n/$'\n'}
  s=${s//\\t/$'\t'}
  s=${s//\\r/$'\r'}
  s=${s//\\\"/\"}
  s=${s//\\\\/\\}
  print -r -- "$s"
}

# flow-accept-line is the bound widget. It replaces the default accept-line.
flow-accept-line() {
  # Empty buffer: behave exactly like accept-line.
  if [[ -z ${BUFFER// /} ]]; then
    zle .accept-line
    return
  fi

  # `flowclear`: reset the daemon's NL conversation context, then clear the line.
  if [[ ${BUFFER// /} == flowclear ]]; then
    _flow_clear_session
    _flow_clear_mark
    BUFFER=""
    POSTDISPLAY=$'\n'"flow: conversation cleared"
    typeset -g FLOW_MARK_ACTIVE=1
    zle .reset-prompt 2>/dev/null
    return
  fi

  # Clear any lingering translation marker before deciding this line.
  _flow_clear_mark
  _flow_dbg "accept-line: buffer=[$BUFFER]"

  # Open the connection and send the request.
  if ! _flow_open; then
    # Daemon unavailable: run the line as-is (degrade, never brick).
    _flow_dbg "  open FAILED -> degrade to accept-line (daemon down?)"
    zle .accept-line
    return
  fi

  # Phase 1: read the first reply within the short timeout. This bounds both the
  # zero-latency command path and a dead/slow daemon. CMD comes back in
  # microseconds; NL comes back as "pending" almost as fast.
  local reply
  if ! reply=$(_flow_read_line "$FLOW_TIMEOUT"); then
    _flow_dbg "  phase1 TIMEOUT/err (>${FLOW_TIMEOUT}s) -> degrade to accept-line"
    _flow_close
    zle .accept-line
    return
  fi

  local action=$(_flow_json_field "$reply" action)
  _flow_dbg "  phase1 reply: $reply"

  if [[ $action == pending ]]; then
    # NL needs routing/translation (2-6s with the capable model). Immediately
    # replace the typed text with the thinking animation so the moment the user
    # presses Enter they see motion, not a frozen line. Save the original to
    # restore if routing can't produce a command.
    typeset -g FLOW_PENDING_ORIGINAL=$BUFFER
    BUFFER=""
    CURSOR=0
    zle .reset-prompt 2>/dev/null
    if ! reply=$(_flow_read_line_animated "$FLOW_TRANSLATE_TIMEOUT"); then
      # Routing timed out: restore the original line and run it as-is.
      _flow_dbg "  phase2 TIMEOUT/err (>${FLOW_TRANSLATE_TIMEOUT}s) -> accept original"
      _flow_close
      _flow_clear_mark
      BUFFER=$FLOW_PENDING_ORIGINAL
      CURSOR=${#BUFFER}
      unset FLOW_PENDING_ORIGINAL
      zle .reset-prompt 2>/dev/null
      zle .accept-line
      return
    fi
    action=$(_flow_json_field "$reply" action)
    _flow_dbg "  phase2 reply: $reply"
  fi

  _flow_close
  _flow_apply_reply "$action" "$reply"
}

# _flow_apply_reply acts on a final daemon reply: replace the buffer (NL→command)
# or accept the line (CMD / degrade). Shared by the phase-1 and phase-2 paths.
_flow_apply_reply() {
  local action=$1 reply=$2
  # The original typed input: if we animated (cleared the line during pending),
  # it's stashed in FLOW_PENDING_ORIGINAL; otherwise it's still in BUFFER.
  local orig=${FLOW_PENDING_ORIGINAL-$BUFFER}
  unset FLOW_PENDING_ORIGINAL
  case $action in
    agent)
      # Route to mode B. Run flow-agent cleanly via a precmd hook instead of
      # stuffing a command into the buffer: this avoids echoing the launcher
      # path and the confusing "line gets replaced then executed" effect. We
      # clear the input line, accept it (empty), and a one-shot precmd runs the
      # agent in the foreground before the next prompt is drawn.
      local task=$(_flow_json_unescape "$(_flow_json_string "$reply" text)")
      [[ -z $task ]] && task=$orig
      typeset -g FLOW_LAST_ORIGINAL=$orig
      typeset -g FLOW_PENDING_AGENT_TASK=$task
      _flow_clear_mark
      BUFFER=""
      zle .reset-prompt 2>/dev/null
      zle .accept-line
      ;;
    replace)
      local text=$(_flow_json_string "$reply" text)
      if [[ -z $text ]]; then
        _flow_clear_mark
        BUFFER=$orig
        CURSOR=${#BUFFER}
        zle .reset-prompt 2>/dev/null
        zle .accept-line
        return
      fi
      local effect=$(_flow_json_field "$reply" effect)
      local cmd=$(_flow_json_unescape "$text")
      typeset -g FLOW_LAST_ORIGINAL=$orig
      typeset -g FLOW_LAST_EFFECT=$effect

      # Three-tier review for translated commands (mirrors the agent's gate):
      #   yolo    -> run immediately, no confirmation
      #   focused -> run read-only immediately; side-effects wait for Enter
      #   strict  -> everything waits for Enter
      local run_now=0
      case $FLOW_REVIEW in
        yolo) run_now=1 ;;
        strict) run_now=0 ;;
        *) [[ $effect == read-only ]] && run_now=1 ;;   # focused (default)
      esac

      _flow_clear_mark
      BUFFER=$cmd
      CURSOR=${#BUFFER}
      if (( run_now )); then
        # Auto-run: no second confirmation under this review level.
        zle .reset-prompt 2>/dev/null
        zle .accept-line
        return
      fi
      # Wait for the user to review and press Enter. Tag with the effect.
      typeset -g FLOW_LAST_TRANSLATED=$BUFFER
      if [[ $effect == side-effect ]]; then
        POSTDISPLAY=$'\n'"$FLOW_MARK_SIDEEFFECT"
      else
        POSTDISPLAY=$'\n'"$FLOW_MARK_READONLY"
      fi
      typeset -g FLOW_MARK_ACTIVE=1
      zle .reset-prompt 2>/dev/null
      ;;
    accept|*)
      # CMD verdict, untranslatable NL, or anything unexpected: run as-is.
      # Restore the original input (cleared during the animation) and run it.
      _flow_clear_mark
      BUFFER=$orig
      CURSOR=${#BUFFER}
      zle .reset-prompt 2>/dev/null
      zle .accept-line
      ;;
  esac
}

# _flow_run_pending_agent is a precmd hook: if a mode-B task was queued, run
# flow-agent in the foreground (clean TTY, no echoed command), then clear the
# flag so it runs exactly once.
_flow_run_pending_agent() {
  [[ -z $FLOW_PENDING_AGENT_TASK ]] && return
  local task=$FLOW_PENDING_AGENT_TASK
  unset FLOW_PENDING_AGENT_TASK
  FLOW_TASK=$task "${FLOW_AGENT_CMD:-flow-agent}"
}

# _flow_clear_mark removes the POSTDISPLAY tag and translation bookkeeping once
# the user starts editing, accepts, or undoes the line.
_flow_clear_mark() {
  if [[ -n $FLOW_MARK_ACTIVE ]]; then
    POSTDISPLAY=""
    unset FLOW_MARK_ACTIVE FLOW_LAST_TRANSLATED
  fi
}

# flow-undo restores the original natural-language input after a translation.
# Bound to Esc Esc (constraint 3: one-keystroke recovery of the original).
flow-undo() {
  if [[ -n ${FLOW_LAST_ORIGINAL+x} ]]; then
    BUFFER=$FLOW_LAST_ORIGINAL
    CURSOR=${#BUFFER}
    unset FLOW_LAST_ORIGINAL FLOW_LAST_EFFECT
    _flow_clear_mark
    zle .reset-prompt 2>/dev/null
  fi
}

# _flow_line_pre_redraw drops the marker as soon as the user edits the line away
# from the translated text. Registered as a zle-line-pre-redraw hook so it runs
# on every redraw without wrapping individual editing widgets.
_flow_line_pre_redraw() {
  if [[ -n $FLOW_MARK_ACTIVE && $BUFFER != $FLOW_LAST_TRANSLATED ]]; then
    _flow_clear_mark
  fi
}

zle -N flow-accept-line
zle -N flow-undo
zle -N zle-line-pre-redraw _flow_line_pre_redraw
bindkey '^M' flow-accept-line   # Enter / Return
bindkey '^J' flow-accept-line   # Ctrl-J / line feed
bindkey '\e\e' flow-undo        # Esc Esc — restore original NL input

# Run a queued mode-B task before each prompt (clean foreground, no echoed cmd).
autoload -Uz add-zsh-hook 2>/dev/null && add-zsh-hook precmd _flow_run_pending_agent
