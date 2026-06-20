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
# FLOW_SOCKET, if set, is an explicit override; otherwise _flow_socket_path
# derives it to match the daemon's daemon.SocketPath() (see below). Do NOT
# pre-seed a default here — an incorrect default would shadow that logic.
: ${FLOW_TIMEOUT:=0.4}      # daemon-reply read timeout; bounds command path + dead daemon
: ${FLOW_HISTORY_LINES:=10} # recent history lines sent as context
: ${FLOW_PROTO:=1}
: ${FLOW_REVIEW:=focused}   # strict | focused | yolo — passed to the agent's permission gate
: ${FLOW_DEBUG:=0}          # 1 = append per-interaction trace to $FLOW_DEBUG_LOG
: ${FLOW_DEBUG_LOG:="${TMPDIR:-/tmp}/flow-widget.log"}
: ${FLOW_RPROMPT:=1}        # 1 = show the flow model on the right of the prompt

zmodload zsh/net/socket 2>/dev/null || return 0  # no socket module -> stay plain
zmodload zsh/system 2>/dev/null || return 0
zmodload zsh/datetime 2>/dev/null || return 0    # for EPOCHREALTIME read deadline

# --- session identity -------------------------------------------------------
# A flow session is one shell. FLOW_SESSION_ID keys the conversation transcript
# (see internal/session). It is generated once and exported so it survives
# `exec zsh`; a new terminal/tab gets a fresh id (a new conversation). The
# zshrc flow block sets it first when wired; this is the fallback when flow.zsh
# is sourced directly.
_flow_gen_session_id() {
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr 'A-Z' 'a-z'
  else
    print -r -- "${EPOCHSECONDS:-$(date +%s)}-$$-${RANDOM}${RANDOM}"
  fi
}
: ${FLOW_SESSION_ID:=$(_flow_gen_session_id)}
export FLOW_SESSION_ID

# _flow_session_file resolves the JSONL transcript path for this shell, matching
# session.Path() in Go: $XDG_RUNTIME_DIR/flow/sessions or $TMPDIR/flow-<uid>/sessions.
_flow_session_file() {
  [[ -n $FLOW_SESSION_ID ]] || return 1
  local base
  if [[ -n $XDG_RUNTIME_DIR ]]; then
    base="$XDG_RUNTIME_DIR/flow/sessions"
  else
    base="${TMPDIR:-/tmp}/flow-${UID}/sessions"
  fi
  print -r -- "$base/${FLOW_SESSION_ID}.jsonl"
}

# flowtmp: make a fresh temp dir and cd into it. Defined as a function (not a
# child process) because it must change THIS shell's cwd.
flowtmp() {
  local d
  d=$(mktemp -d "${TMPDIR:-/tmp}/flow-tmp.XXXXXX") || {
    print -u2 -r -- "flowtmp: mktemp failed"
    return 1
  }
  cd "$d" || return 1
  print -r -- "flow: cwd -> $d"
}

# _flow_sessions_dir resolves the sessions directory (matches session.Path() in Go).
_flow_sessions_dir() {
  if [[ -n $XDG_RUNTIME_DIR ]]; then
    print -r -- "$XDG_RUNTIME_DIR/flow/sessions"
  else
    print -r -- "${TMPDIR:-/tmp}/flow-${UID}/sessions"
  fi
}

