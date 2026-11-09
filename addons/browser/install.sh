#!/bin/bash
# Runs inside the sandbox as the agent user (login shell: proxy env set).
# Installs Google Chrome (used headless) + the Playwright MCP server and registers the
# server ("playwright") with every installed agent. Re-run by abox whenever an
# agent is added, so keep it idempotent and quick when nothing changed.
set -euo pipefail
MCP_PKG=@playwright/mcp
CHROME_DEB=https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
log=/tmp/abox-browser-install.log

if [[ -n ${ABOX_AGENT_UPDATE:-} ]] || ! command -v playwright-mcp >/dev/null; then
  sudo /usr/local/lib/abox/ensure-node
  npm install -g --prefix "$HOME/.local" "$MCP_PKG@latest"
fi
# Google Chrome stable from dl.google.com (Playwright's Chromium builds are served
# from storage.googleapis.com, which would open all of Google Cloud Storage to
# every sandbox). The package also adds Google's apt repo for updates.
if [[ -n ${ABOX_AGENT_UPDATE:-} ]] || ! command -v google-chrome >/dev/null; then
  if ! curl -fsSL -o /tmp/google-chrome.deb "$CHROME_DEB" 2>"$log"; then
    cat "$log" >&2; echo "Chrome download failed. Blocked hosts: abox net log" >&2; exit 1
  fi
  sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y /tmp/google-chrome.deb >>"$log" 2>&1 \
    || { tail -5 "$log" >&2; exit 1; }
  rm -f /tmp/google-chrome.deb
fi

# Headless, in-memory profile; external pages go through the egress proxy
# (allowlist applies), the app under test on localhost is reached directly.
args=(--headless --isolated --browser chrome --output-dir /tmp/playwright-mcp)
if [[ -n ${https_proxy:-} ]]; then
  args+=(--proxy-server "$https_proxy" --proxy-bypass "localhost,127.0.0.1,::1")
fi
cmd=("$HOME/.local/bin/playwright-mcp" "${args[@]}")

if command -v claude >/dev/null; then
  claude mcp remove -s user playwright >/dev/null 2>&1 || true
  claude mcp add -s user playwright -- "${cmd[@]}" >/dev/null && echo "registered playwright MCP with claude"
fi
if command -v codex >/dev/null; then
  codex mcp remove playwright >/dev/null 2>&1 || true
  codex mcp add playwright -- "${cmd[@]}" >/dev/null && echo "registered playwright MCP with codex"
fi
if command -v gemini >/dev/null; then
  gemini mcp remove -s user playwright >/dev/null 2>&1 || true
  gemini mcp add -s user playwright "${cmd[@]}" >/dev/null && echo "registered playwright MCP with gemini"
fi
if command -v opencode >/dev/null; then
  # global config, merged with the per-mode abox-*.json files
  cfg=$HOME/.config/opencode/opencode.json
  mkdir -p "$(dirname "$cfg")"
  [[ -s $cfg ]] || echo '{"$schema": "https://opencode.ai/config.json"}' > "$cfg"
  jq --argjson c "$(printf '%s\n' "${cmd[@]}" | jq -R . | jq -s .)" \
     '.mcp.playwright = {type: "local", command: $c, enabled: true}' "$cfg" > "$cfg.new" && mv "$cfg.new" "$cfg"
  echo "registered playwright MCP with opencode"
fi
