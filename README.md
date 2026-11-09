# agent sandbox (`abox` / `sbx`)

Run coding agents (**Claude Code, Codex, opencode, Gemini CLI**, or any agent you add) in fully autonomous mode inside Incus **system containers** or **VMs**. Each sandbox:

- sees **only one project directory** from the host, mounted at `/work`
- is the agent's own OS: passwordless root, Docker, Compose, `tc`/netns. The agent can install or tune anything, and snapshots let you roll it back
- runs Ubuntu 24.04 LTS by default. NixOS is available as an experiment
- reaches the internet **only through an allowlisting proxy**, with a per-sandbox, time-limited "open" toggle
- can have several agents installed side by side. One of them is the project's default

```
host (Ubuntu 24.04)
├── incus ── sbxbr0 10.88.0.0/24 ──┬── sandbox A (container)  /work ← ~/code/proj-a   agents: claude, codex
│                                  └── sandbox B (VM)         /work ← ~/code/proj-b   agents: opencode
│                                        └── dockerd → compose networks, netem, ...
├── squid  10.88.0.1:3128   domain allowlist (base + enabled agents), logs denials
└── nftables table inet sbx forward: drop everything except per-IP "open" set
                            input:   only DNS, DHCP, proxy from the bridge
```

## Install (once)

```bash
sudo ./install-host.sh          # Incus from Zabbly (or --distro-incus), squid, firewall
newgrp incus-admin              # or log out/in
make install                    # builds the abox binary and links ~/.local/bin/abox
abox doctor
```

Host Docker keeps running untouched. The only change is two `DOCKER-USER` accept rules for `sbxbr0`. Docker's `FORWARD DROP` policy would otherwise break Incus networking.

`abox` is a single static Go binary with no runtime dependencies. Building it needs Go 1.24 (see `.tool-versions`). On a machine without Go, run `make docker-build link` instead. The binary finds the repo (for `sbx`, `config.env` and `agents/`) by resolving its own symlink. Keep it in `<repo>/bin`, or set `ABOX_REPO`.

## Daily use: `abox`

Run it from any project directory, or any subdirectory of one:

```bash
cd ~/code/myproj
abox                      # 1st time: asks name/type/image/agents, creates the sandbox, runs the default agent
abox                      # later: starts the sandbox if stopped, runs the default agent in the same subdir
abox codex                # run a specific agent (offers to enable it for this project)
abox claude -- --resume   # arguments after -- go to the agent
abox agents               # available agents, which this project uses, how each one logs in
abox agent add opencode   # enable + install more agents; agent rm / default / update
abox secret set OPENAI_API_KEY ~/.secrets/openai      # env var in the sandbox (value stays in the host file)
abox ls                   # all projects: agents, state, IP, egress (open/allowlist), snapshots
abox shell | root         # shell in the sandbox (same subdir) | root shell
abox exec -- make test    # one-off command in the sandbox
abox net open 20          # unrestricted internet for this project for 20 min (net close / net log)
abox snap pre-upgrade ; abox restore pre-upgrade      # restore without label = 'clean'
abox set memory=16GiB cpus=8                          # applied live
abox set type=vm && abox rebuild                      # type/image/disk need a rebuild
abox down --all ; abox up --all
abox rm                   # delete sandbox + unregister (project files untouched)
abox doctor               # check host setup, services, registry
abox -p other-proj shell  # target another project by name or path
eval "$(abox completion)" # bash completion (put in ~/.bashrc)
```

Projects are recorded in `~/.config/abox/projects.toml` (`abox edit`), with defaults for type, image and agents under `[defaults]`. If no agent default is set there, `SBX_DEFAULT_AGENTS` from `config.env` applies. The registry lives on the host on purpose: nothing inside a sandbox can change its own resources, agents, secrets or egress.

The registry is the source of truth. If a sandbox is missing an agent or secret the registry lists, `abox` installs it. That happens after `abox agent add`, and after restoring an older snapshot. If an agent fails to install, the failure is a warning, unless you're trying to run that agent or asked for it with `up` or `agent`. `abox net open` never installs anything, so you can always open egress to fix a blocked download.

`sbx` is still available underneath (`sbx help`) for direct, name-based control. If you created sandboxes directly with `sbx`, `abox import` registers them.

### Credentials

Every agent runs as user `agent` (with your host uid) and keeps its login inside the sandbox. The host's `~/.claude`, `~/.codex`, SSH keys and Docker socket are never mounted. `abox agents` prints how each agent authenticates:

| agent | interactive login | API key (`abox secret set KEY FILE`) |
|---|---|---|
| claude | `/login` inside claude, then paste the code | `ANTHROPIC_API_KEY` |
| codex | `codex login --device-auth` | `OPENAI_API_KEY`, then `printenv OPENAI_API_KEY \| codex login --with-api-key` |
| opencode | `opencode auth login` | provider keys, e.g. `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY` |
| gemini | browser flow can't reach the sandbox | `GEMINI_API_KEY` |

