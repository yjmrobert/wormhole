# Roadmap

Each milestone has one gate. M0 is a spike on real machines; M5 is the
public 1.0.

| Milestone | Scope | Gate |
| --- | --- | --- |
| **M0 spike** | Run `docs/m0-spike.md` on two Linux machines with the Copilot app: NATS on the LAN, a CLI-created session in the sidebar, `--resume` refresh, `--available-tools` behavior, staged sessions. | The four open questions in `docs/design.md` have answers written into that file. |
| **M1 round trip** | Envelope, bus interface, memory and NATS adapters, thread map, config with leak guards, daemon, CLI, skill, deploy templates, CI. *(This is what the repository holds today; it is complete except for what M0 feeds back.)* | `make test` passes with the e2e round trip on both adapters; an ask from one machine lands as a session on another. |
| **M2 replies** | Reply path proven on real machines; `copilot --resume` into the asking session when M0 says the app refreshes, new session otherwise. | A reply shows up where the ask was made. |
| **M3 safety** | Shipped default for `copilot.safe_tools` from M0; rate caps and secret scan tuned on real traffic; dead-letter inspection in `doctor`. | Auto mode between two trusting users with no surprises for a week. |
| **M4 auth and platforms** | `nsc` JWT auth with `wormhole login --creds`; macOS `open` and Windows `start` openers; non-systemd service; `doctor --deep-link` after app upgrades. | A macOS user completes a round trip. |
| **M5 public 1.0** | Helm chart, signed release artifacts from public CI, name-collision check, docs pass. | Tagged 1.0.0 with a release page. |
