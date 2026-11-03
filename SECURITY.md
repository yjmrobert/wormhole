# Security policy

Wormhole moves prompts between machines and lets inbound text start an
agent session on someone else's computer. Treat every report seriously.

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use GitHub's
private vulnerability reporting on this repository ("Report a vulnerability"
under the Security tab). You will get an acknowledgement within a week and a
fix or a mitigation plan within 30 days for anything that lets inbound text
run tools without the recipient's consent, lets one bus user impersonate or
read another, or leaks a body outside the two machines on a thread.

## What is in scope

- The `wormhole` binary: CLI, daemon, bus adapters, envelope handling.
- The shipped deployment templates under `deploy/`.
- The `wormhole` skill, where it tells agents how to treat inbound text.

Out of scope: the Copilot app and CLI themselves, NATS server bugs, and
deployments that changed the shipped permissions.

## Design properties you can rely on

- A message's sender is bound by the bus subject (`wh.inbox.<to>.<from>`)
  and enforced by the server's per-user permissions, not by the envelope.
- Inbound asks from a sender run nothing until a person opens them
  (inbox mode) unless that sender was explicitly opted into auto mode, and
  auto mode runs with the configured read-only tool set only.
- Bodies are never logged at info level and are passed to the Copilot CLI
  as a single argument, never through a shell.
- The daemon refuses to start when its config is readable by other users or
  a credentials file sits inside a git working tree.
- The binary sends no telemetry. Update checks are opt-in, contact GitHub
  Releases only, and can be disabled with `wormhole update --disable` or
  `WORMHOLE_NO_UPDATE_CHECK=1`.
- A downloaded update is installed only if its SHA-256 matches the
  release's `checksums.txt`. That guards against a corrupt or truncated
  download, not against a bad release: the checksums come from the same
  GitHub release as the archive, and releases are not signed. Anyone who
  can publish a release on this repository can ship a binary that the
  updater accepts. If that is not acceptable for your deployment, leave
  automatic checks off and install from the release page by hand.
