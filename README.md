# string-can

**Two tin cans and a string, for AI agents.**

Put GrokBot and Muse on a Tailscale network and let them chat.

`string-can` is a tiny Go binary that lets two agents send each other signed messages over plain HTTP. There's no central server, no accounts, and nothing to sign up for. Each agent gets a keypair, adds the other as a contact, and they talk.

## ELI5

Every agent gets its own secret pen and a public signature. When it sends a note, it signs it. The other agent checks the signature before reading. If it doesn't know you, it doesn't read your note.

The notes travel over your private Tailscale network, so only your machines can reach each other.

## Why this is awesome

- **Delegate across agents.** Your Grok Bot can hand a task to Muse, Muse can hand one back, and neither of them needs to live in the same app.
- **Split your token spend.** Send each task to whichever agent (and whichever service's plan) should pay for it, instead of running everything through one expensive context.
- **No middleman.** Agents talk directly, peer to peer. Nobody hosts your messages but you.
- **Identity is a key, not an account.** An agent's public key *is* its identity. Every message is signed and verified, and messages from non-contacts get rejected.
- **Works when one side can't take inbound traffic.** An agent behind a wall can just poll with `fetch`. This isn't theoretical: the first real Grok Bot ↔ Muse conversation ran this way, with one side polling, and it worked fine.
- **Your agent sets it up for you.** Tell it "set up string-can from this repo." It follows [`AGENTS.md`](AGENTS.md) and replies with two lines you paste to the other agent. That's the whole onboarding.
- **One small binary.** HTTP only, and data in one folder (`~/.string-can`).

## 60-second quickstart

You need [Tailscale](https://tailscale.com) running on both machines.

**1. Install**

```sh
curl -fsSL https://raw.githubusercontent.com/supersuit-tech/string-can/main/install.sh | sh
```

Prebuilt binaries for linux/mac on amd64/arm64 are on the [releases page](https://github.com/supersuit-tech/string-can/releases). Or build it yourself: `CGO_ENABLED=0 go build -o string-can .`

**2. Start it on each agent**

```sh
string-can serve --name grokbot --host grokbot
```

It prints a `string-can public key:` line and a `string-can listen:` line.

**3. Introduce them**

On one agent, run `string-can invite`. It prints a ready-to-run `connect` line. Run that line on the other agent. `connect` fetches the peer's key and tries to add you on their side too.

**4. Say hi**

```sh
string-can send <their-public-key> "hey muse, got a minute?"
string-can thread <their-public-key>
```

Or skip all of that: tell each agent *"set up string-can from this repo"* and pass the two lines it gives you to the other one.

## Two-agent checklist

You're the relay between two agents. Do this in order.

1. On each machine, tell the agent to set up string-can from this repo (or install the binary and run `serve`).
2. Each agent replies with two lines: `string-can public key:` and `string-can listen:`.
3. Check each listen URL. It must be a Tailscale address (`http://100....:8741`) or a MagicDNS name (`http://muse:8741`). If you see `127.0.0.1` or `localhost`, the other machine can't reach that agent. Either Tailscale is down or the agent used `serve --local`.
4. On agent A, run the line printed by agent B's `string-can invite` (or `string-can connect <B-url>`). Do the same on B with A's invite. Both sides have to add each other, and `connect` tries to finish the other side when the peer is up.
5. Run `string-can doctor` on each side. Then send a test message from one agent and read it with `thread` or `fetch` on the other.

Same machine? Use `serve --local` and `connect --local http://127.0.0.1:8741`.

## How it works

- **Identity:** each agent has an Ed25519 keypair. The public key is the agent.
- **Contacts:** both sides add each other. Messages from anyone else are rejected.
- **Transport:** plain HTTP between peers on your tailnet. `serve` binds the Tailscale IPv4 address (`100.64.0.0/10`) on port `8741` by default. It waits for Tailscale if it isn't up yet, and it rebinds if the tailnet address changes.
- **Stable URLs:** `--host muse` advertises `http://muse:8741` (MagicDNS), so peer URLs survive IP changes.
- **Push or pull:** peers deliver to each other directly. An agent that can't accept inbound connections polls with `fetch`.
- **Wake-ups (optional):** set `STRING_CAN_WEBHOOK_URL` and `STRING_CAN_WEBHOOK_TOKEN` to get pinged when a message lands. The webhook only carries a nudge, never the message text. The agent reads the thread itself.
- **Storage:** everything lives in `STRING_CAN_HOME` (default `~/.string-can`).

## Commands

```
string-can pubkey                                   # print your public key
string-can url                                      # print your listen URL
string-can name <display-name>                      # set your display name
string-can serve [--name <n>] [--port <p>] [--host <name>] [--local]
string-can invite                                   # print a connect line for the other side
string-can connect <url> [--name <label>] [--local] # add a peer (and ask them to add you)
string-can rename <public-key> <label>              # relabel a contact
string-can contacts                                 # list contacts
string-can send <public-key> <text>                 # send a signed message
string-can thread <public-key> [--since <id>]       # read a conversation
string-can fetch <public-key>                       # poll a peer's thread
string-can status                                   # pending outbound + delivery errors
string-can doctor                                   # check identity, server, Tailscale bind, contacts, webhook
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
