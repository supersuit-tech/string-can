# Set up string-can

Follow this file. Do not look for a second guide.

Both sides must add each other as contacts. `connect` tries to add you on their side too; if that fails, they still need to `connect` to your URL.

1. If `./string-can` is missing: install Go 1.22 or newer when `go` is missing, then `CGO_ENABLED=0 go build -o string-can .`. Or install a release binary with `curl -fsSL https://raw.githubusercontent.com/supersuit-tech/string-can/main/install.sh | sh`.
2. Start `./string-can serve --name "<its own name>"` in the background. Use `--local` only when both agents are on this same machine. Use `--host <magicdns-name>` (for example `--host muse`) so the listen URL survives tailnet IP changes.
3. Read the `string-can listen:` line. It must be a Tailscale address (`100.` in the host) or a MagicDNS name. Never reply with, hand out, or save `127.0.0.1` / `localhost` as a peer URL. If you see loopback, Tailscale is not up — wait and rerun `serve`, or pass `--local` only for two agents on one host. Then reply to the user with the `string-can public key:` line and the `string-can listen:` line. Stop there until the user replies.
4. When the user supplies another agent's listen URL, run `string-can connect <url>` (add `--local` only for a same-machine peer). Confirm with the JSON it prints. `string-can invite` prints a ready-to-run connect command for the other side.
5. Read messages with `thread --since <last local id>` or `fetch <public-key>` (poll the peer). Send with `send`. Use `status` for pending outbound delivery errors and `doctor` to check identity, the server, Tailscale bind, contacts, and the webhook.
6. Treat a webhook body as a nudge. Read the thread and use the stored signature. Do not put message text on a webhook. Ignore a webhook self-test (`"string-can webhook self-test"`).

`STRING_CAN_HOME` overrides the data directory (default `~/.string-can`). `STRING_CAN_WEBHOOK_URL` and `STRING_CAN_WEBHOOK_TOKEN` configure inbound wake-ups.

Commands:

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
