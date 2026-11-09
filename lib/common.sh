# shellcheck shell=bash
# Sourced by sbx, install-host.sh and egress/apply.sh.

SBX_ROOT="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/.." && pwd)"
# shellcheck source=../config.env
source "$SBX_ROOT/config.env"

SBX_HOST_IP="${SBX_NET_PREFIX}.1"
SBX_PROXY_URL="http://${SBX_HOST_IP}:${SBX_PROXY_PORT}"
SBX_NO_PROXY="localhost,127.0.0.1,::1,.local,.internal,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"

die()  { printf 'sbx: error: %s\n' "$*" >&2; exit 1; }
info() { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }

# render <template>: substitute @SBX_*@ placeholders, print to stdout
render() {
  sed -e "s|@SBX_BRIDGE@|${SBX_BRIDGE}|g" \
      -e "s|@SBX_NET_PREFIX@|${SBX_NET_PREFIX}|g" \
      -e "s|@SBX_HOST_IP@|${SBX_HOST_IP}|g" \
      -e "s|@SBX_PROXY_PORT@|${SBX_PROXY_PORT}|g" \
      -e "s|@SBX_PROXY_URL@|${SBX_PROXY_URL}|g" \
      -e "s|@SBX_NO_PROXY@|${SBX_NO_PROXY}|g" \
      -e "s|@SBX_POOL@|${SBX_POOL}|g" \
      -e "s|@SBX_UID@|${SBX_UID:-1000}|g" \
      -e "s|@SBX_GUEST_USER@|${SBX_GUEST_USER}|g" \
      -e "s|@SBX_NET_OPEN_MINUTES@|${SBX_NET_OPEN_MINUTES}|g" \
      "$1"
}
