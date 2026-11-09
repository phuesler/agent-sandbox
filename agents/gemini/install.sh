#!/bin/bash
set -euo pipefail
if [[ -n ${SBX_AGENT_UPDATE:-} ]] || ! command -v gemini >/dev/null; then
  sudo /usr/local/lib/sbx/ensure-node
  npm install -g --prefix "$HOME/.local" @google/gemini-cli@latest
fi
