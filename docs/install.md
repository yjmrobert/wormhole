# Install and configure

Three parts: a bus every machine can reach, a daemon per machine, and the
skill in each person's agent. The binary sets all three up; nothing below
is edited by hand.

## 1. Get the binary

Download a release tarball or build from source, on every machine:

```sh
# release
curl -fsSLO https://github.com/yjmrobert/wormhole/releases/latest/download/checksums.txt
# pick the asset for your arch from the release page, verify, install:
sha256sum -c --ignore-missing checksums.txt
install -m 0755 wormhole ~/.local/bin/wormhole

# or from source
go install github.com/yjmrobert/wormhole/cmd/wormhole@latest
```

`wormhole demo` runs alice and bob on this machine with no server and no
second person, so you can see the round trip before setting anything up.

## 2. Host the bus on one machine

One person runs the bus. `wormhole serve` embeds a NATS JetStream server
in the binary: it generates its own TLS material and its own users, so
there is no `nats-server` to install and no config to copy.

```sh
wormhole serve install --invite bob
```

This writes a systemd user unit (`wormhole-serve`), starts it, joins this
machine to its own bus under your handle (`$USER` unless `--handle` says
otherwise), and prints one invite per `--invite` handle in the exact form
to send:

```
on bob's machine run: wormhole join wh1.eyJ2IjoxLCJpZCI6Ij...
```

More invites later: `wormhole serve invite carol`. Who is on the bus:
`wormhole serve users`. Remove someone: `wormhole serve revoke carol`.
Without systemd, `wormhole serve` runs the server in the foreground.

The server listens on every interface, port 4222, with TLS; `--listen`
changes that and `--advertise host[:port]` names an address the others
should use (a DNS name, a VPN address) when the ones it finds on its own
interfaces are not reachable. The state lives under
`~/.local/state/wormhole/serve`: the CA and server certificate, the user
list with bcrypt-hashed passwords, and the JetStream store. Keep that
directory private; it is mode 700.

