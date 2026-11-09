#!/bin/bash
# Runs inside the sandbox as the agent user (login shell: proxy env set).
# Installs headless Chromium + the Playwright MCP server and registers the
# server ("playwright") with every installed agent. Re-run by abox whenever an
# agent is added, so keep it idempotent and quick when nothing changed.
set -euo pipefail
MCP_PKG=@playwright/mcp
PW_CLI=$HOME/.local/lib/node_modules/@playwright/mcp/node_modules/playwright/cli.js

if [[ -n ${ABOX_AGENT_UPDATE:-} ]] || ! command -v playwright-mcp >/dev/null; then
  sudo /usr/local/lib/abox/ensure-node
  npm install -g --prefix "$HOME/.local" "$MCP_PKG@latest"
fi
if [[ -n ${ABOX_AGENT_UPDATE:-} ]] || ! ls "$HOME"/.cache/ms-playwright/chromium_headless_shell-* >/dev/null 2>&1; then
  log=/tmp/abox-browser-install.log
  # quiet: a blocked download makes Playwright print whole proxy error pages
  if ! node "$PW_CLI" install chromium >"$log" 2>&1; then
    grep -E "Error|error:" "$log" | grep -v -E "^\s*[#/<]" | head -3
    echo "Chromium download failed (full log: $log). Blocked hosts: abox net log" >&2
    exit 1
  fi
  # system libraries for Chromium (apt, via the sandbox proxy)
  sudo env "PATH=$PATH" "http_proxy=${http_proxy:-}" "https_proxy=${https_proxy:-}" node "$PW_CLI" install-deps chromium >>"$log" 2>&1 \
    || { tail -5 "$log"; exit 1; }
fi

# Headless, in-memory profile; external pages go through the egress proxy
# (allowlist applies), the app under test on localhost is reached directly.
args=(--headless --isolated --browser chromium --output-dir /tmp/playwright-mcp)
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
