#!/usr/bin/env bash
# install.sh — installer shipped inside the flow tarball. Run after unpacking:
#   tar xzf flow-<os>-<arch>.tar.gz && cd flow && ./install.sh
#
# Installs flowd + flow-agent to a bin dir, the widget to a share dir, and wires
# ~/.zshrc to start the daemon and load the widget. Override the prefix:
#   PREFIX=/usr/local ./install.sh
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
PREFIX="${PREFIX:-$HOME/.local}"
BINDIR="$PREFIX/bin"
SHARE_DIR="$PREFIX/share/flow"
ZSHRC="${ZDOTDIR:-$HOME}/.zshrc"
MARK_BEGIN="# >>> flow >>>"
MARK_END="# <<< flow <<<"

echo "flow: installing to $PREFIX"
mkdir -p "$BINDIR" "$SHARE_DIR"
install -m 0755 "$here/flowd"      "$BINDIR/flowd"
install -m 0755 "$here/flow-agent" "$BINDIR/flow-agent"
install -m 0644 "$here/flow.zsh"   "$SHARE_DIR/flow.zsh"
install -m 0755 "$here/flow-doctor" "$SHARE_DIR/flow-doctor"

block() {
  cat <<EOF
$MARK_BEGIN
# flow: zsh smart input layer. Remove this block to uninstall the shell hook.
export PATH="$BINDIR:\$PATH"
if [[ -z \${FLOW_NO_AUTOSTART:-} ]] && command -v flowd >/dev/null 2>&1; then
  (flowd >/dev/null 2>&1 &) 2>/dev/null
fi
[[ -f "$SHARE_DIR/flow.zsh" ]] && source "$SHARE_DIR/flow.zsh"
$MARK_END
EOF
}

touch "$ZSHRC"
if grep -qF "$MARK_BEGIN" "$ZSHRC"; then
  tmp=$(mktemp)
  awk -v b="$MARK_BEGIN" -v e="$MARK_END" '$0==b{skip=1} skip==0{print} $0==e{skip=0}' "$ZSHRC" > "$tmp"
  block >> "$tmp"; mv "$tmp" "$ZSHRC"
  echo "flow: updated the block in $ZSHRC"
else
  printf '\n%s\n' "$(block)" >> "$ZSHRC"
  echo "flow: added a block to $ZSHRC"
fi

echo ""
echo "Done. Configure a provider (one of):"
echo "  • export ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL   (compatible proxy)"
echo "  • export ANTHROPIC_API_KEY                            (first-party)"
echo "  • or set them in ~/.claude/settings.json's env block"
echo "Then open a new shell. Check status with: $SHARE_DIR/flow-doctor"
