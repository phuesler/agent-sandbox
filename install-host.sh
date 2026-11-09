#!/bin/bash
# One-time host setup: Incus (+ storage pool, sandbox bridge), squid, egress layer.
# Usage: sudo ./install-host.sh [--distro-incus]
#   default        Incus "stable" from the Zabbly repo (recent; bundles QEMU/OVMF/virtiofsd)
#   --distro-incus Ubuntu 24.04's incus 6.0 package (older, plus qemu-system-x86 + ovmf)
# Leaves the existing host Docker untouched except for two DOCKER-USER accept rules
# for the sandbox bridge (see systemd/docker-user.sh.in).
set -euo pipefail
source "$(dirname "$(readlink -f "$0")")/lib/common.sh"

[[ $EUID -eq 0 ]] || die "run as root: sudo $0"
[[ -n ${SUDO_USER:-} && $SUDO_USER != root ]] || die "run via sudo from your normal user account"
SOURCE=zabbly
[[ ${1:-} == --distro-incus ]] && SOURCE=distro

ZABBLY_FPR=4EFC590696CB15B87C73A3AD82CC8797C838DCFD
export DEBIAN_FRONTEND=noninteractive

if ! command -v incus >/dev/null; then
  if [[ $SOURCE == zabbly ]]; then
    info "adding Zabbly Incus repository"
    install -d -m 0755 /etc/apt/keyrings
    curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
    got=$(gpg --show-keys --with-colons /etc/apt/keyrings/zabbly.asc | awk -F: '/^fpr/{print $10; exit}')
    [[ $got == "$ZABBLY_FPR" ]] || { rm -f /etc/apt/keyrings/zabbly.asc; die "Zabbly key fingerprint mismatch: $got"; }
    . /etc/os-release
    cat > /etc/apt/sources.list.d/zabbly-incus-stable.sources <<EOF
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: ${VERSION_CODENAME}
Components: main
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/zabbly.asc
EOF
    apt-get update
    apt-get install -y incus
  else
    apt-get update
    apt-get install -y incus qemu-system-x86 ovmf
  fi
else
  info "incus already installed: $(incus --version)"
fi

info "installing squid, nftables, btrfs-progs"
apt-get install -y squid nftables btrfs-progs
# We run our own squid instance (sbx-proxy.service); the distro one would grab port 3128.
systemctl disable --now squid.service >/dev/null 2>&1 || true
systemctl mask squid.service >/dev/null 2>&1 || true

systemctl enable --now incus.socket incus.service >/dev/null 2>&1 || systemctl enable --now incus >/dev/null
usermod -aG incus-admin "$SUDO_USER"

info "storage pool '$SBX_POOL' (btrfs loop, $SBX_POOL_SIZE)"
incus storage show "$SBX_POOL" >/dev/null 2>&1 \
  || incus storage create "$SBX_POOL" btrfs size="$SBX_POOL_SIZE"

info "network '$SBX_BRIDGE' (${SBX_HOST_IP}/24, NAT, no IPv6)"
incus network show "$SBX_BRIDGE" >/dev/null 2>&1 \
  || incus network create "$SBX_BRIDGE" \
       ipv4.address="${SBX_HOST_IP}/24" ipv4.nat=true ipv4.dhcp=true \
       ipv6.address=none dns.domain=sbx

"$SBX_ROOT/egress/apply.sh"

if systemctl is-active --quiet docker; then
  info "adding DOCKER-USER accept rules for $SBX_BRIDGE (no Docker restart)"
  /usr/local/lib/sbx/docker-user.sh || true
fi

info "linking sbx (and abox, if built) into /usr/local/bin"
ln -sfn "$SBX_ROOT/sbx" /usr/local/bin/sbx
if [[ -x "$SBX_ROOT/bin/abox" ]]; then
  ln -sfn "$SBX_ROOT/bin/abox" /usr/local/bin/abox
else
  warn "bin/abox not built yet: run 'make install' (or 'make docker-build') as $SUDO_USER"
fi

cat <<EOF

Done. Next steps (as $SUDO_USER):
  1. Log out/in (or run: newgrp incus-admin) so the incus-admin group applies.
  2. cd ~/code/some-project && abox     # register, create sandbox, run the default agent
     abox doctor                          # if anything looks off
EOF