# flowrsm: continue a PREVIOUS window's conversation in this one. With no
# argument it shows an interactive arrow-key picker (each row: directory + the
# last message of that conversation); with an id prefix it resumes that session
# directly. The chosen transcript is merged into THIS session's file, so the
# default per-window replay then continues it. A function (not a child process)
# because it appends to this session's file.
flowrsm() {
  local cur src picked
  cur=$(_flow_session_file)

  if [[ -n $1 ]]; then
    # Resume by id prefix: newest matching transcript that isn't this session.
    local dir=$(_flow_sessions_dir)
    src=$(print -rl -- "$dir"/${1}*.jsonl(Nom) 2>/dev/null | grep -v -F "$cur" | head -1)
    [[ -z $src || ! -s $src ]] && { print -r -- "flow: no session matching '$1'."; return 1; }
  else
    # Interactive picker (drawn on the TTY); prints the chosen id on stdout.
    picked=$(command flow-agent --resume-picker) || return 1
    [[ -z $picked ]] && return 1
    src=$(_flow_sessions_dir)/${picked}.jsonl
    [[ -s $src ]] || { print -r -- "flow: session '$picked' is empty."; return 1; }
  fi

  cat "$src" >> "$cur" 2>/dev/null
  local n=$(wc -l < "$src" 2>/dev/null | tr -d ' ')
  print -r -- "flow: resumed ${n} turn(s) from $(basename ${src%.jsonl}) — continue here."
}

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
  # NOTE: no `2>/dev/null` here. `exec {fd}>&-` with no command applies its
  # redirections to the CURRENT shell permanently, so a trailing `2>/dev/null`
  # would silently send the shell's stderr to /dev/null forever — swallowing
  # every later command's errors. The fd is always valid here, so just close it.
  exec {fd}>&-
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
  [[ -n $FLOW_FD ]] && exec {FLOW_FD}>&-
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
# (accept/agent/CMD/NL) contain no escapes.
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

  # `flowclear`: reset the conversation. Clears the daemon's (legacy) context
  # and truncates this session's transcript so the next agent turn starts fresh.
  if [[ ${BUFFER// /} == flowclear ]]; then
    _flow_clear_session
    local f=$(_flow_session_file)
    [[ -n $f && -f $f ]] && : > "$f"
    # Also clear the persisted review-level override (e.g. yolo from pressing 'a').
    local lvl
    if [[ -n $XDG_RUNTIME_DIR ]]; then
      lvl="$XDG_RUNTIME_DIR/flow/sessions/${FLOW_SESSION_ID}.level"
    else
      lvl="${TMPDIR:-/tmp}/flow-${UID}/sessions/${FLOW_SESSION_ID}.level"
    fi
    [[ -f $lvl ]] && rm -f "$lvl"
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

  # Read the daemon's verdict within the short timeout. This bounds both the
  # zero-latency command path and a dead/slow daemon. The daemon replies in
  # microseconds (offline CMD-vs-NL classification): accept, agent, or (on a
  # bad request) accept.
  local reply
  if ! reply=$(_flow_read_line "$FLOW_TIMEOUT"); then
    _flow_dbg "  read TIMEOUT/err (>${FLOW_TIMEOUT}s) -> degrade to accept-line"
    _flow_close
    zle .accept-line
    return
  fi
  _flow_close

  local action=$(_flow_json_field "$reply" action)
  _flow_dbg "  reply: $reply"
  _flow_apply_reply "$action" "$reply"
}

# _flow_apply_reply acts on the daemon's verdict: run a command as-is (CMD /
# degrade), or hand a natural-language request to the agent (NL).
_flow_apply_reply() {
  local action=$1 reply=$2
  case $action in
    agent)
      # NL -> mode B. Keep the user's typed text where they typed it, then run
      # flow-agent inline BELOW it (we have the TTY inside the widget). No
      # accept-line, no echoed launcher path: a newline commits the
      # "PROMPT + typed text" line into scrollback, the agent prints its
      # animation/output beneath, then we reset to a fresh prompt.
      local task=$BUFFER
      _flow_clear_mark
      # Commit the typed line into scrollback, then hand off to the agent. zle -I
      # lets us write to the terminal from within the widget safely. FLOW_REVIEW
      # is passed through so the agent's permission gate uses the chosen level.
      zle -I
      FLOW_TASK=$task FLOW_REVIEW=$FLOW_REVIEW FLOW_MODEL=$FLOW_MODEL "${FLOW_AGENT_CMD:-flow-agent}"
      # Done: clear the buffer and redraw a clean prompt.
      BUFFER=""
      CURSOR=0
      zle .reset-prompt 2>/dev/null
      ;;
    accept|*)
      # CMD verdict (or anything unexpected): run the typed line as-is.
      _flow_clear_mark
      zle .reset-prompt 2>/dev/null
      zle .accept-line
      ;;
  esac
}

