# Add-ons

Extra tooling for the agents in a sandbox, enabled per project with `abox addon add <name>`. Like agent plugins, add-ons are built into the abox binary. After changing one, rebuild (`make install`) and run `sudo abox host apply`.

An add-on directory contains:

| file | purpose |
|---|---|
| `addon.conf` | `ADDON_DESC="..."`, shown by `abox addon ls` |
| `install.sh` | runs **inside the sandbox** as the `agent` user in a login shell (proxy env set, passwordless `sudo`) |
| `allowlist.txt` | download hosts the add-on needs; always merged into the egress allowlist |

`install.sh` must be idempotent. abox re-runs it after an agent is added to the project, so an add-on can register itself with every installed agent (see `browser/install.sh`). Skip downloads when the tool is already present, unless `ABOX_AGENT_UPDATE` is non-empty (`abox addon update`).
