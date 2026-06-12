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
: ${FLOW_MARK_READONLY:="✓ flow"}        # POSTDISPLAY tag for a read-only translation
: ${FLOW_MARK_SIDEEFFECT:="⚠ flow: side-effect — review before Enter"}
: ${FLOW_MARK_PENDING:="⋯ flow: translating…"}

zmodload zsh/net/socket 2>/dev/null || return 0  # no socket module -> stay plain
zmodload zsh/system 2>/dev/null || return 0
zmodload zsh/datetime 2>/dev/null || return 0    # for EPOCHREALTIME read deadline

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

# _flow_json_field extracts a top-level string/scalar field from a flat JSON
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

  # Clear any lingering translation marker before deciding this line.
  _flow_clear_mark

  # Open the connection and send the request.
  if ! _flow_open; then
    # Daemon unavailable: run the line as-is (degrade, never brick).
    zle .accept-line
    return
  fi

  # Phase 1: read the first reply within the short timeout. This bounds both the
  # zero-latency command path and a dead/slow daemon. CMD comes back in
  # microseconds; NL comes back as "pending" almost as fast.
  local reply
  if ! reply=$(_flow_read_line "$FLOW_TIMEOUT"); then
    _flow_close
    zle .accept-line
    return
  fi

  local action=$(_flow_json_field "$reply" action)

  if [[ $action == pending ]]; then
    # NL is being translated. Show progress, then wait (longer) for phase 2.
    POSTDISPLAY=$'\n'"$FLOW_MARK_PENDING"
    typeset -g FLOW_MARK_ACTIVE=1
    zle -R   # force a redraw so the user sees "translating…" while we wait
    if ! reply=$(_flow_read_line "$FLOW_TRANSLATE_TIMEOUT"); then
      # Translation timed out: drop the marker and accept the original line.
      _flow_close
      _flow_clear_mark
      zle .reset-prompt 2>/dev/null
      zle .accept-line
      return
    fi
    action=$(_flow_json_field "$reply" action)
  fi

  _flow_close
  _flow_apply_reply "$action" "$reply"
}

# _flow_apply_reply acts on a final daemon reply: replace the buffer (NL→command)
# or accept the line (CMD / degrade). Shared by the phase-1 and phase-2 paths.
_flow_apply_reply() {
  local action=$1 reply=$2
  case $action in
    replace)
      # NL translated to a command. Replace buffer, keep cursor at end, do NOT
      # accept — the user reviews and presses Enter again (constraint 3).
      local text=$(_flow_json_string "$reply" text)
      if [[ -n $text ]]; then
        # Save the ORIGINAL input BEFORE overwriting, for Esc Esc undo.
        typeset -g FLOW_LAST_ORIGINAL=$BUFFER
        typeset -g FLOW_LAST_EFFECT=$(_flow_json_field "$reply" effect)
        BUFFER=$(_flow_json_unescape "$text")
        CURSOR=${#BUFFER}
        typeset -g FLOW_LAST_TRANSLATED=$BUFFER
        # Mark the line with the effect (visual only — Enter still runs it).
        if [[ $FLOW_LAST_EFFECT == side-effect ]]; then
          POSTDISPLAY=$'\n'"$FLOW_MARK_SIDEEFFECT"
        else
          POSTDISPLAY=$'\n'"$FLOW_MARK_READONLY"
        fi
        typeset -g FLOW_MARK_ACTIVE=1
        zle .reset-prompt 2>/dev/null
        return
      fi
      # No text despite replace: degrade.
      _flow_clear_mark
      zle .reset-prompt 2>/dev/null
      zle .accept-line
      ;;
    accept|*)
      # CMD verdict, untranslatable NL, or anything unexpected: run as-is. Clear
      # any pending marker first.
      _flow_clear_mark
      zle .reset-prompt 2>/dev/null
      zle .accept-line
      ;;
  esac
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
