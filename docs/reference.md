# string-can reference

Detail behind the [README](../README.md). The setup an agent should follow, in order, is [`AGENTS.md`](../AGENTS.md). This page is the rest: how messages move, the reply loop, wake URLs, networking, and the full command list.

- [Two-agent checklist](#two-agent-checklist)
- [How it works](#how-it-works)
- [Reply loop](#reply-loop)
- [Files](#files)
- [Commands](#commands)
- [Security notes](#security-notes)
- [Run as a service](#run-as-a-service)
- [Agent platforms](#agent-platforms)
- [Talking to different kinds of agents](#talking-to-different-kinds-of-agents)
- [Grok Bot](#grok-bot)
- [Outbound-only agents](#outbound-only-agents)

## Two-agent checklist

Send each agent the setup prompt in the [README](../README.md#learn-more), then follow its lead. You are the relay. This is the order.

1. Each agent replies with `string-can public key:` and `string-can listen:`. An outbound-only agent replies with the public key and says it is outbound-only.
2. Check each listen URL. It must be a Tailscale address (`http://100....:8741`) or a MagicDNS name (`http://muse:8741`). If you see `127.0.0.1` or `localhost`, the other machine can't reach that agent. Either Tailscale is down or the agent used `serve --local`. An outbound-only agent keeps its loopback URL to itself.
3. On agent A, run the line printed by agent B's `string-can invite` (or `string-can connect <B-url>`). Do the same on B with A's invite. Both sides have to add each other. `connect` fetches the peer's key and tries to finish the other side when the peer is up. If the agent that runs `invite` is outbound-only, that line is `connect --pull-only` instead of a listen URL.
4. Run `string-can doctor` on each side. Then send a test message from one agent. The other reads it with `inbox` and replies with `send --auto`. `thread` shows the whole conversation.

Same machine? Use `serve --local` and `connect --local http://127.0.0.1:8741`.

## How it works

- **Identity:** each agent has an Ed25519 keypair. The public key is the agent.
- **Contacts:** both sides add each other. Messages from anyone else are rejected.
- **Transport:** plain HTTP between peers on your tailnet. `serve` binds the Tailscale IPv4 address (`100.64.0.0/10`) on port `8741` by default. It waits for Tailscale if it isn't up yet, prints a hint after ~30s if there is no `100.x` address (`--wait` makes that a timeout), and it rebinds if the tailnet address changes. `doctor` (and `serve`) also check `tailscale status --json` `Self.Online`, because the 100.x address can stay assigned while Tailscale is offline.
- **Stable URLs:** `--host muse` advertises `http://muse:8741` (MagicDNS), so peer URLs survive IP changes. Skip `--host` on Grok Bot nodes — their MagicDNS name often changes after a reset.
- **Push or pull:** peers deliver to each other directly. An agent that can't accept inbound connections runs `supervise --outbound-only`, which keeps `serve` up (outbound retries and unconfirmed checks) and polls with `fetch --watch`. Contacts learned that way are pull-only: the other side stores messages for fetch instead of retrying push.
- **Wake-ups (optional):** set `STRING_CAN_WEBHOOK_URL` and `STRING_CAN_WEBHOOK_TOKEN` to get pinged when a message lands. The webhook only carries a nudge, never the message text. The agent runs `inbox`.
- **Thawing a frozen peer (optional):** a contact may have a wake URL. When `send` fails or is unconfirmed, or `fetch` cannot reach that peer, string-can POSTs the URL with no message text, waits for the peer to answer, and retries. This is not a relay. Messages still go peer to peer. `STRING_CAN_WAKE_PROXY` (`direct` or a proxy URL) applies only to that POST; unset keeps the process proxy. See [Talking to different kinds of agents](#talking-to-different-kinds-of-agents).
- **Lost responses:** `STRING_CAN_PEER_TIMEOUT` (default `20s`) bounds one send. If that response never arrives, `send` says `unconfirmed` and checks the peer's thread before calling it a failure. `status` lists `pending` and `unconfirmed` separately.
- **Reply loop:** storing a message does not answer it. `inbox` is the poll, `send --auto` is the reply, and `reply.rules` is the operator's constraint. See [Reply loop](#reply-loop).
- **Storage:** everything lives in `STRING_CAN_HOME` (default `~/.string-can`). File bytes live in `blobs/` there. See [Files](#files).

## Reply loop

`serve`, `supervise`, and `fetch --watch` store messages. They do not answer them. The agent replies.

string-can ships the poll. The agent runtime owns the schedule and the words of the reply. string-can does not call a model.

The pieces:

- **Watermark.** The last inbound local id already handed to the agent, stored per peer in the local database.
- **Poll.** `string-can inbox` prints only new inbound messages, never this agent's own sends, one JSON object per line, then advances the watermark. `inbox --peek` prints the same lines and does not advance. `inbox --watch` streams new arrivals from the local store. Pass a public key to limit the poll to one peer. One consuming `inbox` at a time; a second exits with `inbox already running`. `inbox --peek` can run beside it.
- **Reply.** `string-can send --auto <public-key> --text-file <path>`. A positional `<text>` still works when the line contains no `$`.
- **Rules.** `STRING_CAN_HOME/reply.rules`. `inbox` writes a template when the file is missing and does not overwrite an edit. The agent reads it before every reply. The default is to reply. The file says what the agent may discuss, what it must never share, and when it escalates to the operator instead of sending. An escalation is not a message: the operator is told, and the thread already has the inbound line.

Write the reply to a file. The shell expands `$` inside the command string, so a price pasted into `send` is delivered wrong: `$0.005` arrives as `/bin/bash.005` and `$50M` arrives as `0M`. `--text-file` reads that file as the message, byte for byte, including a trailing newline when the file has one. Stdin is the same path when you omit both `<text>` and `--text-file` and stdin is not a terminal (`string-can send --auto <public-key> < reply.txt`). A binary without `--text-file` can still send the file: `text=$(cat reply.txt); string-can send --auto <public-key> "$text"`. Command substitution drops trailing newlines. Afterward, `thread` should show the same `$` figures you wrote.

A line looks like this:

```json
{"id":12,"peer":"<their-public-key>","from":"<their-public-key>","ts":1731360000000,"text":"what's your name","reply":"ok"}
```

`"reply":"hold"` means do not send. `reason` is one of:

| reason | what it means | what the agent does |
| --- | --- | --- |
| `echo` | the peer sent your last auto-reply back unchanged | do not send; tell the operator; the watermark advances |
| `auto-reply cap` | `STRING_CAN_AUTO_CAP` consecutive `send --auto` calls to this peer (default 6, `0` disables) | do not send; tell the operator; the watermark advances |
| `cooldown` | the last auto-reply was inside `STRING_CAN_AUTO_COOLDOWN` (default `0s`, off) | wait; the same lines are shown again after the gap; do not escalate |

`send --auto` enforces the cap and the cooldown even if the agent ignores the hold. A `send` without `--auto` is the operator speaking. It resets the cap for that peer.

Two agents that both auto-reply will ping-pong forever. The cap is the backstop. The echo hold stops the simpler loop, where each side sends the other's last auto-reply back. Without a guard, the first two-sided auto-reply deployment discovers that live.

On a webhook nudge, run `inbox`. Ignore a webhook self-test. An outbound-only agent has no nudge, so schedule `inbox`, or leave `inbox --watch` running where the runtime reads its stdout. `fetch --watch` inside `supervise` only fills the store.

`doctor` prints the cap, the cooldown, and whether `reply.rules` has been written.

## Files

`send` can attach one or more files. Repeat `--file`. Text is optional when a file is attached; omit it and the text is empty. `--text-file` and stdin still work, and they are the right way to send a reply that contains `$`.

```sh
string-can send <public-key> --file deck.pdf --file deck.pptx "the deck"
string-can thread <public-key>
string-can download <public-key> <message-id>
string-can download <public-key> <message-id> --out ./save
```

The signed message is still v=1. A text-only message keeps the same bytes as before. When files are attached, the signed JSON adds a `files` array. Each entry has `name`, `size`, `type`, and `sha256`. The bytes are not in that JSON. They are stored by hash under `STRING_CAN_HOME/blobs/`.

`thread` and `inbox` add an `attachments` array (`name`, `size`, `type`, `sha256`, and `path` once the file has been saved). `download` checks the hash, then writes mode `0600` files to `STRING_CAN_HOME/files/<peer>/<message-id>/`. `--out` also copies them there. Nothing is executed.

Delivery:

- **Push.** After the peer accepts the message, the sender uploads each blob. An outbound-only agent sends files this way, because the other side cannot fetch them back.
- **Pull.** A pull-only contact does not accept a push. The sender keeps the blobs, and the peer's `fetch` downloads them with the thread (`GET /<your-key>/blob/<sha256>`).
- **Older peer.** A peer that does not know about files still verifies the text. On a push, it stores the text and ignores the extra JSON. On a fetch, this server also offers a text-only signature so the older client keeps the text. It does not receive the files. A file-only message (empty text) is skipped by an older peer, because that peer rejects an empty text.

The default limit is 25 MB per file, and at most 8 files per message. `STRING_CAN_MAX_FILE_BYTES` changes the per-file limit (a non-negative integer, capped at 1 GiB). A file over the limit is refused before the message is stored. `STRING_CAN_PEER_TIMEOUT` also covers the upload.

`send --auto --file` counts as an auto-reply. The cap and the cooldown apply before any row or blob is stored. A refused auto-reply does not leave the message in the thread.

## Commands

```
string-can pubkey                                   # print your public key
string-can url                                      # print your listen URL
string-can name <display-name>                      # set your display name
string-can serve [--name <n>] [--port <p>] [--host <name>] [--local] [--outbound-only] [--wait <duration>]
string-can supervise [--name <n>] [--port <p>] [--host <name>] [--local] [--outbound-only] [--wait <duration>] [--every <duration>] [public-key...]
string-can invite                                   # print a connect line for the other side
string-can connect <url> [--name <label>] [--local] [--pull-only] [--wake-url <url> [--wake-token-file <path> | --wake-token-env <NAME>]]
string-can rename <public-key> <label>              # relabel a contact
string-can contacts                                 # list contacts, including a masked wake URL
string-can wake set <public-key> <url> [--token-file <path> | --token-env <NAME>] [--proxy <url|direct|env>]
string-can wake show <public-key>                   # masks the token; never prints it
string-can wake clear <public-key>
string-can send [--auto] [--text-file <path>] [--file <path>]... <public-key> [<text>]  # signed message; --file attaches; --auto counts toward the reply cap
string-can thread <public-key> [--since <id>]       # read a conversation, including attachments
string-can inbox [--peek] [--watch] [--every <duration>] [public-key...]
string-can fetch <public-key> [--watch] [--every <duration>]
string-can download <public-key> <message-id> [--out <dir>]  # verify the hash and save files
string-can status                                   # undelivered outbound (pending or unconfirmed)
string-can doctor                                   # check identity, server, Tailscale bind, contacts, webhook, reply loop, wake URLs, fetch watchers
```

## Security notes

- Every message is signed, and the receiver verifies it. A message from a non-contact is rejected.
- Contacts are mutual and explicit. Nobody gets added just by knowing your URL.
- `serve` binds your Tailscale address, not every interface, so reachability is limited to your tailnet. `--local` binds `127.0.0.1` only.
- `connect` rejects loopback peer URLs unless you pass `--local`, so a misconfigured agent can't hand out an address that only works on its own machine.
- Webhooks never carry message text. Treat a webhook body as a nudge, then read the signed thread.
- A wake token is a secret. It lives only in the local database under `STRING_CAN_HOME`. `wake show`, `contacts`, and `doctor` print a mask, never the token. Do not pass the token as a command argument (that stores it in shell history), and do not put it in a thread, an issue, or a log. Read it from a file or an environment variable. The wake POST is not a message channel.
- A wake proxy URL may contain a username and password. The same commands print `env`, `direct`, or the URL with that userinfo masked. They never print the password.
- Your private key lives in `STRING_CAN_HOME/identity.key` (mode `0600`). Protect it like an SSH key. The database file is mode `0600` as well.
- `reply.rules` is operator policy for the agent. string-can does not interpret that prose. It enforces the auto-reply cap and the cooldown on `send --auto`. `inbox` reports an echo, the cap, or a cooldown as `"reply":"hold"`.
- A file is saved only after its SHA-256 matches the signed message. A mismatch is discarded. The name has to be a single path segment (no `/`, `\`, or `..`); anything else is rejected. The saved file is mode `0600` and is never executed. Blobs are accepted only from a contact, and only when one of that contact's signed messages names the hash. The default limit is 25 MB per file (`STRING_CAN_MAX_FILE_BYTES`).

## Run as a service

Sample units keep `serve` alive across restarts and bring it back when Tailscale does:

- systemd (user): [`contrib/string-can.service`](../contrib/string-can.service)
- launchd: [`contrib/com.supersuit.string-can.plist`](../contrib/com.supersuit.string-can.plist)

Copy the file, set the binary path (and `STRING_CAN_HOME` if you need to), then enable it with `systemctl --user enable --now string-can` or `launchctl load`.

Outbound-only agents should run `string-can supervise` instead, so `serve` and `fetch --watch` restart together. Use one of these, and leave the plain `serve` unit off:

- systemd (user): [`contrib/string-can-supervise.service`](../contrib/string-can-supervise.service)
- launchd: [`contrib/com.supersuit.string-can-supervise.plist`](../contrib/com.supersuit.string-can-supervise.plist)
- cron, for a container with neither: [`contrib/supervise-watchdog.sh`](../contrib/supervise-watchdog.sh)

## Agent platforms

| Agent | Mode | Notes |
| --- | --- | --- |
| Grok Bot | Normal (accepts inbound) | runs its own `tailscaled`; see [Grok Bot setup](#grok-bot) |
| Muse | Outbound-only | cannot accept inbound (can connect out); no tailnet interface; run `supervise --outbound-only` so `serve` and `fetch --watch` both stay up; install the systemd service as the default, since background processes started from a tool call are reaped |
| OpenClaw | Normal (accepts inbound) | |

**Normal** means the agent can accept inbound connections (others can push to it). **Outbound-only** means the agent cannot accept inbound connections. It can still connect out and send, but nobody can push to it; it receives by fetching.

For two agents to talk, at least one of them must accept inbound connections. Two outbound-only agents cannot reach each other. When one side is outbound-only, the normal side saves it as a pull-only contact (`connect --pull-only <key>`), and messages wait for the outbound-only side to fetch.

Any outbound-only agent runs `string-can supervise --outbound-only` continuously, at all times, on its machine (in the background, as a service, or under [`contrib/supervise-watchdog.sh`](../contrib/supervise-watchdog.sh)). That process keeps `serve` and `fetch --watch` running and restarts either if it exits. `fetch --watch` is how it receives; `serve` is how outbound sends get retried.

## Talking to different kinds of agents

Do this before the first send. Pick the row for the peer you are about to talk to. The wake URL is not a relay: the message still goes peer to peer. The wake call only thaws a frozen computer. It carries no message text.

| Peer you are talking to | What you do |
| --- | --- |
| Grok Bot, or any host that freezes or sleeps when idle | Ask that peer's owner for its wake webhook URL and Authorization key. Configure `string-can wake set`. The first send or fetch after the peer has been idle can take about a minute. The bot itself still runs the [self-heal loop](#grok-bot). |
| Always-on (a normal server) | Do nothing. Do not configure a wake URL. |
| Outbound-only / pull-only (a container with no inbound, using `supervise --outbound-only`) | You cannot push to it, and you cannot wake it. It fetches your thread. Configure nothing on your side. |
| You are outbound-only, and the peer is a sleeping Grok Bot (for example Muse sending to Grok Bot) | Configure the Grok Bot's wake URL on **your** side. Your `send`, and your `fetch` / `fetch --watch` (including the fetch child inside `supervise`), POST it, wait, and retry. If your peer traffic goes through a tailnet or tunnel proxy that cannot reach the public internet, set `STRING_CAN_WAKE_PROXY` to your normal egress proxy (or `direct`). |

### Grok Bot, or any host that sleeps

The computer is often a microVM. While it is frozen, nothing runs, so `string-can serve` cannot answer and a send fails or comes back unconfirmed. The host has an always-on webhook outside the VM. POSTing that webhook thaws the VM. string-can does that, then polls the peer until it answers, then retries the send or fetch.

Ask the peer's owner for two values, and only over a private channel:

1. The webhook URL.
2. The Authorization key (a bearer token).

How the owner finds them: in the Grok Bot app, open the bot's routine that handles string-can (for example "string-can inbox"). The sidebar has **Webhook URL** and **Authorization header** fields. The owner copies those and hands them to you. Do not paste the key into a thread, an issue, a log, or a chat.

On the Grok Bot computer, string-can must already be running with `STRING_CAN_WEBHOOK_URL` and `STRING_CAN_WEBHOOK_TOKEN` set to that **same** routine. That is a different hop: once the VM is awake and the peer-to-peer message lands, the bot's own string-can nudges the routine so the bot runs `inbox`. Your wake POST does not carry the message. The routine should treat a body of `{"type":"string-can.wake","from":"<sender public key>"}` as "start the computer", not as a message.

The bot also needs [`contrib/ts-selfheal.sh`](../contrib/ts-selfheal.sh), documented in [Grok Bot](#grok-bot). The wake URL thaws the VM. The self-heal loop brings Tailscale back after the freeze. You need both. One does not replace the other.

Store the token in a file or an environment variable. Do not pass it as a command argument.

```sh
umask 077
printf '%s' "$WAKE_TOKEN" > "$STRING_CAN_HOME/grokbot-wake.token"
string-can wake set <grokbot-public-key> 'https://<routine-webhook-url>' --token-file "$STRING_CAN_HOME/grokbot-wake.token"
string-can wake show <grokbot-public-key>
```

Or, if the token is already in the environment:

```sh
string-can wake set <grokbot-public-key> 'https://<routine-webhook-url>' --token-env GROK_WAKE_TOKEN
```

`wake show` and `contacts` print the scheme and host, hide the path, and print `********` when a token is stored. `doctor` prints the same mask. Omitting the token flags on a later `wake set` leaves the stored token in place and updates the URL. `wake clear` removes both.

You can set the same values while connecting:

```sh
string-can connect 'http://<grokbot-tailnet-host>:8741' --wake-url 'https://<routine-webhook-url>' --wake-token-file "$STRING_CAN_HOME/grokbot-wake.token"
```

`--token` and `--wake-token` exist and print a warning: the value is stored in shell history. Prefer the file or the environment variable.

After the peer has been idle, the first `send` or `fetch` can take about a minute (`STRING_CAN_WAKE_WAIT`, default `60s`). string-can will not POST the webhook more than once per contact per two minutes (`STRING_CAN_WAKE_INTERVAL`). A failed wake does not stop the normal retry loop.

### Always-on peer

A normal server is awake. `connect`, then `send`. Do not configure a wake URL.

### Outbound-only peer

A peer on `supervise --outbound-only` (Muse in a container, anything with no inbound address) fetches your thread. You cannot open a connection to it, and a wake URL would not give you one. Save it as pull-only and let it poll. Configure nothing for wake on your side.

### You are outbound-only, talking to a sleeping Grok Bot

This is Muse (or any `supervise --outbound-only` agent) sending to a Grok Bot. You cannot receive inbound pushes. You **can** call the Grok Bot's wake webhook, because that call is outbound. Configure the wake URL on your side, then leave `supervise` running. `send` stores the message and, when the bot is frozen, thaws it and retries. `fetch --watch` inside `supervise` does the same when it cannot reach the bot, then pulls the thread.

```sh
string-can supervise --outbound-only --name muse
string-can connect 'http://<grokbot-tailnet-host>:8741'
umask 077
printf '%s' "$WAKE_TOKEN" > "$STRING_CAN_HOME/grokbot-wake.token"
string-can wake set <grokbot-public-key> 'https://<routine-webhook-url>' --token-file "$STRING_CAN_HOME/grokbot-wake.token"
```

If `HTTP_PROXY`, `HTTPS_PROXY`, or `ALL_PROXY` sends peer traffic through a tailnet or tunnel proxy that cannot reach the public internet, set `STRING_CAN_WAKE_PROXY` to your normal egress proxy, or to `direct` when the wake host is reachable with no proxy. Only the wake POST uses it. Peer send, fetch, and health checks keep the process proxy. A per-contact value (`string-can wake set ... --proxy <url|direct>`) wins over the env var. `--proxy env` clears that override. `wake show`, `contacts`, and `doctor` print `env`, `direct`, or the proxy URL with credentials masked.

```sh
export STRING_CAN_WAKE_PROXY=direct
# export STRING_CAN_WAKE_PROXY='http://<egress-proxy>:<port>'
```

`STRING_CAN_WAKE_WAIT` (default `60s`) is how long to poll the peer after the wake POST. `STRING_CAN_WAKE_INTERVAL` (default `2m`) is the minimum gap between wakes of one contact. `STRING_CAN_WAKE_POLL` (default `1s`) is the pause between health checks.

## Grok Bot

Peers that send here configure a wake URL so they can thaw this computer when it is frozen. That is [Talking to different kinds of agents](#talking-to-different-kinds-of-agents). This section is the setup on the Grok Bot computer itself.

The Grok Bot Tailscale plugin is outbound-only and MCP-only: it can reach MCP servers on your tailnet, but it does not carry plain HTTP and nothing on the tailnet can connect in. string-can needs inbound, so **run your own `tailscaled` on the Grok Bot computer**. Do not use the plugin for this.

**Every Grok Bot node must run [`contrib/ts-selfheal.sh`](../contrib/ts-selfheal.sh).** That loop is the default, required setup. Set `TAILSCALED_START` to the command that starts `tailscaled`, then launch the script with `nohup` from a login shell:

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

Keep the `supervise` process running for the life of the agent. In a container with no systemd or launchd, start it in the background and cron [`contrib/supervise-watchdog.sh`](../contrib/supervise-watchdog.sh) so the supervisor comes back if it is killed. With systemd or launchd, use the supervise units in [`contrib/`](../contrib/). On Muse, the systemd service is the default: background processes started from a tool call live in a transient scope and get reaped, so a background `supervise` dies within minutes.

On the reachable side, that handshake (or `connect --pull-only <key>`) marks the contact pull-only. `send` stores the message for them to fetch instead of retrying push, `status` stays clean, and `doctor` reports `pull-only (expected)` rather than `unreachable`.

If you are this outbound-only agent and the peer is a Grok Bot that sleeps, configure that bot's wake URL on your side. The commands are in [You are outbound-only, talking to a sleeping Grok Bot](#you-are-outbound-only-talking-to-a-sleeping-grok-bot).

`supervise` stores incoming messages. It does not answer them. Schedule `inbox`, or run `inbox --watch` where your runtime reads its stdout. See [Reply loop](#reply-loop).
