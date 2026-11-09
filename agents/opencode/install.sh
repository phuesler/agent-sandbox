#!/bin/bash
set -euo pipefail
if [[ -n ${ABOX_AGENT_UPDATE:-} ]] || ! command -v opencode >/dev/null; then
  curl -fsSL https://opencode.ai/install | bash -s -- --no-modify-path
  mkdir -p "$HOME/.local/bin"
  ln -sfn "$HOME/.opencode/bin/opencode" "$HOME/.local/bin/opencode"
fi
# One config per abox permission mode, selected via OPENCODE_CONFIG (see agent.conf).
dir="$HOME/.config/opencode"
mkdir -p "$dir"
write() { # mode edit bash webfetch
  printf '{\n  "$schema": "https://opencode.ai/config.json",\n  "permission": { "edit": "%s", "bash": "%s", "webfetch": "%s" }\n}\n' \
    "$2" "$3" "$4" > "$dir/abox-$1.json"
}
write ask    ask   ask   ask
write auto   allow ask   allow
write bypass allow allow allow
# Older abox versions wrote an allow-everything global config; drop it if unmodified.
legacy='{
  "$schema": "https://opencode.ai/config.json",
  "permission": { "edit": "allow", "bash": "allow", "webfetch": "allow" }
}'
if [[ -f $dir/opencode.json && "$(cat "$dir/opencode.json")" == "$legacy" ]]; then
  rm -f "$dir/opencode.json"
fi
