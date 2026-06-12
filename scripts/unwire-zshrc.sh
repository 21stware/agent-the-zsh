#!/usr/bin/env bash
# unwire-zshrc.sh — remove the flow block from ~/.zshrc (idempotent).
set -euo pipefail

ZSHRC="${ZDOTDIR:-$HOME}/.zshrc"
MARK_BEGIN="# >>> flow >>>"
MARK_END="# <<< flow <<<"

[[ -f "$ZSHRC" ]] || { echo "unwire-zshrc: no $ZSHRC"; exit 0; }
if ! grep -qF "$MARK_BEGIN" "$ZSHRC"; then
  echo "unwire-zshrc: no flow block in $ZSHRC"
  exit 0
fi

tmp=$(mktemp)
awk -v b="$MARK_BEGIN" -v e="$MARK_END" '
  $0==b {skip=1; next}
  $0==e {skip=0; next}
  skip==0 {print}
' "$ZSHRC" > "$tmp"
# Drop a possible blank line left where the block was.
mv "$tmp" "$ZSHRC"
echo "unwire-zshrc: removed flow block from $ZSHRC"
