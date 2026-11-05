# Wormhole

Wormhole lets `ask ^bob what he thinks about X` in any GitHub Copilot app
session deliver that question into a new session on Bob's machine, and
brings his answer back to the session that asked.

One Go binary (`wormhole` CLI plus `wormhole daemon`), a message bus (NATS
JetStream first, others behind an interface) and a skill that teaches agents
the `^handle` syntax. A wormhole joins two sessions on two machines; a thread
is an open wormhole; `wormhole ask` opens one and `wormhole reply` travels
back through it.

Wormhole is an independent open-source project. Every team runs its own bus,
embedded in the binary (`wormhole serve`) or from the templates in
`deploy/`; nothing here is hosted and there is no telemetry. Explicitly enabled update checks contact GitHub Releases only.

## Status

Pre-1.0. The round trip works end to end against a real NATS server in the
test suite and was run once on two real machines (October 2026, Copilot
CLI 1.0.105): an ask became a session in the other machine's app and the
agent's reply came back in about four seconds. What that run did not show
is a reply reaching the agent already running in the session that asked;
replies therefore open a `Reply from ^bob` session by default, and
`docs/design.md` ("M0 results") has the details. Docs:
https://yjmrobert.github.io/wormhole/ (design, install, usage, roadmap).
Binaries: https://github.com/yjmrobert/wormhole/releases.

Linux only for now; macOS and Windows openers come later.

## How it works

```
alice's session ──wormhole ask──▶ bus ──▶ bob's daemon ──▶ new session on bob's machine
alice's daemon ◀── bus ◀──wormhole reply── bob's session (agent or human)
```

- **Inbox mode (default).** Bob's daemon stages the message and notifies
  him; nothing runs until he reads it. `wormhole open` runs it with his
  normal tools, `wormhole open --agent` hands it to his agent under the
  safe profile, `wormhole snooze` hides it for a while and `wormhole
  decline` tells Alice he passed. `wormhole inbox --watch` keeps the inbox
  open in a terminal with one key per verb, `daemon.inbox_ui` serves the
  same inbox as a page on this machine only, `daemon.tray` puts the count
  in the panel, and `daemon.notify` picks between a toast per message, one
  bubble with Show, Later and Decline buttons, or a digest.
- **Auto mode (opt-in per sender).** Bob's daemon runs the kickoff headless
  with a restricted tool set (`copilot.safe_tools` plus the CLI's
  permission flags in `copilot.extra_args`), so Bob's agent answers in
  seconds.
- **Replies come back as a session.** Alice's daemon opens a `Reply from
  ^bob` session with the answer. `daemon.reply_in_place: true` appends it
  to the session that asked instead (`copilot --resume`); it shows in that
  session's transcript, but an agent the app is already running there does
  not see it, which is why it is off by default.

Either way the sender is bound by the bus subject (`wh.inbox.bob.alice`) and
enforced by the server's per-user permissions, not by anything in the
message.

## Quick start

One machine hosts the bus; every other machine joins it with one command.
No file is edited by hand.

On alice's machine:

```
wormhole serve install --invite bob    # embedded NATS as a user service, this machine joined as alice, an invite for bob
```

Send bob the `wormhole join wh1...` line it printed. On bob's machine:

```
wormhole join wh1...                   # config, credential, skill, daemon service; alice may ask bob (inbox mode)
```

Then, in a Copilot session with the skill installed:

> ask ^bob whether the retry policy in PR #123 is sane

Try it alone first with `wormhole demo`: alice and bob on one machine,
no server, no second person. Teams that want the bus on a VM instead of a
laptop follow "Operating your own bus" in `docs/install.md`.

## CLI

```
wormhole demo                                  # the round trip on this machine: no server, no second person
wormhole serve [install] [--invite <handle>]   # host the bus: embedded NATS with TLS; install runs it as a user service
wormhole serve invite|users|revoke <handle>    # mint a one-time invite, list users, remove one
wormhole join <invite>                         # join a bus: config, credential, skill, daemon service
wormhole ask <user> "<prompt>" [--repo owner/name] [--context "..."] [--session <id>] [--yes]
wormhole reply <thread> "<text>"               # from inside a relayed session
wormhole send <user> --thread <ref> "<text>"   # free-form follow-up on an open thread
wormhole inbox [--watch|--ui] [--all]          # staged inbound messages waiting for you; --watch keeps it open in the terminal, --ui opens the page
wormhole open <thread> [--agent] [--yes]       # read a staged message, confirm, run it in its session and show it; --agent uses the safe profile
wormhole snooze <thread> [<duration>|off]      # hide a staged message until then; the daemon reminds you once
wormhole decline <thread> ["<reason>"]         # pass on a staged message and tell the asker
wormhole threads [--open] [--peer <user>]      # local thread map
wormhole users                                 # handles on the bus, with presence
wormhole senders [add|remove <handle> [--auto]] # who may ask you
wormhole daemon [--once] [--pause|--resume]    # the subscriber; pause is the local kill switch
wormhole service install|status|uninstall      # the daemon as a systemd user service
wormhole skill install|status [--agent <name>] # the skill in every detected agent
wormhole doctor [--deep-link]                  # bus, creds, copilot, display, dead letters; every failure names its fix
wormhole init [--no-input]                     # the config wizard (or flags for every question)
wormhole login [--creds <file>]                # install credentials privately
wormhole admin init --url <nats url>           # create the stream and registry bucket on a bus you run yourself
wormhole completion bash|zsh|fish              # generate shell completion
wormhole commands [--flags] [--json]           # discover commands and flags
wormhole man                                   # generate the command manual
wormhole update [--enable|--disable]           # update now or control daily checks
```

A `<thread>` is a thread id, a unique prefix of one, or `^bob` (the newest
open thread with bob; `^bob/2` the one before).


Every command takes `--json`, `--config <path>`, and `--no-input`, and returns
a stable exit code: 0 ok, 1 error, 2 usage, 3 refused, 4 not found, 5 bus or
app unavailable. `--no-input` never implies `--yes`.

## Safety

- Sender allowlist on the recipient; unknown senders get a rejection receipt
  and are never materialized.
- Inbox mode by default; auto mode per sender, with `--available-tools`
  limited to the configured set and the shell pinned to `wormhole reply`
  by the permission flags in `copilot.extra_args`.
- Fixed preamble marks inbound text as a colleague's request, not
  instructions, and states whether a person reviewed it.
- Secret scan on every outbound body; 32 KB cap; bodies never logged.
- Per-sender and daily caps on inbound asks; `max_hops` stops agent
  ping-pong.
- Local kill switch (`wormhole daemon --pause`), server-side kill switch
  (`wormhole serve revoke <handle>`, or disable the user in `accounts.conf`).
- The daemon refuses a world-readable config or credentials inside a git
  working tree.

See `SECURITY.md` for reporting and `docs/design.md` for the full picture.

## Releasing

A release is a `v*` tag on `main`, nothing more. `make release TAG=v0.4.0`
checks that you are on an up-to-date, clean `main`, runs the tests, tags,
and pushes the tag; the release workflow then builds the Linux tarballs
and publishes the GitHub Release. Where a tag cannot be pushed (a hosted
session behind a git proxy), dispatch the release workflow on `main` with
the version: it creates the same tag and carries on. Tags that are not on
`main` are refused.

## License

Apache-2.0.
