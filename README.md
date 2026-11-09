# agent-sandbox (`abox`)

Run coding agents (**Claude Code, Codex, opencode, Gemini CLI**, or any agent you add) inside Incus **system containers** or **VMs**, by default in each agent's own automatic permission mode. Each sandbox:

- sees **only one project directory** from the host, mounted at `/work`
- is the agent's own OS: passwordless root, Docker, Compose, `tc`/netns. The agent can install or tune anything, and snapshots let you roll it back
- runs Ubuntu 24.04 LTS by default. NixOS is available as an experiment
- reaches the internet **only through an allowlisting proxy**, with a per-sandbox, time-limited "open" toggle
- can have several agents installed side by side. One of them is the project's default

```
host (Ubuntu 24.04)
├── incus ── aboxbr0 10.88.0.0/24 ──┬── sandbox A (container)  /work ← ~/code/proj-a   agents: claude, codex
│                                  └── sandbox B (VM)         /work ← ~/code/proj-b   agents: opencode
│                                        └── dockerd → compose networks, netem, ...
├── abox egress filter  10.88.0.1:3128-3130  hostname allowlist (explicit proxy + transparent HTTPS/HTTP), logs denials
└── nftables table inet abox forward: drop everything except per-IP "open" set
                            input:   only DNS, DHCP, proxy from the bridge
```

## Install (once)

```bash
make install                    # builds bin/abox (Go 1.24, see .tool-versions) and links ~/.local/bin/abox
                                # no Go? make docker-build link
sudo abox host install          # Incus from Zabbly (or --distro-incus), egress filter + firewall
newgrp incus-admin              # or log out/in
abox doctor
```

`abox` is a single static binary with no runtime dependencies. Everything it needs is built in: the agent plugins, Incus profiles, egress and systemd templates, and default settings. Copy it anywhere; the repo is only needed to build it.

`sudo abox host install` installs Incus and nftables, creates the storage pool and the `aboxbr0` bridge, and sets up the egress layer. It also writes the settings to `/etc/abox/config.env`. That file is root-owned, so nothing in a sandbox can change it. After editing it, or after rebuilding with a changed allowlist or new agents, run `sudo abox host apply`.

Host Docker keeps running untouched. The only change is two `DOCKER-USER` accept rules for `aboxbr0`. Docker's `FORWARD DROP` policy would otherwise break Incus networking.

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
abox addon add browser    # headless Chrome the agents can drive (see "Web apps" below)
abox image ls             # base image new sandboxes are cloned from (see below)
abox port 3000            # http://localhost:3000 on this machine -> port 3000 in the sandbox
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

Projects are recorded in `~/.config/abox/projects.toml` (`abox edit`), with defaults for type, image and agents under `[defaults]`. If no agent default is set there, `ABOX_DEFAULT_AGENTS` from `/etc/abox/config.env` applies. The registry lives on the host on purpose: nothing inside a sandbox can change its own resources, agents, secrets or egress.

The registry is the source of truth. If a sandbox is missing an agent or secret the registry lists, `abox` installs it. That happens after `abox agent add`, and after restoring an older snapshot. If an agent fails to install, the failure is a warning, unless you're trying to run that agent or asked for it with `up` or `agent`. `abox net open` never installs anything, so you can always open egress to fix a blocked download.

Sandboxes are ordinary Incus instances (`incus list user.abox=true`). If you created or changed them outside `abox`, `abox import` registers them.

### Permission modes

Each project has a mode that decides how the agent is started. Set it with `abox set mode=...`, or override it for a single run with `abox --ask`, `abox --auto` or `abox --bypass`. `abox ls` and `abox status` show it, and every run prints it.

| mode | meaning | claude | codex | gemini | opencode |
|---|---|---|---|---|---|
| `ask` | asks before edits and commands | `--permission-mode manual` | read-only sandbox, asks on request | `--approval-mode default` | edit/bash/webfetch: ask |
| **`auto`** (default) | the agent's own automatic mode: routine actions run, risky ones are checked or asked | `--permission-mode auto` | `--approve-for-me` (automatic review, Codex's own workspace sandbox) | `--approval-mode auto_edit` | edits and webfetch allowed, shell commands ask |
| `bypass` | no permission checks; the sandbox is the only boundary | `--dangerously-skip-permissions` | `--dangerously-bypass-approvals-and-sandbox` | `--yolo` | everything allowed |

Even in `bypass` mode the sandbox limits the damage: only the project directory is visible, only allowlisted domains are reachable, and no host credentials are present. But `/work` is your real project directory, so the agent can delete or overwrite anything in it, and it can use any API key or token you gave it. Commit often, and prefer `auto` unless you are watching.