# _flow_clear_mark removes a transient POSTDISPLAY note (e.g. the flowclear
# confirmation) on the next line edit/submit.
_flow_clear_mark() {
  if [[ -n $FLOW_MARK_ACTIVE ]]; then
    POSTDISPLAY=""
    unset FLOW_MARK_ACTIVE
  fi
}

zle -N flow-accept-line
bindkey '^M' flow-accept-line   # Enter / Return
bindkey '^J' flow-accept-line   # Ctrl-J / line feed

# _flow_query_info asks the daemon for status (model name, agent enabled) once.
# Prints the model name on success; empty on any failure.
_flow_query_info() {
  local sock=$(_flow_socket_path)
  [[ -S $sock ]] || return 1
  zsocket "$sock" 2>/dev/null || return 1
  local fd=$REPLY
  print -u $fd -r -- "{\"info\":true,\"proto\":$FLOW_PROTO}" 2>/dev/null || { exec {fd}>&-; return 1; }
  local reply="" chunk
  if sysread -t 0.5 -i $fd reply 2>/dev/null; then :; fi
  exec {fd}>&-
  reply=${reply%%$'\n'*}
  local model=$(_flow_json_field "$reply" model)
  [[ -n $model ]] && print -r -- "$model"
}

# _flow_setup_rprompt appends the flow model to RPROMPT (right side), leaving the
# user's left PROMPT (e.g. an oh-my-zsh theme) untouched. It retries on each
# prompt until the daemon reports a model (discovery may be async), then stops.
# Set FLOW_RPROMPT=0 to disable.
typeset -g FLOW_RPROMPT_DONE=
typeset -g FLOW_MODEL=
_flow_setup_rprompt() {
  [[ $FLOW_RPROMPT == 1 ]] || return 0
  [[ -n $FLOW_RPROMPT_DONE ]] && return 0
  local model=$(_flow_query_info 2>/dev/null)
  [[ -z $model ]] && return 0
  FLOW_RPROMPT_DONE=1
  FLOW_MODEL=$model
  local seg="%F{244}(${model})%f"
  if [[ -n $RPROMPT && $RPROMPT != *"$seg"* ]]; then
    RPROMPT="$RPROMPT $seg"
  elif [[ -z $RPROMPT ]]; then
    RPROMPT="$seg"
  fi
}
autoload -Uz add-zsh-hook 2>/dev/null && add-zsh-hook precmd _flow_setup_rprompt
_flow_setup_rprompt   # try immediately too

# --- command_not_found_handler -----------------------------------------------
# When the classifier says CMD but the command doesn't actually exist (e.g.
# "delete xyz.md" where "delete" is not a known command, or pinyin like
# "shanchu"), re-route the original input to the agent as a fallback. This
# catches NL input that was misclassified as CMD by the stage-0 classifier.
# Returns 127 (standard "not found") when flow is not active so non-flow
# shells are unaffected.
command_not_found_handler() {
  [[ -n $FLOW_SESSION_ID ]] || return 127
  local sock=$(_flow_socket_path)
  [[ -S $sock ]] || return 127

  local line="${(j: :)@}"
  [[ -z $line ]] && return 127

  print -r -- "flow: '$1' not found — routing to agent"
  FLOW_TASK=$line FLOW_REVIEW=$FLOW_REVIEW FLOW_MODEL=$FLOW_MODEL "${FLOW_AGENT_CMD:-flow-agent}"
  return $?
}
