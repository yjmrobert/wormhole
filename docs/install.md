# Install and configure

Three parts: a bus that every machine can reach, a daemon per machine, and
the skill in each user's Copilot setup.

## 1. The bus

One `nats-server` with JetStream on a VM on the same LAN as the users.
Native systemd is preferred; Docker with host networking also works.

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
during the spike. Then:

```sh
systemctl enable --now nats
wormhole admin init --url tls://bus.example.internal:4222 --user admin
```

`admin init` creates the `WORMHOLE` stream (7-day retention, 64 KB
messages) and the `WORMHOLE_USERS` registry bucket. Keep the admin password
off laptops; users never need it.

Both config files are private. The repository ships only `*.example`
copies, and CI refuses a commit that contains the real ones.

## 2. The daemon

Download a release tarball or build from source:

```sh
# release
curl -fsSLO https://github.com/yjmrobert/wormhole/releases/latest/download/checksums.txt
# pick the asset for your arch from the release page, verify, install:
sha256sum -c --ignore-missing checksums.txt
install -m 0755 wormhole ~/.local/bin/wormhole

# or from source
go install github.com/yjmrobert/wormhole/cmd/wormhole@latest
```

Then on each machine:

```sh
wormhole init                             # ~/.config/wormhole/config.yaml, mode 600
$EDITOR ~/.config/wormhole/config.yaml    # handle, bus.url, senders
wormhole login                            # stores the bus password privately, verifies the connection
wormhole doctor
```

Run the daemon under systemd so it survives logouts and gets the display
environment it needs for `xdg-open` and `notify-send`:

```sh
mkdir -p ~/.config/systemd/user
cp deploy/systemd/wormhole.service ~/.config/systemd/user/
systemctl --user import-environment DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS
systemctl --user enable --now wormhole
```

### The config file

```yaml
handle: alice                       # must equal the bus user
bus:
  kind: nats
  url: tls://bus.example.internal:4222
  password_file: ~/.config/wormhole/password
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

## 3. The skill

Copy `skills/wormhole/SKILL.md` into your Copilot skills directory (or
point your skills configuration at the file in the release tarball). It
triggers on a `^handle` token and on sessions whose first message starts
with `[wormhole]`. See [Using it](usage.md) for what it does.
