#!/usr/bin/env bash
# wire-zshrc.sh — idempotently add the flow source + autostart block to ~/.zshrc.
# Invoked by `make install`. Reads FLOW_BIN and SHARE_DIR from the environment.
set -euo pipefail

ZSHRC="${ZDOTDIR:-$HOME}/.zshrc"
FLOW_BIN="${FLOW_BIN:?FLOW_BIN not set}"
SHARE_DIR="${SHARE_DIR:?SHARE_DIR not set}"

MARK_BEGIN="# >>> flow >>>"
MARK_END="# <<< flow <<<"

block() {
  cat <<EOF
$MARK_BEGIN
# flow: zsh smart input layer. Remove this block to uninstall the shell hook.
export PATH="$FLOW_BIN:\$PATH"
# Start the daemon once per machine (it self-exits if one is already running).
if [[ -z \${FLOW_NO_AUTOSTART:-} ]] && command -v flowd >/dev/null 2>&1; then
  (flowd >/dev/null 2>&1 &) 2>/dev/null
fi
[[ -f "$SHARE_DIR/flow.zsh" ]] && source "$SHARE_DIR/flow.zsh"
$MARK_END
EOF
}

touch "$ZSHRC"

if grep -qF "$MARK_BEGIN" "$ZSHRC"; then
  # Replace the existing block in place.
  tmp=$(mktemp)
  awk -v b="$MARK_BEGIN" -v e="$MARK_END" '
    $0==b {skip=1}
    skip==0 {print}
    $0==e {skip=0}
  ' "$ZSHRC" > "$tmp"
  block >> "$tmp"
  mv "$tmp" "$ZSHRC"
  echo "wire-zshrc: updated flow block in $ZSHRC"
else
  printf '\n%s\n' "$(block)" >> "$ZSHRC"
  echo "wire-zshrc: added flow block to $ZSHRC"
fi
