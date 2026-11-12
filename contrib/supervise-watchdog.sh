#!/bin/sh
# Start string-can supervise when it is not already running.
#
# supervise itself restarts serve and fetch --watch within seconds. This
# script only brings supervise back after the whole process is killed, which
# is the case for a container that has cron and neither systemd nor launchd.
#
#   * * * * * STRING_CAN_SUPERVISE_ARGS='--outbound-only --name muse' /path/to/contrib/supervise-watchdog.sh
#
# Optional: STRING_CAN_SUPERVISE_LOG (default ~/.string-can-supervise.log).
# string-can must be on PATH. STRING_CAN_HOME and webhook variables, if you
# need them, belong in the cron environment too.
set -eu

if pgrep -f '[s]tring-can supervise' >/dev/null 2>&1; then
	exit 0
fi

log="${STRING_CAN_SUPERVISE_LOG:-$HOME/.string-can-supervise.log}"
# Intentionally unquoted so STRING_CAN_SUPERVISE_ARGS splits into flags.
# shellcheck disable=SC2086
nohup string-can supervise ${STRING_CAN_SUPERVISE_ARGS:-} >>"$log" 2>&1 &
