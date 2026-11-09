# Agent plugins

Each directory here is one coding agent. They are built into the abox binary (`go:embed`): after adding or changing one, rebuild with `make install`. A valid plugin needs `agent.conf` and `install.sh`, and its name must be lowercase letters, digits and dashes. It must also not collide with an `abox` command such as `run`, `ls` or `shell`.

## `agent.conf`

Plain `KEY="value"` lines (one per line, no shell expansion).

| key | required | meaning |
|---|---|---|
| `AGENT_DESC` | yes | one-line description (`abox agents`) |
| `AGENT_CMD` | yes | command that starts the agent **in autonomous mode**: no permission prompts, because the sandbox is the boundary. Arguments after `abox <agent> --` are appended |
| `AGENT_NIXPKG` | for NixOS | nixpkgs attribute that provides the agent |
| `AGENT_AUTH` | no | how to log in / which `abox secret` to set (shown by `abox agents`) |

## `install.sh`

Runs **inside the sandbox** as the unprivileged user `agent`, in a login shell. The proxy environment is set, `~/.local/bin` is on PATH, and passwordless `sudo` is available.

- Be idempotent. Skip the download if the binary already exists, unless `SBX_AGENT_UPDATE` is non-empty (set by `abox agent update`).
- Install into `~/.local/bin` (link the binary there if the vendor installs elsewhere).
- npm-based agents call `sudo /usr/local/lib/sbx/ensure-node` first (installs Node 22 once), then `npm install -g --prefix "$HOME/.local" <pkg>`.
- Write config that makes the agent autonomous if it has no CLI flag for it (see `opencode/install.sh`).
- On NixOS the binary comes from `AGENT_NIXPKG`, and `install.sh` only runs its config part, because `command -v` already succeeds.

## `allowlist.txt`

Domains the agent needs for installing, updating and talking to its API, in squid `dstdomain` format (`.example.com` = domain plus subdomains). They are merged into the global egress allowlist for agents listed in `SBX_AGENTS_ENABLED` (`/etc/abox/config.env`; empty means all). After adding or changing one, rebuild and run `sudo abox host apply`. To find what an agent needs, try installing it (`abox agent add <name>`) without opening egress, read the blocked hosts from `abox net log`, add them, and repeat. `net log` only shows denied requests.

## Example: aider (untested sketch)

```
agents/aider/agent.conf
  AGENT_DESC="aider (multi-provider)"
  AGENT_CMD="aider --yes-always"
  AGENT_NIXPKG="aider-chat"
  AGENT_AUTH="abox secret set ANTHROPIC_API_KEY / OPENAI_API_KEY <file>"

agents/aider/install.sh
  #!/bin/bash
  set -euo pipefail
  if [[ -n ${SBX_AGENT_UPDATE:-} ]] || ! command -v aider >/dev/null; then
    curl -LsSf https://aider.chat/install.sh | sh
  fi

agents/aider/allowlist.txt
  aider.chat
  .astral.sh
```
