#!/bin/bash
# Runs inside the sandbox as the agent user (login shell: proxy env set).
set -euo pipefail
if [[ -n ${ABOX_AGENT_UPDATE:-} ]] || ! command -v claude >/dev/null; then
  curl -fsSL https://claude.ai/install.sh | bash
fi
