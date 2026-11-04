# Roadmap

Each milestone has one gate, and every gate from M2 on is a developer
experience gate: something a person does in a measured number of commands
and minutes, with no file edited by hand. M0 and M1 are done. The rest is
the order in which Wormhole becomes the open, self-hosted layer between a
team's people and their agents: any agent, any machine, a person in the
loop by default, and no account anywhere.

Wormhole competes with hosted, account-bound products by being the
protocol those products keep private, and by being easier to start than
they are. The second half of that is the roadmap's job. Linux remains the
only supported desktop until someone with another one needs it; nothing
below depends on that.

## Developer experience rules

These apply to every milestone and every pull request, not to one of them.

1. **One command to try, one to join, one to ask.** `wormhole demo`,
   `wormhole join <invite>`, `wormhole ask`. Anything a team has to do
   before the first ask is counted and the count only goes down.
2. **Defaults that work.** A config key the person must set before the
   first round trip is a defect. A file they must edit by hand is a bug
   report. `wormhole init` asks, it does not hand over a template.
3. **Every error names the fix.** Exit codes 3, 4 and 5 end with the
   command or config line that resolves them, and `wormhole doctor` says
   the same thing in one place.
4. **Never type an identifier.** Threads have short references
   (`^bob/3`, the newest open thread with that peer by default), ULIDs
   accept unique prefixes, and shell completion knows handles, threads and
   flags.
5. **Same verbs everywhere.** Open, agent, snooze, decline, approve and
   deny mean one thing and are reached the same way from the CLI, `inbox
   --watch`, the inbox page, the tray, the notification buttons, the MCP
   server and the skill.
6. **The tool generates what it knows.** Permission flags for the detected
   agent version, the systemd unit, the skill install, bus credentials and
   TLS material come out of the binary, never out of a doc the person
   copies from.
7. **Agents are developers too.** Every command takes `--json` and returns
   a stable exit code, the MCP tools mirror the CLI verbs one to one, and
   an agent never needs a shell to use Wormhole.

## Milestones

| Milestone | Scope | Gate |
| --- | --- | --- |
| **M0 spike** | Run `docs/m0-spike.md` on two Linux machines with the Copilot app. *Done October 2026 on v0.2.0; results and the defects it found are in `docs/design.md`, "M0 results".* | The four open questions in `docs/design.md` have answers written into that file. |
| **M1 round trip** | Envelope, bus interface, memory and NATS adapters, thread map, config with leak guards, daemon, CLI, skill, deploy templates, CI. *Complete; M0 proved the round trip on real machines in about four seconds.* | `make test` passes with the e2e round trip on both adapters; an ask from one machine lands as a session on another. |
| **M2 five-minute start** | Today the path to a first ask is fourteen steps on two machines, a VM with root, three copied config files and a hand-edited YAML. Replace it: `wormhole serve` embeds a NATS server, writes its own accounts and TLS material, and prints an invite; `wormhole join <invite>` on each machine writes the config, stores the credentials, installs the skill into every detected agent and installs and starts the user service; `wormhole demo` runs alice and bob on one machine with no server at all. `wormhole init` becomes an interactive wizard with a `--no-input` path. Short thread references, prefix matching on ids, and fix-naming errors land here because they are what the first hour of use hits. The VM path stays documented for teams that want it. | Two people on a fresh network go from the download to an answered ask in five minutes, each typing at most three commands, with no file edited by hand. |
| **M3 synchronous asks** | A reply the asking agent can act on, without injecting a turn into a live session. `wormhole ask --wait [duration]` blocks until the reply arrives and prints it, so the agent's tool call simply returns the answer. The daemon doubles as an MCP server with `ask_peer`, `list_peers` and `inbox` tools, and `wormhole mcp install` registers it with each detected agent, so an agent needs no shell and the person edits no agent config. `reply_in_place` stays as the transcript option. | An agent asks a peer through the MCP tool and acts on a value only the reply contained, with the person having run one command to set it up. |
| **M4 agent drivers** | `internal/ghapp` becomes one driver behind an `agent` interface: detect, spawn with a kickoff, resume, open, and the permission profile that pins the shell to `wormhole reply`. Claude Code is the second driver; Codex the third. `wormhole doctor` lists the agents it found and their versions, and `wormhole profile` generates the permission flags for that version, so auto mode is one config line (`mode: auto`) rather than flags copied from a design doc. Each driver carries a recorded profile per CLI version. | A round trip where the asking session runs one agent and the answering session runs another, and auto mode turned on without the person writing a flag. |
| **M5 approvals** | A new envelope type, `approval`: a structured payload (tool, arguments, a one-line summary of what changes) plus `wormhole approve` and `wormhole deny` beside open, snooze and decline, one keystroke each in `inbox --watch` and one button each in the notification and the page. The MCP server exposes a permission-prompt tool so an agent's blocked tool call becomes an approval request on the bus. Approvals use the same caps, preamble and secret scan as asks. | An agent's tool call on one machine stays blocked until a person approves it from a different machine with one keystroke, and a deny stops it with the reason shown to the agent. |
| **M6 reach and groups** | Bridges from the bus to places people already look: Slack, Matrix, ntfy and email, each a small self-hosted process started by `wormhole bridge <kind>` with the same body cap and secret scan, read-only until the person acts. Group handles (`^backend`) with first-ack-wins pickup on a shared consumer, which lifts the one-recipient non-goal. | A person approves an M5 request from a phone with the laptop closed, and an ask to a group is answered by exactly one member. |
| **M7 safety on real traffic** | Rate caps and the secret scan tuned on a month of real use; dead-letter inspection and replay in `doctor`; an audit log export of every envelope and verb with bodies redacted; a written threat model a reviewer can read in ten minutes. | Auto mode and approvals between two trusting users with no surprises for a week, and the threat model answers every question that week raised. |
| **M8 public 1.0** | The envelope and subject scheme published as a versioned protocol document with thin Go and TypeScript clients; `wormhole serve` mints `nsc` JWT credentials so `join` and revocation never expose the operator tooling; a second production bus adapter; Helm chart; signed release artifacts from public CI; name-collision check; docs pass. | Tagged 1.0.0 with a release page, and the M2 gate still holds on the release build. |

## Why this order

- M2 first because the first five minutes decide whether there is a second
  person. Every later milestone is measured against the path M2 builds,
  which is why its gate is repeated at 1.0.
- M3 next because nothing else matters if an agent cannot use the answer.
  Blocking on the reply is the smallest change that gets there; it is also
  how every human-as-a-tool pattern works, and it does not depend on any
  undocumented app behavior.
- M4 before M5 because approvals are only worth building once more than
  one agent can raise them, and the driver interface is where each agent's
  generated permission profile lives, so M5 inherits a tested one.
- M6 is what makes a human gate usable away from the desk.
- Auth hardening, the protocol document and the chart wait until the
  envelope has stopped moving; M8 is the first release where it is frozen
  at v1.

## Not planned

- A hosted service, accounts, or telemetry of any kind.
- An IDE or a session-orchestration UI. The thread map can feed one; the
  CLI, the inbox page and the tray are the surfaces this project ships.
- Attachments. Links and pasted snippets stay the way to share material.
- macOS and Windows daemons, until a contributor on those systems needs
  them. The driver and bridge work in M4 and M6 does not assume Linux.