An invite is one token that fits in a chat message. It carries the bus
addresses, the CA certificate, the handle, who sent it and a one-time
secret that expires after 24 hours (`--ttl`). The secret is not a bus
password: `join` exchanges it once for a credential the server generates,
and the invite is dead afterwards. See [Design](design.md#joining-a-bus).

## 3. Join the bus on every other machine

```sh
wormhole join wh1.eyJ2IjoxLCJpZCI6Ij...
```

`join` writes `~/.config/wormhole/config.yaml` (mode 600) with your
handle, the bus addresses and the CA, stores the credential the server
handed back next to it, verifies the connection, installs the skill into
every agent it finds on this machine, installs and starts the daemon as a
systemd user service, and ends with the one thing to do next:

```
joined tls://alice-laptop:4222 as ^bob
ask them something: wormhole ask ^alice "..."
```

The person who invited you is added under `senders:` in inbox mode, so
their first ask waits in your inbox instead of being rejected; nothing of
theirs runs until you read it. `wormhole senders remove alice` undoes
that; `wormhole senders add carol` lets someone else ask you.

Running `join` again with a new invite rotates the credential and leaves
the rest of the config alone.

Without a systemd user session (an SSH login, a container), `join` says so
and the fallback is to run `wormhole daemon` in a terminal.

## 4. The skill and the service, by hand

`join` already did both. The standalone forms, for a machine set up another
way or after reinstalling an agent:

```sh
wormhole skill install            # into every detected agent; --agent copilot, or --dir <skills directory>
wormhole skill status
wormhole service install          # the daemon as a systemd user unit, with the display environment imported
wormhole service status
```

The service unit ties the daemon to `graphical-session.target` and
imports `DISPLAY`, `WAYLAND_DISPLAY` and `DBUS_SESSION_BUS_ADDRESS`, which
`xdg-open`, `notify-send` and the Copilot CLI's keychain need. `wormhole
doctor` says when they are missing.

Copilot's user-level skills live in `~/.copilot/skills`; the skill is
written to `wormhole/SKILL.md` there. Another agent, or a different
layout, takes `--dir`.

## 5. `wormhole init` for everything else

`wormhole init` is a wizard: handle, how to reach the bus (paste an
invite, or a URL plus a credential), who may ask you, notifications. Every
answer has a default that works. `wormhole init --no-input` takes a flag
for each question (`--handle`, `--invite`, `--url`, `--password-file`,
`--creds`, `--senders a,b`, `--notify`) and skips what it cannot answer;
`wormhole doctor` then names what is still missing, with the command that
fixes it.

`wormhole doctor` checks the config, credentials, the bus, the stream and
registry, the Copilot binary, the display environment, the inbox page and
dead letters. Every failing check ends with its fix, and `--json` carries
it in a `fix` field.

## Operating your own bus

Teams that want the bus on a VM, under their own TLS certificate and
account management, run `nats-server` themselves. `wormhole serve` and
this path use the same stream, bucket, subjects and per-user permissions;
a daemon cannot tell them apart.

```sh
# on the VM, as root
useradd --system --home /var/lib/nats --create-home nats
install -d -o nats -g nats /var/lib/nats/jetstream /etc/nats
cp deploy/nats/nats-server.conf.example /etc/nats/nats-server.conf
cp deploy/nats/accounts.conf.example    /etc/nats/accounts.conf
cp deploy/nats/nats.service             /etc/systemd/system/
```

Edit `/etc/nats/accounts.conf`: set real passwords (hash them with
`nats server passwd`) and add one block per user with
`deploy/nats/add-user.sh <handle>`. Edit `/etc/nats/nats-server.conf`:
point `tls` at your certificate, or comment the block out on a trusted LAN
during a spike. Then:

```sh
systemctl enable --now nats
wormhole admin init --url tls://bus.example.internal:4222 --user admin
```

`admin init` creates the `WORMHOLE` stream (7-day retention, 64 KB
messages) and the `WORMHOLE_USERS` registry bucket. Keep the admin password
off laptops; users never need it. Docker with host networking also works:
`deploy/nats/docker-compose.yml`.

Each machine then joins without an invite:

```sh
wormhole init --no-input --handle alice --url tls://bus.example.internal:4222 --senders bob
wormhole login                    # stores the bus password privately, verifies the connection
wormhole skill install
wormhole service install
wormhole doctor
```

Both server config files are private. The repository ships only `*.example`
copies, and CI refuses a commit that contains the real ones.

### Updates

`wormhole update` checks GitHub Releases and installs a newer binary after
verifying its SHA-256 checksum against the release's `checksums.txt`. The
checksum guards against a corrupt download, not against a bad release: both
files come from the same GitHub release, and releases are not signed.
Automatic checks are opt-in and run at most once every 24 hours on eligible
interactive commands:

```sh
wormhole update --enable    # enable daily automatic checks
wormhole update --disable   # disable them; manual updates still work
wormhole update             # check now, regardless of the automatic setting
```

Check that self-update can work on your machine before enabling automatic
checks. `--enable` does not check this for you, and an update that can never
succeed prints an error on every daily attempt:

- Self-update supports the release binaries for Linux amd64 and arm64 only.
  A binary from `go install` or for another platform is updated the way it
  was installed.
- The directory holding the `wormhole` executable must be writable by the
  user running it. A root-owned `/usr/local/bin/wormhole` cannot update
  itself; `~/.local/bin/wormhole` can.

An automatic check has a 30 second budget for the release lookup, the
download and the install together, and a failed attempt is not retried for
24 hours. On a slow connection run `wormhole update` by hand instead; it has
no such limit.

Set `WORMHOLE_NO_UPDATE_CHECK=1` to suppress automatic checks without changing
the saved setting. Development builds, `--json`, `--no-input`, shell
completion, command discovery, and the long-running daemon never trigger an
automatic replacement. Update state lives in `update.json` in the Wormhole
state directory (`$WORMHOLE_STATE_DIR`, else `$XDG_STATE_HOME/wormhole`,
default `~/.local/state/wormhole`); any interactive command creates that
directory and keeps it owner-only, whether or not checks are enabled. If the
executable's directory is not writable, the command fails with a permission
error and leaves the current binary untouched; reinstall it manually using
the release steps in section 1. Restart the daemon after updating its binary:

```sh
systemctl --user restart wormhole
```

### The config file

`join` and `init` write it; `senders add` and `senders remove` edit it.
Every key is documented in `deploy/config.yaml.example`.

```yaml
handle: alice                       # must equal the bus user
bus:
  kind: nats
  url: tls://bus.example.internal:4222   # several addresses: comma-separated
  password_file: ~/.config/wormhole/password
  tls_ca: ~/.config/wormhole/bus-ca.pem  # written by join; the CA of a wormhole serve bus
senders:
  - bob                             # inbox mode: a person reads it first
  - {handle: carol, mode: auto}     # auto mode: runs headless with the tools below
limits:
  per_sender_per_day: 20
  per_day: 50
  max_hops: 6
copilot:
  workdir: ~/wormhole
  safe_tools: []                    # --available-tools for auto mode; empty refuses auto mode
  extra_args: []                    # the CLI's permission flags, appended to every run
repos:
  "<owner>/<repo>": /home/alice/src/repo
daemon:
  reply_in_place: false             # true: append replies to the session that asked (see usage)
```

The daemon refuses to start if this file is readable by other users or if
`password_file` or `creds` points inside a git working tree. Auto mode is
refused while `safe_tools` is empty.

Answering needs the shell tool (the agent runs `wormhole reply`), so a
usable `safe_tools` is not read-only on its own. Pin it down with the
Copilot CLI's permission flags in `extra_args`: allow the shell for
`wormhole` and deny every subcommand but `reply`. The flag names depend on
the CLI version; the profile that worked on 1.0.105 is in the design
page's "M0 results".
