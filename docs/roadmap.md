# Roadmap

Each milestone has one gate. M0 and M1 are done; the rest is the order in
which Wormhole becomes the open, self-hosted layer between a team's people
and their agents: any agent, any machine, a person in the loop by default,
and no account anywhere.

Wormhole competes with hosted, account-bound products by being the
protocol those products keep private: an envelope, a subject scheme that
makes the server enforce who sent what, and a daemon a team runs itself.
The milestones below add the pieces that story is missing today, in the
order each one unblocks the next. Linux remains the only supported desktop
until someone with another one needs it; nothing below depends on that.

| Milestone | Scope | Gate |
| --- | --- | --- |
| **M0 spike** | Run `docs/m0-spike.md` on two Linux machines with the Copilot app. *Done October 2026 on v0.2.0; results and the defects it found are in `docs/design.md`, "M0 results".* | The four open questions in `docs/design.md` have answers written into that file. |
| **M1 round trip** | Envelope, bus interface, memory and NATS adapters, thread map, config with leak guards, daemon, CLI, skill, deploy templates, CI. *Complete; M0 proved the round trip on real machines in about four seconds.* | `make test` passes with the e2e round trip on both adapters; an ask from one machine lands as a session on another. |
| **M2 synchronous asks** | A reply the asking agent can act on, without injecting a turn into a live session. `wormhole ask --wait [duration]` blocks until the reply arrives and prints it, so the agent's tool call simply returns the answer. The daemon doubles as an MCP server (`wormhole mcp`) with `ask_peer`, `list_peers` and `inbox` tools, so an agent needs no shell to use it. `reply_in_place` stays as the transcript option. | The agent in the asking session acts on the reply without a person nudging it, verified with a value only the reply contained, on a stock desktop. |
| **M3 agent drivers** | `internal/ghapp` becomes one driver behind an `agent` interface: spawn with a kickoff, resume a session, open it, and the permission profile that pins the shell to `wormhole reply`. Claude Code is the second driver (`-p`, `--resume`, hooks); Codex the third. The skill ships in each agent's skill format. Auto-mode profiles are recorded per driver and CLI version, which closes the "tested on one version" caveat from M0. | A round trip where the asking session runs one agent and the answering session runs a different one. |
| **M4 approvals** | A new envelope type, `approval`: a structured payload (tool, arguments, a one-line risk summary) plus `wormhole approve` and `wormhole deny` beside open, snooze and decline. The MCP server exposes a permission-prompt tool so an agent's blocked tool call becomes an approval request on the bus, answered from the inbox on any machine the person's daemon runs on. Approvals use the same caps, preamble and secret scan as asks. | An agent's tool call on one machine stays blocked until a person approves it from a different machine, and a deny stops it. |
| **M5 reach and groups** | Bridges from the bus to places people already look: Slack, Matrix, ntfy and email, each a small self-hosted process with the same body cap and secret scan, read-only until the person acts. Group handles (`^backend`) with first-ack-wins pickup on a shared consumer, which lifts the one-recipient non-goal. | A person approves an M4 request from a phone with the laptop closed, and an ask to a group is answered by exactly one member. |
| **M6 onboarding and protocol** | `wormhole serve` embeds a NATS server for a team that does not want a VM yet; `wormhole demo` runs alice and bob on one machine over the memory adapter. The envelope and subject scheme are published as a versioned protocol document, with thin Go and TypeScript clients so a tool can speak Wormhole without the CLI. | Two people on a fresh network go from the home page to a working round trip in five minutes using only the docs. |
| **M7 safety on real traffic** | Rate caps and the secret scan tuned on a month of real use; dead-letter inspection in `doctor`; an audit log export of every envelope and verb with bodies redacted; a written threat model that a reviewer at a company can read in ten minutes. | Auto mode and approvals between two trusting users with no surprises for a week, and the threat model answers every question that week raised. |
| **M8 public 1.0** | `nsc` JWT auth with `wormhole login --creds` and `nsc revoke` for revocation; a second production bus adapter; Helm chart; signed release artifacts from public CI; name-collision check; docs pass. | Tagged 1.0.0 with a release page. |

## Why this order

- M2 first because nothing else matters if an agent cannot use the answer.
  Blocking on the reply is the smallest change that gets there; it is also
  how every human-as-a-tool pattern works, and it does not depend on any
  undocumented app behavior.
- M3 before M4 because approvals are only worth building once more than
  one agent can raise them. The driver interface is also where each
  agent's permission flags live, so M4 inherits a tested profile.
- M5 and M6 are the adoption milestones: reaching a phone is what makes a
  human gate usable, and a five-minute trial is what gets a second person
  to install it.
- Auth hardening and the chart wait until the protocol has stopped moving;
  M8 is the first release where the envelope is frozen at v1.

## Not planned

- A hosted service, accounts, or telemetry of any kind.
- An IDE or a session-orchestration UI. The thread map can feed one; the
  CLI, the inbox page and the tray are the surfaces this project ships.
- Attachments. Links and pasted snippets stay the way to share material.
- macOS and Windows daemons, until a contributor on those systems needs
  them. The driver and bridge work in M3 and M5 does not assume Linux.
