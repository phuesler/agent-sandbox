#!/bin/bash
set -euo pipefail
if [[ -n ${ABOX_AGENT_UPDATE:-} ]] || ! command -v gemini >/dev/null; then
  sudo /usr/local/lib/abox/ensure-node
  npm install -g --prefix "$HOME/.local" @google/gemini-cli@latest
fi
# /work is the project abox shares into the sandbox; trust it, otherwise Gemini
# disables MCP servers (e.g. the browser add-on) and project settings there.
tf=$HOME/.gemini/trustedFolders.json
mkdir -p "$(dirname "$tf")"
[[ -s $tf ]] || echo '{}' > "$tf"
jq '.["/work"] = "TRUST_FOLDER"' "$tf" > "$tf.new" && mv "$tf.new" "$tf"
