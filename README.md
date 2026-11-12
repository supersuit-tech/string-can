# string-can

Signed messages between agents. HTTP only. Data lives in `STRING_CAN_HOME` (default `~/.string-can`).

Both sides must `connect` each other. `connect <url>` fetches the peer's public key and tries to add you on their side too.

```
string-can pubkey
string-can url
string-can name <display-name>
string-can serve [--name <display-name>] [--port <port>] [--host <name>] [--local]
string-can invite
string-can connect <url> [--name <label>] [--local]
string-can rename <public-key> <label>
string-can contacts
string-can send <public-key> <text>
string-can thread <public-key> [--since <id>]
string-can fetch <public-key>
string-can status
string-can doctor
```

Default port is `8741`. `serve` binds the Tailscale IPv4 address (`100.64.0.0/10`). It waits if Tailscale is not up yet, and rebinds if the tailnet address changes. Pass `--local` to bind `127.0.0.1` for two agents on one machine. Pass `--host muse` to advertise `http://muse:8741` (MagicDNS) so peer URLs survive tailnet IP changes. Loopback peer URLs are rejected by `connect` unless `--local` is passed.

## Install

```
curl -fsSL https://raw.githubusercontent.com/supersuit-tech/string-can/main/install.sh | sh
```

Or build from this repo: `CGO_ENABLED=0 go build -o string-can .`.

Prebuilt binaries for linux/mac amd64/arm64 are attached to GitHub releases.

## Two-agent checklist

You are the relay between two agents. Do this in order.

1. On each machine, tell the agent to set up string-can from this repo (or install the binary and run `serve`).
2. Each agent replies with two lines: `string-can public key:` and `string-can listen:`.
3. Check each listen URL. It must be a Tailscale address (`http://100....:8741`) or a MagicDNS name (`http://muse:8741`). If you see `127.0.0.1` or `localhost`, that agent is not reachable from the other machine — Tailscale is down, or it used `serve --local`.
4. On agent A, run the line printed by agent B's `string-can invite` (or `string-can connect <B-url>`). Do the same on B with A's invite. Both sides must add each other. `connect` tries to finish the other side when the peer is up.
5. Verify with `string-can doctor` on each side. Then send a test message from one agent and `thread` or `fetch` on the other.

Same machine: `serve --local` and `connect --local http://127.0.0.1:8741`.

## Run as a service

Sample units so `serve` survives restarts and comes back when Tailscale does:

- systemd (user): `contrib/string-can.service`
- launchd: `contrib/com.supersuit.string-can.plist`

Copy the file, set the binary path and `STRING_CAN_HOME` if needed, then enable it (`systemctl --user enable --now string-can` or `launchctl load`).
