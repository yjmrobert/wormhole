# Wormhole

Wormhole lets `ask ^bob what he thinks about X` in any GitHub Copilot app
session deliver that question into a new session on Bob's machine, and
brings his answer back to the session that asked.

One Go binary (`wormhole` CLI plus `wormhole daemon`), a message bus (NATS
JetStream first, others behind an interface) and a skill that teaches
agents the `^handle` syntax. A wormhole joins two sessions on two machines;
a thread is an open wormhole; `wormhole ask` opens one and `wormhole reply`
travels back through it.

```
alice's session ──wormhole ask──▶ bus ──▶ bob's daemon ──▶ new session on bob's machine
alice's daemon ◀── bus ◀──wormhole reply── bob's session (agent or human)
```

Wormhole is an independent open-source project under Apache-2.0. Every
team runs its own bus from the shipped templates; nothing is hosted and the
binary never phones home.

## Status

Pre-1.0. The round trip works end to end against a real NATS server in the
test suite. The pieces that touch the Copilot app (`copilot -p`, the deep
link, `--available-tools`) are wired but still need the [M0 spike](m0-spike.md)
to be run on real machines. Linux only for now.

## Where to go

- [Install and configure](install.md): the bus, the daemon, the skill.
- [Using it](usage.md): the CLI, delivery modes, what the recipient sees.
- [Design](design.md): architecture, envelope, security model.
- [Roadmap](roadmap.md): milestones M0 to M5.
- [Releases](https://github.com/yjmrobert/wormhole/releases): Linux
  binaries with checksums.
- [Security policy](https://github.com/yjmrobert/wormhole/blob/main/SECURITY.md):
  private reporting.

## Safety in one paragraph

Inbound asks from a sender run nothing until a person opens them unless
that sender was explicitly opted into auto mode, and auto mode runs with a
configured read-only tool set. The sender of every message is bound by the
bus subject and enforced by the server's per-user permissions, not by the
message. Bodies are secret-scanned on the way out, capped at 32 KB, never
logged, and handed to the Copilot CLI as a single argument. Per-sender and
daily caps plus a hop limit stop runaway agent ping-pong.
