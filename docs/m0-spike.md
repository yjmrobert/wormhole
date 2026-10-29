# M0 spike: answer the unknowns on real machines

Everything in this repository that touches the Copilot app is wired from
the plan's verified building blocks but has not been exercised here: this
code was written without the app, a display or a LAN. M0 is a one-or-two
day spike on two Linux machines with the Copilot app installed, in the
order below. Write each answer into the matching checkbox in
`docs/design.md` ("Open questions") and open an issue for anything that
changes a default.

The scripts under `scripts/m0/` are plain shell; read them before running.
They use the placeholder handles `alice` and `bob` and the bus host
`bus.example.internal`; export `WH_BUS`, `WH_ME` and `WH_PEER` to override.

## Test 1: NATS on the LAN VM

On the VM:

```
cp deploy/nats/nats-server.conf.example /etc/nats/nats-server.conf
cp deploy/nats/accounts.conf.example    /etc/nats/accounts.conf
# set real passwords (nats server passwd), comment out tls{} for the spike
nats-server -c /etc/nats/nats-server.conf
```

From machine A then B:

```
scripts/m0/01-bus.sh          # nats pub/sub both ways with alice and bob creds
```

Pass: a message published by alice as `wh.inbox.bob.alice` arrives at bob;
publishing as `wh.inbox.bob.carol` with alice's creds is rejected with a
permissions violation.

## Test 2: a bus message becomes a session in the sidebar

On machine B (bob), with the real daemon:

```
wormhole init && $EDITOR ~/.config/wormhole/config.yaml   # handle: bob, senders: [{handle: alice, mode: auto}], copilot.safe_tools: [<anything for now>]
wormhole login
wormhole doctor
wormhole daemon --verbose
```

On machine A (alice):

```
wormhole ask bob "Reply with the single word pong." --yes
```

Pass: a session named `^alice: Reply with the single word pong.` appears in
bob's app sidebar without bob touching anything, `wormhole threads` on A
shows `delivered`, and `wormhole reply` from inside bob's session lands
`replied` on A.

Record: how long `copilot -p` took to return; whether the sidebar updated
immediately on `xdg-open`, only after a restart, or never (then test 5
matters more).

## Test 3: does `copilot --resume` refresh a listed session?

```
scripts/m0/03-resume.sh       # creates a session, opens it, appends a turn with --resume
```

Pass: the appended turn is visible in the app on the already-open session
without reopening it. This decides whether M2 can land replies in the
asking session instead of a new `Reply from ^bob` session.

## Test 4: what does `--available-tools` restrict in `-p` mode?

```
scripts/m0/04-tools.sh        # runs a prompt that tries to read, write and shell out under several tool sets
```

For each candidate set, record which of these the agent could do: read a
file, list a directory, grep, write a file, run a shell command, `git
push`, fetch a URL. The largest set where only the first three succeed
becomes the shipped default for `copilot.safe_tools` in
`internal/config/config.go` (`Example`) and `deploy/config.yaml.example`.
Also note whether `--deny-tool` is needed in addition.

## Test 5: can a session be created with the kickoff staged but not run?

```
scripts/m0/05-staged.sh       # tries the obvious flags and inspects the result
```

If some invocation creates a listed session whose first turn is pending
rather than executed, note the exact command. `wormhole open` then
switches from "run the staged kickoff now" to "open the pre-created
session". If nothing works, the current behavior stays and this question
closes.

## Test 6: does the app pick up a CLI-created session without the deep link?

Create a session with `copilot -p ... --session-id <uuid>` and do not open
any link. Note whether it appears in the sidebar immediately, after a
rescan, after a restart, or never. This sizes the deep-link fallback in
`wormhole open`.

## Writing the results back

- `docs/design.md` open questions: tick and annotate.
- `internal/config/config.go` `Example` and `deploy/config.yaml.example`:
  shipped `safe_tools`.
- `internal/ghapp`: any flag changes to `Create`, `Resume` and `Open`.
- `internal/daemon`: the reply path if test 3 passes.
- `docs/roadmap.md`: adjust M2 and M3 gates.