Codex's own sandbox (used by `ask` and `auto`) needs user namespaces, which Ubuntu restricts through AppArmor. The codex plugin installs a narrow AppArmor profile inside the sandbox that allows them only for Codex's bundled `bwrap`.

### Credentials

Every agent runs as user `agent` (with your host uid) and keeps its login inside the sandbox. The host's `~/.claude`, `~/.codex`, SSH keys and Docker socket are never mounted. `abox agents` prints how each agent authenticates:

| agent | interactive login | API key (`abox secret set KEY FILE`) |
|---|---|---|
| claude | `/login` inside claude, then paste the code | `ANTHROPIC_API_KEY` |
| codex | `codex login --device-auth` | `OPENAI_API_KEY`, then `printenv OPENAI_API_KEY \| codex login --with-api-key` |
| opencode | `opencode auth login` | provider keys, e.g. `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY` |
| gemini | browser flow can't reach the sandbox | `GEMINI_API_KEY` |

Secrets are exported in the sandbox's login shells from `~/.abox-env` (mode 600). The registry only stores the path of the host file. `abox secret sync` re-pushes the values after you rotate a key. For git push, create a deploy key or fine-grained token per project inside the sandbox.

### Adding another agent

Create `agents/<name>/` with three files (see `agents/README.md`), rebuild with `make install`, then run `sudo abox host apply` so its domains are allowed. No Go code changes are needed: `abox agents` lists it, and `abox <name>` runs it.

## Fast sandbox creation: the base image

New Ubuntu sandboxes are copy-on-write clones of a local base image (`abox-base-container`, or `abox-base-vm` for VMs). The image already contains the OS packages, Docker, Node and the agents. Creating a sandbox takes about 5 seconds instead of several minutes. The image is about 1.2 GB; clones share it copy-on-write.

- The image is built automatically by the first `abox add` (about 7 minutes, once per sandbox type). It's rebuilt automatically when abox or its settings change, because the image records a hash of everything it was built from.
- `abox image ls` shows its age and status. `abox image build [--vm]` rebuilds it to pick up package and agent updates; `abox doctor` reminds you after 14 days.
- The image never contains a project, secrets or agent logins. It's built from a fresh sandbox, and per-instance state (machine ID, SSH host keys, cloud-init state, logs, caches) is reset so every clone gets its own identity.
- `ABOX_IMAGE_AGENTS` in `/etc/abox/config.env` limits which agents are preinstalled (empty means all).
- Add-on binaries are preinstalled too (`ABOX_IMAGE_ADDONS`, empty means all, `none` means none), but add-ons stay opt-in. Without `abox addon add browser`, no agent gets a browser tool. Enabling it only registers the tool, which takes a few seconds instead of minutes.
- NixOS sandboxes are still provisioned from scratch.

## Web apps: browser and port forwarding

Agents can check their own work in a real browser, and you can watch the same app from your machine:

```bash
abox addon add browser    # headless Chrome + Playwright MCP server, registered with every agent
abox port 3000            # http://localhost:3000 here -> the dev server in the sandbox
abox port 8080:3000       # different host port
abox port                 # list; abox port rm 3000 to stop
```