Secrets are exported in the sandbox's login shells from `~/.sbx-env` (mode 600). The registry only stores the path of the host file. `abox secret sync` re-pushes the values after you rotate a key. For git push, create a deploy key or fine-grained token per project inside the sandbox.

### Adding another agent

Create `agents/<name>/` with three files (see `agents/README.md`), then run `sudo ./egress/apply.sh` so its domains are allowed. No code changes are needed: `abox agents` lists it, and `abox <name>` runs it.

## Container or VM?

| | container (`--container`, default) | VM (`--vm`) |
|---|---|---|
| start / RAM overhead | seconds / small | ~20 s / ~0.5–1 GiB |
| kernel | host's (modules preloaded via profile) | own: any module, sysctl, eBPF, kernel version |
| isolation | unprivileged userns + nesting | hardware virtualization |
| use for | normal projects, most Compose setups | heavy network simulation, kernel-level work, untrusted code |

Both run the same Docker/Compose stack. Copy `examples/netsim` into a project and run `bash test.sh` inside the sandbox. It exercises two Compose networks, `tc netem` latency, and a network partition.

## Egress

- Guests get `http(s)_proxy` (login shells, apt, dockerd, npm), set to `http://10.88.0.1:3128`. Direct connections are dropped. Private (RFC 1918), link-local and CGNAT ranges (LAN, most VPN overlays) are blocked even when a sandbox is open.
- The allowlist is `egress/allowlist.txt` plus `agents/*/allowlist.txt` for the agents in `SBX_AGENTS_ENABLED` (`config.env`; empty means all). `apply.sh` merges the lists and drops overlapping entries. The allowlist is global, not per sandbox: a sandbox with only Claude can still reach the OpenAI API if Codex is enabled on the host.
- `abox net log` lists blocked destinations per sandbox. Add what you need, then run `sudo ./egress/apply.sh`. Re-applying closes any open sandboxes.
- `abox net open 20` gives the sandbox unrestricted internet for 20 minutes, both through the proxy and directly. `abox net close` ends it early.
- **Containers inside the sandbox don't inherit the proxy.** Compose services that need the internet during build or run need `HTTP(S)_PROXY` set explicitly, or use `net open`. Image pulls work, because dockerd uses the proxy.
- Known residual channels: DNS queries (the bridge resolver forwards upstream) and allowlisted shared hosts such as `.githubusercontent.com` and `.openai.com`.

## NixOS (experimental)

```bash
abox add ~/code/proj --image nixos [--vm] --agent claude,codex
```

`nixos/sbx.nix` is installed as `/etc/nixos/sbx.nix` and imported. It sets up the proxy, Docker and the `agent` user. Agents come from nixpkgs (`AGENT_NIXPKG`) through a generated `/etc/nixos/sbx-agents.nix`, and `agent update` runs `nixos-rebuild switch --upgrade` there. The agent can change the OS declaratively (`sudo nixos-rebuild switch`, with `--rollback` to undo). Ask it to keep OS changes it depends on in the project repo, for example as `/work/.sandbox/*.nix`. Use Ubuntu for projects that require 24.04.

## Files

| path | purpose |
|---|---|
| `config.env` | subnet, bridge, pool, images, guest user, default/enabled agents |
| `install-host.sh` | one-time host setup |
| `cmd/abox/` | `abox` source (Go; `make install` builds `bin/abox`) |
| `sbx` | low-level CLI used by abox |
| `agents/<name>/` | agent plugins: `agent.conf`, `install.sh`, `allowlist.txt` |
| `profiles/*.yaml` | Incus profiles (`sbx init` reloads; changes to limits apply to existing sandboxes) |
| `egress/` | base allowlist, squid + nftables templates, `sbx-net` root helper, `apply.sh` |
| `systemd/` | `sbx-egress.service`, `sbx-proxy.service`, Docker/Incus forward fix |
| `nixos/sbx.nix` | NixOS module for `--image nixos` |
| `examples/netsim/` | Compose networking smoke test |

## Troubleshooting

- Agent install fails: run `abox net log`. Usually the agent's download host isn't in the allowlist yet, so run `sudo ./egress/apply.sh` after adding agents. As a one-off, `abox net open 10 && abox up && abox net close` also works.
- Provisioning fails: run `abox root` and look at `/var/log/cloud-init-output.log`.
- No network in guests: `systemctl status sbx-egress sbx-proxy` and `sudo nft list table inet sbx`. If ufw or firewalld is active, allow `sbxbr0`.
- Docker inside a container fails: use `--vm` for that project. It happens with exotic storage drivers or AppArmor interactions.
