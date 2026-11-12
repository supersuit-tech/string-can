#!/bin/bash
# Restart tailscaled + string-can if Tailscale stays offline for ~3 minutes.
# The 100.x address can remain assigned while Self.Online=false, so serve looks
# healthy but peers cannot connect. Run on the Grok Bot computer, not via the
# Tailscale plugin:
#
#   export TAILSCALED_START='<your tailscaled start command, using a reusable auth key>'
#   export STRING_CAN_NAME=grokbot
#   nohup bash -lc ./ts-selfheal.sh >> ~/.string-can-selfheal.log 2>&1 &
#
# A full reset of the Grok Bot computer kills background processes; have the
# bot re-launch this loop (a daily check is enough).
set -eu
NAME="${STRING_CAN_NAME:-string-can}"
if [[ -z "${TAILSCALED_START:-}" ]]; then
  echo "ts-selfheal: set TAILSCALED_START to your tailscaled start command" >&2
  exit 1
fi
bad=0
while true; do
  on=$(tailscale status --json 2>/dev/null | python3 -c "import json,sys;print(json.load(sys.stdin)['Self'].get('Online'))" 2>/dev/null || true)
  if [[ "$on" == "True" ]]; then
    bad=0
  else
    bad=$((bad+1))
  fi
  if (( bad >= 3 )); then
    sudo pkill -f tailscaled || true
    sleep 2
    bash -lc "$TAILSCALED_START"
    sleep 5
    pkill -f "string-can serve" || true
    nohup string-can serve --name "$NAME" >> ~/.string-can-serve.log 2>&1 &
    bad=0
  fi
  sleep 60
done