- **Browser add-on:** installs Google Chrome (run headless; from `dl.google.com`, the only extra allowlist entry) and the [Playwright MCP server](https://github.com/microsoft/playwright-mcp) inside the sandbox and registers it as `playwright` with Claude, Codex, Gemini and opencode. That includes agents you add later. Agents can then navigate, click, fill in forms, read the page and take screenshots. The browser is headless and keeps no profile. It reaches the app under test on `localhost` directly. Every other site goes through the egress proxy, so the allowlist applies: an app that loads fonts or scripts from a CDN may need `abox net open` or extra allowlist entries.
- **Port forwarding:** an Incus proxy device listening on `127.0.0.1` only, so it's never exposed to your LAN. In containers, the app can listen on `localhost`. In VMs, the forward goes to the VM's address, which `abox` pins, so the app must listen on `0.0.0.0`. Forwards are stored with the project and restored after `restore`/`rebuild`.
- Add-ons are not available for NixOS sandboxes yet.

## Container or VM?

| | container (`--container`, default) | VM (`--vm`) |
|---|---|---|
| start / RAM overhead | seconds / small | ~20 s / ~0.5–1 GiB |
| kernel | host's (modules preloaded via profile) | own: any module, sysctl, eBPF, kernel version |
| isolation | unprivileged userns + nesting | hardware virtualization |
| use for | normal projects, most Compose setups | heavy network simulation, kernel-level work, untrusted code |

Both run the same Docker/Compose stack. Copy `examples/netsim` into a project and run `bash test.sh` inside the sandbox. It exercises two Compose networks, `tc netem` latency, and a network partition.

## Egress

- Sandbox HTTPS (port 443) and HTTP (port 80) is redirected by nftables to the **abox egress filter** on the host. The filter reads the hostname from the TLS handshake (SNI, nothing is decrypted) or the HTTP `Host` header, checks it against the allowlist, and connects out by that name. This also works for containers inside the sandbox and for tools that ignore proxy settings. Guests additionally get `http(s)_proxy=http://10.88.0.1:3128`, the same filter as an explicit proxy. All other direct connections are dropped.
- The filter refuses destinations that resolve to private, loopback, link-local or CGNAT addresses, even for allowlisted names, which prevents DNS rebinding into your LAN. It runs as an unprivileged system user and listens on the bridge address only; nftables additionally drops its ports on every other interface. Private (RFC 1918), link-local and CGNAT ranges are blocked even when a sandbox is open.
- The allowlist is `egress/allowlist.txt` plus `agents/*/allowlist.txt` for the agents in `ABOX_AGENTS_ENABLED` (`/etc/abox/config.env`; empty means all). `abox host apply` merges the lists and drops overlapping entries. The allowlist is global, not per sandbox: a sandbox with only Claude can still reach the OpenAI API if Codex is enabled on the host.
- `abox net log` lists blocked destinations per sandbox. Add what you need to the allowlist, rebuild, then run `sudo abox host apply`. Re-applying closes any open sandboxes.
- `abox net open 20` gives the sandbox unrestricted internet for 20 minutes (the redirect is skipped and direct connections are allowed). `abox net close` ends it early.
- QUIC (HTTP/3 over UDP) and non-HTTP protocols such as git over SSH are not filtered by name and stay blocked; clients fall back to HTTPS over TCP, or use `abox net open`.
- Known residual channels: DNS queries (the bridge resolver forwards upstream), allowlisted shared hosts such as `.githubusercontent.com` and `.openai.com`, and domain fronting on CDNs that still allow it (the hostname is checked, the encrypted request inside is not).

## NixOS (experimental)

```bash
abox add ~/code/proj --image nixos [--vm] --agent claude,codex
```

`nixos/abox.nix` is installed as `/etc/nixos/abox.nix` and imported. It sets up the proxy, Docker and the `agent` user. Agents come from nixpkgs (`AGENT_NIXPKG`) through a generated `/etc/nixos/abox-agents.nix`, and `agent update` runs `nixos-rebuild switch --upgrade` there. The agent can change the OS declaratively (`sudo nixos-rebuild switch`, with `--rollback` to undo). Ask it to keep OS changes it depends on in the project repo, for example as `/work/.sandbox/*.nix`. Use Ubuntu for projects that require 24.04.

## Files

| path | purpose |
|---|---|
| `cmd/abox/` | `abox` source: CLI, registry, Incus operations (`sandbox.go`), host setup (`host.go`), egress filter (`proxy.go`) |
| `assets.go` | embeds the directories below into the binary |
| `config.env` | default settings (copied to `/etc/abox/config.env` by `host install`) |
| `agents/<name>/` | agent plugins: `agent.conf`, `install.sh`, `allowlist.txt` |
| `addons/<name>/` | add-ons (e.g. `browser`): `addon.conf`, `install.sh`, `allowlist.txt` |
| `profiles/*.yaml` | Incus profiles (reloaded whenever a sandbox is created; limit changes apply to existing sandboxes) |
| `egress/` | base allowlist, nftables template (firewall + transparent redirects), `abox-net` root helper (target of the sudo rule) |
| `systemd/` | `abox-egress.service`, `abox-proxy.service`, Docker/Incus forward fix, logrotate |
| `nixos/abox.nix` | NixOS module for `--image nixos` |
| `examples/netsim/` | Compose networking smoke test |

## Troubleshooting

- Agent install fails: run `abox net log`. Usually the agent's download host isn't in the allowlist yet, so add it, rebuild and run `sudo abox host apply`. As a one-off, `abox net open 10 && abox up && abox net close` also works.
- Provisioning fails: run `abox root` and look at `/var/log/cloud-init-output.log`.
- No network in guests: run `abox doctor`, `systemctl status abox-egress abox-proxy` and `sudo nft list table inet abox`. If ufw or firewalld is active, allow `aboxbr0`.
- Docker inside a container fails: use `--vm` for that project. It happens with exotic storage drivers or AppArmor interactions.

