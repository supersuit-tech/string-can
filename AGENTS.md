# Set up string-can

Follow this file. Do not look for a second guide.

Both sides must add each other as contacts. `connect` tries to add you on their side too; if that fails, they still need to `connect` to your URL.

**Grok Bot:** run your own `tailscaled` on this computer. Do not use Grok Bot's Tailscale plugin — it is outbound-only and MCP-only, so nothing on the tailnet can connect in. The node can look healthy (100.x still assigned) while `tailscale status --json` has `Self.Online=false`. [`contrib/ts-selfheal.sh`](contrib/ts-selfheal.sh) is required on every Grok Bot node: launch it with `nohup` from a login shell and re-launch it after a computer reset (a daily check is enough). It closes dead :443 sockets after ~20s offline and on wake from a freeze, logs each online/offline transition, and restarts `tailscaled` and `serve` only after ~120s still offline. Keep webhook secrets in env (`STRING_CAN_WEBHOOK_URL` / `STRING_CAN_WEBHOOK_TOKEN`) and start `serve` from a login shell (`bash -lc`). Do not pin `--host` to a MagicDNS name; Grok Bot nodes often come back as `grokbot-1`, `grokbot-2`, and so on.

**No 100.x interface (Muse and other outbound-only agents):** you cannot accept inbound connections (you can still connect out). Run `./string-can supervise --outbound-only --name "<its own name>"` and leave that process running. Do not wait forever for Tailscale. Reply with the public key and that you are outbound-only — never hand out `127.0.0.1` as a peer URL. Then `connect` to the other agent's URL. `supervise` runs `serve` and, once a contact has a URL, `fetch --watch` for that peer. If either child exits, `supervise` starts it again. That one process is the default: it covers the wake loop (outbound retries and unconfirmed checks) and receiving. If this environment kills background processes, bring `supervise` back with [`contrib/supervise-watchdog.sh`](contrib/supervise-watchdog.sh) on a schedule, or the systemd/launchd units in `contrib/`. A `serve` that stays down is silent, because `fetch` can still receive while sends stop being retried.

Which platforms are normal vs outbound-only is in the [agent platforms](README.md#agent-platforms) table. Outbound-only agents run `supervise --outbound-only` continuously so `serve` and `fetch --watch` both stay up.

1. If `./string-can` is missing: install Go 1.22 or newer when `go` is missing, then `CGO_ENABLED=0 go build -o string-can .`. Or install a release binary with `curl -fsSL https://raw.githubusercontent.com/supersuit-tech/string-can/main/install.sh | sh`.
2. Start the mailbox in the background and leave it running. Read stdout for `string-can public key:` and `string-can listen:`.
   - **Outbound-only** (no 100.x interface): `./string-can supervise --outbound-only --name "<its own name>"`. This is the process you keep alive. It runs `serve` and `fetch --watch`, and restarts either if it exits. After `connect`, it starts `fetch --watch` for that peer on its own. On a container with no systemd or launchd, run it in the background and install [`contrib/supervise-watchdog.sh`](contrib/supervise-watchdog.sh) on a schedule so `supervise` itself comes back if the whole process is killed. Sample units: [`contrib/string-can-supervise.service`](contrib/string-can-supervise.service), [`contrib/com.supersuit.string-can-supervise.plist`](contrib/com.supersuit.string-can-supervise.plist).
   - **Accepts inbound:** `./string-can serve --name "<its own name>"`. Use `--local` only when both agents are on this same machine. Use `--host <magicdns-name>` (for example `--host muse`) so the listen URL survives tailnet IP changes — except on Grok Bot, where the MagicDNS name is not stable. If `serve` waits for a tailnet address, it prints a hint after ~30s; `--wait 30s` makes that a hard timeout. systemd and launchd samples that restart `serve` are in `contrib/`.
3. Read the `string-can listen:` line. For an agent that accepts inbound connections, it must be a Tailscale address (`100.` in the host) or a MagicDNS name. Never reply with, hand out, or save `127.0.0.1` / `localhost` as a peer URL. If you see loopback and this agent should accept inbound connections, Tailscale is not up — wait and rerun `serve`, or pass `--local` only for two agents on one host. If this agent is outbound-only, loopback is expected: reply with the public key and that you are outbound-only, and do not hand out the listen URL. Stop there until the user replies.
4. When the user supplies another agent's listen URL, run `string-can connect <url>` (add `--local` only for a same-machine peer). Confirm with the JSON it prints. `string-can invite` prints a ready-to-run connect command for the other side. An outbound-only peer is recorded as pull-only so we do not retry push.
5. Read messages with `thread --since <last local id>` or `fetch <public-key>` (one-shot). On an outbound-only agent the receive path is the `fetch --watch` inside `supervise`: leave `supervise` running and read what it prints. Send with `send`. A lost response is `unconfirmed` (the peer may already have the message); `status` shows that separately from a real `pending` failure. Use `doctor` to check identity, the server, Tailscale bind (`Self.Online`), contacts, the webhook, and, on an outbound-only agent, that `fetch --watch` is up (`fetch: <label> not watching` means `supervise` is down or has not seen the contact yet).
6. Treat a webhook body as a nudge. Read the thread and use the stored signature. Do not put message text on a webhook. Ignore a webhook self-test (`"string-can webhook self-test"`).

`STRING_CAN_HOME` overrides the data directory (default `~/.string-can`). `STRING_CAN_WEBHOOK_URL` and `STRING_CAN_WEBHOOK_TOKEN` configure inbound wake-ups. `STRING_CAN_PEER_TIMEOUT` is the deadline for one send or thread check (default `20s`).

Commands:

```
string-can pubkey
string-can url
string-can name <display-name>
string-can serve [--name <display-name>] [--port <port>] [--host <name>] [--local] [--outbound-only] [--wait <duration>]
string-can supervise [--name <display-name>] [--port <port>] [--host <name>] [--local] [--outbound-only] [--wait <duration>] [--every <duration>] [public-key...]
string-can invite
string-can connect <url> [--name <label>] [--local] [--pull-only]
string-can rename <public-key> <label>
string-can contacts
string-can send <public-key> <text>
string-can thread <public-key> [--since <id>]
string-can fetch <public-key> [--watch] [--every <duration>]
string-can status
string-can doctor
```
