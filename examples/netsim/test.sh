#!/bin/bash
# Run INSIDE a sandbox (copy this dir into /work, then: bash test.sh)
# Checks: nested Docker, multi-network Compose, tc netem, network isolation.
set -euo pipefail
cd "$(dirname "$0")"
dc() { docker compose -p abox-netsim "$@"; }
trap 'dc down -t 0 >/dev/null 2>&1' EXIT

dc up -d --quiet-pull
echo "== b -> c baseline";        dc exec -T b ping -c 3 -q c | tail -1
dc exec -T c tc qdisc add dev eth0 root netem delay 100ms loss 0%
echo "== b -> c with 100ms netem"; out=$(dc exec -T b ping -c 3 -q c | tail -1); echo "$out"
avg=$(awk -F/ '{print int($5)}' <<<"$out"); (( avg >= 90 )) || { echo "FAIL: netem not applied"; exit 1; }
echo "== a -> c must fail (different network)"
if dc exec -T a ping -c 1 -W 1 c >/dev/null 2>&1; then echo "FAIL: a reached c"; exit 1; fi
echo "== partition: drop b<->c"
dc exec -T c tc qdisc change dev eth0 root netem loss 100%
if dc exec -T b ping -c 2 -W 1 c >/dev/null 2>&1; then echo "FAIL: partition leaked"; exit 1; fi
echo "PASS"
