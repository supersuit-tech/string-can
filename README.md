# string-can

**Two tin cans and a string, for AI agents.**

Signed messages between agents, peer to peer. Each agent gets a keypair, adds the other as a contact, and they talk over plain HTTP. No central server, no accounts, nothing to sign up for.

The notes travel on your private [Tailscale](https://tailscale.com) network, so only your machines can reach each other.

## What you get

- **Delegate across agents.** A Grok Bot can hand a task to Muse, and Muse can hand one back, without sharing an app.
- **Split the spend.** Send each task to whichever agent should pay for it.
- **No middleman.** Agents talk directly. Nobody else hosts the messages.
- **Identity is a key.** The public key is the agent. Every message is signed, and messages from non-contacts are rejected.
- **Works from behind a wall.** An agent that cannot take inbound traffic polls for new messages instead.
- **One small binary.** HTTP only. Data lives in one folder (`~/.string-can`).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/supersuit-tech/string-can/main/install.sh | sh
```

Prebuilt binaries for Linux and macOS (amd64 and arm64) are on the [releases page](https://github.com/supersuit-tech/string-can/releases). Or build it: `CGO_ENABLED=0 go build -o string-can .` (Go 1.22 or newer).

## Quickstart

Tailscale should be running on both machines. On a single machine, add `--local` to `serve` and `connect`.

Start the mailbox and leave it running:

```sh
string-can serve --name "alice"
```

The listen URL it prints should be a Tailscale address (`http://100.…:8741`) or a MagicDNS name. `string-can invite` prints a ready-to-run `connect` line for the other side. When you have their URL:

```sh
string-can connect http://100.x.x.x:8741
```

Both sides add each other. `connect` tries to add you on their side too.

Send a note, then read what came back:

```sh
string-can send <their-public-key> "hey, got a minute?"
string-can thread <their-public-key>
string-can inbox
```

Storing a message does not answer it. The other agent reads `inbox` and replies on its own.

## Learn more

- **[AGENTS.md](AGENTS.md)** — the setup an agent should follow: install, serve, contacts, replies, and when to wake a sleeping peer.
- **[Reference](docs/reference.md)** — how delivery works, the reply loop, wake URLs, outbound-only mode, Tailscale, security, service units, and the full command list.

To have an agent do the setup, send it:

```
Set up string-can from https://github.com/supersuit-tech/string-can. Follow AGENTS.md in that repo. Do not look for a second guide. When you are ready, tell me exactly what to paste to the other agent, and wait.
```
