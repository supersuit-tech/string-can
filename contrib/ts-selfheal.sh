#!/bin/bash
# Keep a normal Grok Bot node reachable after a microVM freeze.
# A freeze drops tailscaled's coordination connection without a TCP reset, so
# Self.Online stays false while that dead socket is reused. Close tailscaled's
# established TCP :443 sockets on wake and after ~20s offline (again at ~60s)
# so it redials immediately. If it is still offline after ~120s, restart
# tailscaled and string-can serve.
# This is for a normal Grok Bot node. Outbound-only agents should run
# `string-can supervise` instead; that restarts serve and fetch --watch itself.
# The 100.x address can remain assigned while Self.Online=false, so serve looks
# healthy but peers cannot connect. Run on the Grok Bot computer, not via the
# Tailscale plugin. Required on every Grok Bot node:
#
#   export TAILSCALED_START='<your tailscaled start command, using a reusable auth key>'
#   export STRING_CAN_NAME=grokbot
#   # optional overrides: STRING_CAN_BIN=/path/to/string-can
#   #                    LOG=$HOME/.local/tailscale/selfheal.log
#   nohup bash -lc ./ts-selfheal.sh >> ~/.string-can-selfheal.log 2>&1 &
#
# A full reset of the Grok Bot computer kills background processes; have the
# bot re-launch this loop (a daily check is enough).
set -eu
NAME="${STRING_CAN_NAME:-string-can}"
BIN="${STRING_CAN_BIN:-string-can}"
LOG="${LOG:-$HOME/.local/tailscale/selfheal.log}"
if [[ -z "${TAILSCALED_START:-}" ]]; then
  echo "ts-selfheal: set TAILSCALED_START to your tailscaled start command" >&2
  exit 1
fi
mkdir -p "$(dirname "$LOG")"

# `ss ... state established` omits the State column, so field 3 is Local Address:Port.
kill_socks() {
  local pid n sp
  pid=$(pgrep -x tailscaled | tail -1 || true)
  [[ -z "$pid" ]] && return 0
  n=0
  for sp in $(sudo ss -tnpH state established "( dport = :443 )" | grep "pid=$pid," | awk '{print $3}' | awk -F: '{print $NF}' || true); do
    [[ "$sp" =~ ^[0-9]+$ ]] || continue
    if sudo ss -K -tn "( dport = :443 and sport = :$sp )" >/dev/null 2>&1; then
      n=$((n + 1))
    fi
  done
  echo "$(date '+%F %T') $1: killed $n tailscaled :443 sockets" >> "$LOG"
}

bad=0
last=""
prev=$(date +%s)
while true; do
  now=$(date +%s)
  if (( now - prev > 55 )); then
    kill_socks "wake after $((now - prev))s freeze"
    sleep 3
  fi
  on=$(tailscale status --json 2>/dev/null | python3 -c "import json,sys;print(json.load(sys.stdin)['Self'].get('Online'))" 2>/dev/null || true)
  [[ -z "$on" ]] && on="unknown"
  if [[ "$on" != "$last" ]]; then
    echo "$(date '+%F %T') state $last -> $on" >> "$LOG"
    last="$on"
  fi
  if [[ "$on" == "True" ]]; then
    bad=0
  else
    bad=$((bad + 1))
  fi
  if (( bad == 2 || bad == 6 )); then
    kill_socks "offline ~$((bad * 10))s"
  fi
  if (( bad >= 12 )); then
    echo "$(date '+%F %T') offline ~120s after socket kills, restarting" >> "$LOG"
    sudo pkill -f tailscaled || true
    sleep 2
    bash -lc "$TAILSCALED_START"
    sleep 5
    pkill -f "$(basename "$BIN") serve" || true
    nohup "$BIN" serve --name "$NAME" >> ~/.string-can-serve.log 2>&1 &
    bad=0
    last=""
  fi
  prev=$(date +%s)
  sleep 10
done
