#!/bin/bash
set -euo pipefail
if [[ -n ${ABOX_AGENT_UPDATE:-} ]] || ! command -v gemini >/dev/null; then
  sudo /usr/local/lib/abox/ensure-node
  npm install -g --prefix "$HOME/.local" @google/gemini-cli@latest
fi
