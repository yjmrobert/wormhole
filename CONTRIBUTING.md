# Contributing

Issues and pull requests are welcome. There is no CLA; contributions are
accepted under the Apache-2.0 license in `LICENSE`. The maintainer keeps
final say on scope, and the non-goals in `docs/design.md` are real: group
asks, attachments, a hosted service and human chat are out.

## Ground rules for the public repository

Wormhole is an independent project. Every team that uses it deploys it
privately. Nothing that identifies a deployment belongs here:

- No organization names, hostnames, internal repository names, colleague
  handles or ticket references in code, docs, commit messages, issues or
  test fixtures. Use `alice`, `bob`, `bus.example.internal` and
  `<owner>/<repo>`.
- Configuration ships as `*.example` files only. Real configs, credentials
  and TLS material are gitignored and the daemon refuses credentials that
  live inside a git working tree.
- `scripts/leakcheck.sh` greps the tree for terms you list in a private,
  gitignored `.wormhole-forbidden-terms` file; wire it as a pre-commit hook
  in your own clone.

## Development

```
make test        # go vet + go test -race on every package (embedded NATS, no Docker)
make build       # bin/wormhole
make e2e         # two daemons, two bus users, one machine
```

The `memory` bus adapter is what unit tests use; the `nats` adapter tests
start an embedded `nats-server` from `deploy/nats/accounts.conf.example`, so
a change to the permission template is tested on every run.

Tests that need the Copilot app installed sit behind the `ghapp` build tag
and are not run in CI.

## Pull requests

- One change per PR, with tests. Keep the bus interface small; anything a
  transport cannot do natively is documented as a deployment requirement for
  that adapter, not emulated in code.
- `internal/ghapp` is the only package allowed to know the `ghapp://`
  scheme; a test enforces that.
- Run `gofmt`, `go vet` and `make test` before pushing.
