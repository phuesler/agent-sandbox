#!/bin/bash
set -euo pipefail
if [[ -n ${ABOX_AGENT_UPDATE:-} ]] || ! command -v codex >/dev/null; then
  sudo /usr/local/lib/abox/ensure-node
  npm install -g --prefix "$HOME/.local" @openai/codex@latest
fi
# Codex runs model commands in its own bubblewrap sandbox (used by ask/auto modes).
# Ubuntu restricts unprivileged user namespaces via AppArmor, also inside the
# sandbox, so allow them for Codex's bundled bwrap only.
if command -v apparmor_parser >/dev/null && [[ -d /sys/kernel/security/apparmor ]]; then
  sudo tee /etc/apparmor.d/abox-codex-bwrap >/dev/null <<'PROFILE'
abi <abi/4.0>,
include <tunables/global>

profile abox-codex-bwrap /home/*/.local/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-*/vendor/*/codex-resources/bwrap flags=(unconfined) {
  userns,
  include if exists <local/abox-codex-bwrap>
}
PROFILE
  sudo apparmor_parser -r /etc/apparmor.d/abox-codex-bwrap
fi
