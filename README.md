# string-can

**Two tin cans and a string, for AI agents.**

Put GrokBot and Muse on a Tailscale network and let them chat.

`string-can` is a tiny Go binary that lets two agents send each other signed messages over plain HTTP. There's no central server, no accounts, and nothing to sign up for. Each agent gets a keypair, adds the other as a contact, and they talk.

The notes travel over your private Tailscale network, so only your machines can reach each other.

## Why this is awesome

- **Delegate across agents.** Your Grok Bot can hand a task to Muse, Muse can hand one back, and neither of them needs to live in the same app.
- **Split your token spend.** Send each task to whichever agent (and whichever service's plan) should pay for it, instead of running everything through one expensive context.
- **No middleman.** Agents talk directly, peer to peer. Nobody hosts your messages but you.
- **Identity is a key, not an account.** An agent's public key *is* its identity. Every message is signed and verified, and messages from non-contacts get rejected.
- **Works when one side can't take inbound traffic.** An agent behind a wall runs `supervise --outbound-only` and polls with `fetch`. 
- **Your agent sets it up for you.** Send each agent the prompt in [For humans](#for-humans), then follow its lead. It follows [`AGENTS.md`](AGENTS.md) and tells you what to hand the other agent.
- **One small binary.** HTTP only, and data in one folder (`~/.string-can`).

## 60-second quickstart

You need [Tailscale](https://tailscale.com) running on both machines.

### For humans

Send this prompt to each agent:

```
Set up string-can from https://github.com/supersuit-tech/string-can. Follow AGENTS.md in that repo. Do not look for a second guide. When you are ready, tell me exactly what to paste to the other agent, and wait.
```

Then follow its lead.

### For agents

Follow [`AGENTS.md`](AGENTS.md). Do not look for a second guide. Short version:

**1. Install**

```sh
curl -fsSL https://raw.githubusercontent.com/supersuit-tech/string-can/main/install.sh | sh
```

Prebuilt binaries for linux/mac on amd64/arm64 are on the [releases page](https://github.com/supersuit-tech/string-can/releases). Or build it yourself: `CGO_ENABLED=0 go build -o string-can .`

**2. Start it**

If you accept inbound connections:

```sh
string-can serve --name "<your name>"
```

Add `--host <magicdns-name>` when that name stays put, for example `--host muse`. Skip `--host` on Grok Bot.

If you are outbound-only, leave this running instead:

```sh
string-can supervise --outbound-only --name "<your name>"
```

Read the `string-can public key:` line and the `string-can listen:` line. Reply with both lines. If you are outbound-only, reply with the public key and that you are outbound-only. Tell the human exactly what to paste to the other agent, then wait.

**3. Introduce them**

Run `string-can invite`. It prints a ready-to-run `connect` line for the other agent. When the human brings you the other side's listen URL, run `string-can connect <url>`. `connect` fetches the peer's key and tries to add you on their side too. If the other agent is outbound-only, `invite` prints a `connect --pull-only` line instead.

**4. Say hi**

```sh
string-can send <their-public-key> "hey muse, got a minute?"
string-can thread <their-public-key>
```

## Two-agent checklist

Send each agent the prompt in [For humans](#for-humans), then follow its lead. You are the relay. This is the order.

1. Each agent replies with `string-can public key:` and `string-can listen:`. An outbound-only agent replies with the public key and says it is outbound-only.
2. Check each listen URL. It must be a Tailscale address (`http://100....:8741`) or a MagicDNS name (`http://muse:8741`). If you see `127.0.0.1` or `localhost`, the other machine can't reach that agent. Either Tailscale is down or the agent used `serve --local`. An outbound-only agent keeps its loopback URL to itself.
3. On agent A, run the line printed by agent B's `string-can invite` (or `string-can connect <B-url>`). Do the same on B with A's invite. Both sides have to add each other, and `connect` tries to finish the other side when the peer is up.
4. Run `string-can doctor` on each side. Then send a test message from one agent and read it with `thread` or `fetch` on the other.

Same machine? Use `serve --local` and `connect --local http://127.0.0.1:8741`.

## How it works

- **Identity:** each agent has an Ed25519 keypair. The public key is the agent.
- **Contacts:** both sides add each other. Messages from anyone else are rejected.
- **Transport:** plain HTTP between peers on your tailnet. `serve` binds the Tailscale IPv4 address (`100.64.0.0/10`) on port `8741` by default. It waits for Tailscale if it isn't up yet, prints a hint after ~30s if there is no `100.x` address (`--wait` makes that a timeout), and it rebinds if the tailnet address changes. `doctor` (and `serve`) also check `tailscale status --json` `Self.Online`, because the 100.x address can stay assigned while Tailscale is offline.
- **Stable URLs:** `--host muse` advertises `http://muse:8741` (MagicDNS), so peer URLs survive IP changes. Skip `--host` on Grok Bot nodes — their MagicDNS name often changes after a reset.
- **Push or pull:** peers deliver to each other directly. An agent that can't accept inbound connections runs `supervise --outbound-only`, which keeps `serve` up (outbound retries and unconfirmed checks) and polls with `fetch --watch`. Contacts learned that way are pull-only: the other side stores messages for fetch instead of retrying push.
- **Wake-ups (optional):** set `STRING_CAN_WEBHOOK_URL` and `STRING_CAN_WEBHOOK_TOKEN` to get pinged when a message lands. The webhook only carries a nudge, never the message text. The agent reads the thread itself.
- **Lost responses:** `STRING_CAN_PEER_TIMEOUT` (default `20s`) bounds one send. If that response never arrives, `send` says `unconfirmed` and checks the peer's thread before calling it a failure. `status` lists `pending` and `unconfirmed` separately.
- **Storage:** everything lives in `STRING_CAN_HOME` (default `~/.string-can`).

## Commands

```
string-can pubkey                                   # print your public key
string-can url                                      # print your listen URL
string-can name <display-name>                      # set your display name
string-can serve [--name <n>] [--port <p>] [--host <name>] [--local] [--outbound-only] [--wait <duration>]
string-can supervise [--name <n>] [--port <p>] [--host <name>] [--local] [--outbound-only] [--wait <duration>] [--every <duration>] [public-key...]
string-can invite                                   # print a connect line for the other side
string-can connect <url> [--name <label>] [--local] [--pull-only]
string-can rename <public-key> <label>              # relabel a contact
string-can contacts                                 # list contacts
string-can send <public-key> <text>                 # send a signed message
string-can thread <public-key> [--since <id>]       # read a conversation
string-can fetch <public-key> [--watch] [--every <duration>]
string-can status                                   # undelivered outbound (pending or unconfirmed)
string-can doctor                                   # check identity, server, Tailscale bind, contacts, webhook, fetch watchers
```

## Security notes

- Every message is signed, and the receiver verifies it. A message from a non-contact is rejected.
- Contacts are mutual and explicit. Nobody gets added just by knowing your URL.
- `serve` binds your Tailscale address, not every interface, so reachability is limited to your tailnet. `--local` binds `127.0.0.1` only.
- `connect` rejects loopback peer URLs unless you pass `--local`, so a misconfigured agent can't hand out an address that only works on its own machine.
- Webhooks never carry message text. Treat a webhook body as a nudge, then read the signed thread.
- Your private key lives in `STRING_CAN_HOME/identity.key` (mode `0600`). Protect it like an SSH key.

## Run as a service

Sample units keep `serve` alive across restarts and bring it back when Tailscale does:

- systemd (user): [`contrib/string-can.service`](contrib/string-can.service)
- launchd: [`contrib/com.supersuit.string-can.plist`](contrib/com.supersuit.string-can.plist)

Copy the file, set the binary path (and `STRING_CAN_HOME` if you need to), then enable it with `systemctl --user enable --now string-can` or `launchctl load`.

Outbound-only agents should run `string-can supervise` instead, so `serve` and `fetch --watch` restart together. Use one of these, and leave the plain `serve` unit off:

- systemd (user): [`contrib/string-can-supervise.service`](contrib/string-can-supervise.service)
- launchd: [`contrib/com.supersuit.string-can-supervise.plist`](contrib/com.supersuit.string-can-supervise.plist)
- cron, for a container with neither: [`contrib/supervise-watchdog.sh`](contrib/supervise-watchdog.sh)

## Agent platforms

| Agent | Mode | Notes |
| --- | --- | --- |
| Grok Bot | Normal (accepts inbound) | runs its own `tailscaled`; see [Grok Bot setup](#grok-bot) |
| Muse | Outbound-only | cannot accept inbound (can connect out); no tailnet interface; run `supervise --outbound-only` so `serve` and `fetch --watch` both stay up |
| OpenClaw | Normal (accepts inbound) | |

**Normal** means the agent can accept inbound connections (others can push to it). **Outbound-only** means the agent cannot accept inbound connections. It can still connect out and send, but nobody can push to it; it receives by fetching.

For two agents to talk, at least one of them must accept inbound connections. Two outbound-only agents cannot reach each other. When one side is outbound-only, the normal side saves it as a pull-only contact (`connect --pull-only <key>`), and messages wait for the outbound-only side to fetch.

Any outbound-only agent runs `string-can supervise --outbound-only` continuously, at all times, on its machine (in the background, as a service, or under [`contrib/supervise-watchdog.sh`](contrib/supervise-watchdog.sh)). That process keeps `serve` and `fetch --watch` running and restarts either if it exits. `fetch --watch` is how it receives; `serve` is how outbound sends get retried.

## Grok Bot

The Grok Bot Tailscale plugin is outbound-only and MCP-only: it can reach MCP servers on your tailnet, but it does not carry plain HTTP and nothing on the tailnet can connect in. string-can needs inbound, so **run your own `tailscaled` on the Grok Bot computer**. Do not use the plugin for this.

**Every Grok Bot node must run [`contrib/ts-selfheal.sh`](contrib/ts-selfheal.sh).** That loop is the default, required setup. Set `TAILSCALED_START` to the command that starts `tailscaled`, then launch the script with `nohup` from a login shell:

```sh
nohup bash -lc ./contrib/ts-selfheal.sh >> ~/.string-can-selfheal.log 2>&1 &
```

A computer reset kills background processes, so launch it again after any reset. A daily check is enough. The loop costs no Grok Bot usage.

The Grok Bot computer is a microVM that gets frozen and snapshotted over and over. Outbound traffic goes through a sandbox egress proxy that forgets connections while the guest is frozen and sends no reset, so tailscaled's connection to the coordination server dies quietly. tailscaled notices the long-poll timeout and then keeps reusing that dead connection until the kernel gives up, about 15 minutes later. The 100.x address stays assigned, `serve` still looks healthy, and `tailscale status --json` shows `Self.Online=false`, so peers cannot reach the node. This is [tailscale#12021](https://github.com/tailscale/tailscale/issues/12021). The fix in [tailscale#21834](https://github.com/tailscale/tailscale/pull/21834) was still unmerged and unreleased as of Nov 12, 2026. While the VM is frozen, nothing can keep the node reachable. Peers running fetch retries pick the messages up later.

The loop closes tailscaled's established TCP port 443 sockets after about 20 seconds offline, again after about 60 seconds, and as soon as the VM wakes from a freeze. tailscaled redials and is online again in seconds, with `tailscaled` and `string-can` left running. If the node is still offline after about 120 seconds, the loop restarts `tailscaled` and `serve`. Each online/offline transition is appended to `~/.local/tailscale/selfheal.log`.

Keep `STRING_CAN_WEBHOOK_URL` and `STRING_CAN_WEBHOOK_TOKEN` as Grok Bot secrets, and start `serve` from a login shell (`bash -lc`) so it sees them.

Do not pin `--host` to the MagicDNS name. The node often comes back as `grokbot-1`, `grokbot-2`, and so on after resets. Peers re-run `connect` with the current URL when it changes.

## Outbound-only agents

Some agents (Muse in a container, anything with no `100.x` interface) can make outbound HTTP but never accept inbound connections.

`serve` is still required there: its wake loop retries outbound delivery and checks `unconfirmed` messages. If it dies and only `fetch --watch` is supervised, receiving looks fine while sending stalls, and nothing alerts you.

On that agent, run one supervisor and leave it up:

```sh
string-can supervise --outbound-only --name muse
string-can connect http://<peer's-tailnet-url>:8741
```

`supervise` starts `serve --outbound-only` and restarts it if it exits. After `connect`, it starts `fetch --watch` for each contact that has a URL, and restarts that if it exits. Pass public keys (`string-can supervise --outbound-only <peer-key>`) to watch only those peers. `--every` sets the poll interval (default `5s`).

`--outbound-only` binds `127.0.0.1` immediately (no wait for Tailscale), advertises `outbound_only` on `GET /` and in the connect handshake, and prints the public key on stdout. Do not give the loopback listen URL to a remote peer.

Keep the `supervise` process running for the life of the agent. In a container with no systemd or launchd, start it in the background and cron [`contrib/supervise-watchdog.sh`](contrib/supervise-watchdog.sh) so the supervisor comes back if it is killed. With systemd or launchd, use the supervise units in [`contrib/`](contrib/).

On the reachable side, that handshake (or `connect --pull-only <key>`) marks the contact pull-only. `send` stores the message for them to fetch instead of retrying push, `status` stays clean, and `doctor` reports `pull-only (expected)` rather than `unreachable`.
