# Wormhole

Wormhole lets `ask ^bob what he thinks about X` in any GitHub Copilot app
session deliver that question into a new session on Bob's machine, and
brings his answer back to the session that asked.

One Go binary (`wormhole` CLI plus `wormhole daemon`), a message bus (NATS
JetStream first, others behind an interface) and a skill that teaches agents
the `^handle` syntax. A wormhole joins two sessions on two machines; a thread
is an open wormhole; `wormhole ask` opens one and `wormhole reply` travels
back through it.

Wormhole is an independent open-source project. Every team runs its own bus
from the templates in `deploy/`; nothing here is hosted and the binary never
phones home.

## Status

Pre-1.0. The round trip works end to end against a real NATS server in the
test suite; the pieces that touch the Copilot app (`copilot -p`, the
`ghapp://` deep link, `--available-tools`) are wired but still need the M0
spike in `docs/m0-spike.md` to be run on real machines. See `docs/design.md`
for the design and `docs/roadmap.md` for milestones.

Linux only for now; macOS and Windows openers come later.

## How it works

```
alice's session ──wormhole ask──▶ bus ──▶ bob's daemon ──▶ new session on bob's machine
alice's daemon ◀── bus ◀──wormhole reply── bob's session (agent or human)
```

- **Inbox mode (default).** Bob's daemon stages the message and notifies
  him; nothing runs until he opens it with `wormhole open`.
- **Auto mode (opt-in per sender).** Bob's daemon runs the kickoff headless
  with a read-only tool set, so Bob's agent answers in seconds.

Either way the sender is bound by the bus subject (`wh.inbox.bob.alice`) and
enforced by the server's per-user permissions, not by anything in the
message.

## Quick start

On a VM every machine can reach:

```
cp deploy/nats/nats-server.conf.example /etc/nats/nats-server.conf
cp deploy/nats/accounts.conf.example    /etc/nats/accounts.conf   # edit users and passwords
nats-server -c /etc/nats/nats-server.conf
wormhole admin init --url tls://bus.example.internal:4222 --user admin
```

On each machine:

```
go install github.com/yjmrobert/wormhole/cmd/wormhole@latest
wormhole init                       # writes ~/.config/wormhole/config.yaml (mode 600)
$EDITOR ~/.config/wormhole/config.yaml   # handle, bus.url, senders
wormhole login                      # stores the bus password privately and checks the connection
wormhole doctor
systemctl --user enable --now wormhole   # see deploy/systemd/wormhole.service
```

Then, in a Copilot session with the `wormhole` skill installed:

> ask ^bob whether the retry policy in PR #123 is sane

## CLI

```
wormhole ask <user> "<prompt>" [--repo owner/name] [--context "..."] [--yes]
wormhole reply <thread_id> "<text>"            # from inside a relayed session
wormhole send <user> --thread <id> "<text>"    # free-form follow-up on an open thread
wormhole inbox                                 # staged inbound messages waiting for you
wormhole open <thread_id>                      # start/show the session; prints the --resume fallback
wormhole threads [--open] [--peer <user>]      # local thread map
wormhole users                                 # handles on the bus, with presence
wormhole daemon [--once] [--pause|--resume]    # the subscriber; pause is the local kill switch
wormhole doctor [--deep-link]                  # bus, creds, copilot, display, dead letters
wormhole login [--creds <file>]                # install credentials privately
wormhole admin init --url <nats url>           # create the stream and registry bucket
```

Every command takes `--json` and returns a stable exit code: 0 ok, 1 error,
2 usage, 3 refused, 4 not found, 5 bus or app unavailable.

## Safety

- Sender allowlist on the recipient; unknown senders get a rejection receipt
  and are never materialized.
- Inbox mode by default; auto mode per sender, with `--available-tools`
  limited to the configured read-only set.
- Fixed preamble marks inbound text as a colleague's request, not
  instructions, and states whether a person reviewed it.
- Secret scan on every outbound body; 32 KB cap; bodies never logged.
- Per-sender and daily caps on inbound asks; `max_hops` stops agent
  ping-pong.
- Local kill switch (`wormhole daemon --pause`), server-side kill switch
  (disable the user in `accounts.conf`).
- The daemon refuses a world-readable config or credentials inside a git
  working tree.

See `SECURITY.md` for reporting and `docs/design.md` for the full picture.

## License

Apache-2.0.
