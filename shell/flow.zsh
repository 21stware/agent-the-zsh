# flow.zsh — the flow thin client: a ZLE widget that intercepts Enter, asks the
# flowd daemon whether the current line is a shell command or natural language,
# and acts on the verdict.
#
# Design constraints honored here (see project spec):
#   1. Zero latency for commands: the local unix-socket round-trip is sub-ms;
#      on a CMD verdict we accept-line immediately. Nothing touches the network.
#   4. Fail to degrade: if the daemon is missing, slow, or errors, we fall back
#      to a plain accept-line. flow never bricks the terminal. A non-zero
#      $FLOW_TIMEOUT bounds the worst case.
#
# Step 2 scope: the daemon always returns action=accept, so this widget is
# functionally a transparent passthrough — the user experiences a normal zsh.
# The action=replace branch is wired up now (against the final protocol) but the
# daemon does not emit it until step 3.
#
# Install: source this file from ~/.zshrc, after starting flowd.
#   source /path/to/flow.zsh

# --- configuration (override before sourcing) -------------------------------
: ${FLOW_SOCKET:="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}/flow-${UID}}/flow/flowd.sock"}
: ${FLOW_TIMEOUT:=0.4}      # seconds to wait for a daemon reply before degrading
: ${FLOW_HISTORY_LINES:=10} # recent history lines sent as context
: ${FLOW_PROTO:=1}

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

# _flow_query sends the request and reads one reply line. Prints the raw JSON
# reply on stdout. Returns non-zero on any failure (connect/write/read/timeout),
# which the caller treats as "degrade to accept-line".
_flow_query() {
  local sock=$(_flow_socket_path)
  [[ -S $sock ]] || return 1   # no socket -> daemon not running -> degrade

  local fd
  # zsocket sets REPLY to the fd on success.
  if ! zsocket "$sock" 2>/dev/null; then
    return 1
  fi
  fd=$REPLY

  local req=$(_flow_build_request)
  if ! print -u $fd -r -- "$req" 2>/dev/null; then
    exec {fd}>&- 2>/dev/null
    return 1
  fi

  # Read one line with a timeout. sysread -t bounds the wait so a hung daemon
  # cannot freeze the prompt (constraint 4). A single sysread may return a
  # partial read, so accumulate until we see a newline or the deadline passes.
  local reply="" chunk
  local deadline=$(( EPOCHREALTIME + FLOW_TIMEOUT ))
  while true; do
    local remain=$(( deadline - EPOCHREALTIME ))
    (( remain <= 0 )) && { exec {fd}>&- 2>/dev/null; return 1; }
    chunk=""
    if ! sysread -t $remain -i $fd chunk 2>/dev/null; then
      # rc!=0 is timeout or EOF. If we already have a full line, use it.
      break
    fi
    reply+=$chunk
    [[ $reply == *$'\n'* ]] && break
  done
  exec {fd}>&- 2>/dev/null

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

  local reply
  if ! reply=$(_flow_query); then
    # Degrade path: daemon unavailable/slow/error. Run the line as-is.
    zle .accept-line
    return
  fi

  local action=$(_flow_json_field "$reply" action)
  case $action in
    replace)
      # NL translated to a command. Replace buffer, keep cursor at end, do NOT
      # accept — the user reviews and presses Enter again (constraint 3).
      local text=$(_flow_json_string "$reply" text)
      if [[ -n $text ]]; then
        # Save the ORIGINAL input BEFORE overwriting, for Esc Esc undo (step 4).
        typeset -g FLOW_LAST_ORIGINAL=$BUFFER
        typeset -g FLOW_LAST_EFFECT=$(_flow_json_field "$reply" effect)
        BUFFER=$(_flow_json_unescape "$text")
        CURSOR=${#BUFFER}
        zle .reset-prompt 2>/dev/null
        return
      fi
      # No text despite replace: degrade.
      zle .accept-line
      ;;
    accept|*)
      # CMD verdict, or anything unexpected: accept-line (zero latency / safe).
      zle .accept-line
      ;;
  esac
}

zle -N flow-accept-line
bindkey '^M' flow-accept-line   # Enter / Return
bindkey '^J' flow-accept-line   # Ctrl-J / line feed
