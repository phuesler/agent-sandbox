#!/bin/bash
set -euo pipefail
if [[ -n ${SBX_AGENT_UPDATE:-} ]] || ! command -v opencode >/dev/null; then
  curl -fsSL https://opencode.ai/install | bash -s -- --no-modify-path
  mkdir -p "$HOME/.local/bin"
  ln -sfn "$HOME/.opencode/bin/opencode" "$HOME/.local/bin/opencode"
fi
# Autonomous mode: the sandbox is the security boundary.
cfg="$HOME/.config/opencode/opencode.json"
if [[ ! -e $cfg ]]; then
  mkdir -p "$(dirname "$cfg")"
  cat > "$cfg" <<'JSON'
{
  "$schema": "https://opencode.ai/config.json",
  "permission": { "edit": "allow", "bash": "allow", "webfetch": "allow" }
}
JSON
fi
