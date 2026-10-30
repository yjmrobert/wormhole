# Roadmap

Each milestone has one gate. M0 is a spike on real machines; M5 is the
public 1.0.

| Milestone | Scope | Gate |
| --- | --- | --- |
| **M0 spike** | Run `docs/m0-spike.md` on two Linux machines with the Copilot app. *Done October 2026 on v0.2.0; results and the defects it found are in `docs/design.md`, "M0 results".* | The four open questions in `docs/design.md` have answers written into that file. |
| **M1 round trip** | Envelope, bus interface, memory and NATS adapters, thread map, config with leak guards, daemon, CLI, skill, deploy templates, CI. *Complete; M0 proved the round trip on real machines in about four seconds.* | `make test` passes with the e2e round trip on both adapters; an ask from one machine lands as a session on another. |
| **M2 replies** | A reply the asking agent can act on. M0 showed `copilot --resume` writes the turn into the transcript but not into the context of an agent the app is running, so `daemon.reply_in_place` is off by default and replies open a `Reply from ^<peer>` session. M2 needs a supported way to hand a turn to a live app session, or an explicit decision that the separate session is the product. | The agent in the asking session acts on the reply without a person nudging it, verified with a value only the reply contained. |
| **M3 safety** | A tested auto-mode profile for `copilot.safe_tools` plus `copilot.extra_args` on a stock desktop, recorded for each supported CLI version; rate caps and secret scan tuned on real traffic; dead-letter inspection in `doctor`. | Auto mode between two trusting users with no surprises for a week. |
| **M4 auth and platforms** | `nsc` JWT auth with `wormhole login --creds`; macOS `open` and Windows `start` openers; non-systemd service; `doctor --deep-link` after app upgrades. | A macOS user completes a round trip. |
| **M5 public 1.0** | Helm chart, signed release artifacts from public CI, name-collision check, docs pass. | Tagged 1.0.0 with a release page. |
