#!/usr/bin/env sh
# M0 test 1: the bus is reachable and enforces sender identity.
# Needs the nats CLI (https://github.com/nats-io/natscli).
set -eu
. "$(dirname "$0")/common.sh"
need nats
note "connect as $WH_ME to $WH_BUS"
nats --server "$WH_BUS" --user "$WH_ME" --password "$(pw)" server check connection

note "subscribe as $WH_ME to my inbox in the background"
nats --server "$WH_BUS" --user "$WH_ME" --password "$(pw)" sub "wh.inbox.$WH_ME.>" --count 1 &
subpid=$!
sleep 1

note "publish to myself on wh.inbox.$WH_ME.$WH_ME (allowed: last token is me)"
nats --server "$WH_BUS" --user "$WH_ME" --password "$(pw)" pub "wh.inbox.$WH_ME.$WH_ME" '{"probe":true}'
wait $subpid && echo "PASS: own-subject publish delivered"

note "publish with a forged sender token wh.inbox.$WH_PEER.carol (must be refused)"
if nats --server "$WH_BUS" --user "$WH_ME" --password "$(pw)" pub "wh.inbox.$WH_PEER.carol" '{"spoof":true}' 2>&1 | grep -qi "permissions violation"; then
  echo "PASS: forged sender refused"
else
  echo "CHECK: no permissions violation reported; look at the server log (publish may be silently dropped by the CLI)"
fi

note "subscribe to the peer's inbox wh.inbox.$WH_PEER.> (must be refused)"
if timeout 3 nats --server "$WH_BUS" --user "$WH_ME" --password "$(pw)" sub "wh.inbox.$WH_PEER.>" --count 1 2>&1 | grep -qi "permissions violation"; then
  echo "PASS: foreign inbox refused"
else
  echo "CHECK: no permissions violation reported for the foreign subscribe"
fi

note "stream and registry exist"
nats --server "$WH_BUS" --user "$WH_ME" --password "$(pw)" stream info WORMHOLE >/dev/null && echo "PASS: stream WORMHOLE"
nats --server "$WH_BUS" --user "$WH_ME" --password "$(pw)" kv ls 2>/dev/null | grep -q WORMHOLE_USERS && echo "PASS: bucket WORMHOLE_USERS" || echo "CHECK: run wormhole admin init"
