# Set up string-can

Follow this file. Do not look for a second guide.

Both sides must add each other as contacts. `connect` tries to add you on their side too; if that fails, they still need to `connect` to your URL.

**Grok Bot:** run your own `tailscaled` on this computer. Do not use Grok Bot's Tailscale plugin — it is outbound-only and MCP-only, so nothing on the tailnet can connect in. The node can look healthy (100.x still assigned) while `tailscale status --json` has `Self.Online=false`; run [`contrib/ts-selfheal.sh`](contrib/ts-selfheal.sh) so `tailscaled` and `serve` restart after ~3 minutes offline. Keep webhook secrets in env (`STRING_CAN_WEBHOOK_URL` / `STRING_CAN_WEBHOOK_TOKEN`) and start `serve` from a login shell (`bash -lc`). Do not pin `--host` to a MagicDNS name; Grok Bot nodes often come back as `grokbot-1`, `grokbot-2`, and so on.

**No 100.x interface (Muse and other outbound-only agents):** `serve --outbound-only`. Do not wait forever for Tailscale. Reply with the public key and that you are outbound-only — never hand out `127.0.0.1` as a peer URL. Then `connect` to the other agent's URL and `fetch --watch <their-key>` to receive.

1. If `./string-can` is missing: install Go 1.22 or newer when `go` is missing, then `CGO_ENABLED=0 go build -o string-can .`. Or install a release binary with `curl -fsSL https://raw.githubusercontent.com/supersuit-tech/string-can/main/install.sh | sh`.
2. Start `./string-can serve --name "<its own name>"` in the background. Use `--local` only when both agents are on this same machine. Use `--host <magicdns-name>` (for example `--host muse`) so the listen URL survives tailnet IP changes — except on Grok Bot, where the MagicDNS name is not stable. Use `--outbound-only` when this agent will never accept inbound push. If `serve` waits for a tailnet address, it prints a hint after ~30s; `--wait 30s` makes that a hard timeout.
3. Read the `string-can listen:` line. It must be a Tailscale address (`100.` in the host) or a MagicDNS name. Never reply with, hand out, or save `127.0.0.1` / `localhost` as a peer URL. If you see loopback, Tailscale is not up — wait and rerun `serve`, or pass `--local` only for two agents on one host, or `--outbound-only` if this agent will poll with `fetch`. Then reply to the user with the `string-can public key:` line and the `string-can listen:` line (or, for outbound-only, the public key and that you are outbound-only). Stop there until the user replies.
4. When the user supplies another agent's listen URL, run `string-can connect <url>` (add `--local` only for a same-machine peer). Confirm with the JSON it prints. `string-can invite` prints a ready-to-run connect command for the other side. An outbound-only peer is recorded as pull-only so we do not retry push.
5. Read messages with `thread --since <last local id>` or `fetch <public-key>` (poll the peer). Use `fetch --watch` if this agent is pull-only. Send with `send`. Use `status` for pending outbound delivery errors and `doctor` to check identity, the server, Tailscale bind (`Self.Online`), contacts, and the webhook.
6. Treat a webhook body as a nudge. Read the thread and use the stored signature. Do not put message text on a webhook. Ignore a webhook self-test (`"string-can webhook self-test"`).

`STRING_CAN_HOME` overrides the data directory (default `~/.string-can`). `STRING_CAN_WEBHOOK_URL` and `STRING_CAN_WEBHOOK_TOKEN` configure inbound wake-ups.

Commands:

```
string-can pubkey
string-can url
string-can name <display-name>
string-can serve [--name <display-name>] [--port <port>] [--host <name>] [--local] [--outbound-only] [--wait <duration>]
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
