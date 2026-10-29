#!/usr/bin/env sh
# Print the accounts.conf block for one more Wormhole user.
# Usage: deploy/nats/add-user.sh <handle> ['<bcrypt or password>']
# Paste the output into the users list of your private accounts.conf and
# reload the server (systemctl reload nats). Hash passwords with:
#   nats server passwd
set -eu
handle="${1:?usage: add-user.sh <handle> [password]}"
password="${2:-CHANGE_ME_$handle}"
case "$handle" in
  *[!a-z0-9_-]*|"") echo "handle must match ^[a-z0-9][a-z0-9_-]*$" >&2; exit 2 ;;
esac
cat <<BLOCK
    {
      user: $handle
      password: "$password"
      permissions: {
        publish: {
          allow: [
            "wh.inbox.*.$handle", "wh.ack.*.$handle", "wh.dlq.$handle",
            "\$JS.API.INFO",
            "\$JS.API.STREAM.INFO.WORMHOLE",
            "\$JS.API.CONSUMER.CREATE.WORMHOLE.wh-$handle", "\$JS.API.CONSUMER.CREATE.WORMHOLE.wh-$handle.>",
            "\$JS.API.CONSUMER.DURABLE.CREATE.WORMHOLE.wh-$handle",
            "\$JS.API.CONSUMER.INFO.WORMHOLE.wh-$handle",
            "\$JS.API.CONSUMER.MSG.NEXT.WORMHOLE.wh-$handle",
            "\$JS.API.CONSUMER.DELETE.WORMHOLE.wh-$handle",
            "\$JS.ACK.WORMHOLE.wh-$handle.>",
            "\$JS.API.STREAM.INFO.KV_WORMHOLE_USERS",
            "\$JS.API.CONSUMER.CREATE.KV_WORMHOLE_USERS.>",
            "\$JS.API.CONSUMER.INFO.KV_WORMHOLE_USERS.>",
            "\$JS.API.CONSUMER.MSG.NEXT.KV_WORMHOLE_USERS.>",
            "\$JS.API.CONSUMER.DELETE.KV_WORMHOLE_USERS.>",
            "\$KV.WORMHOLE_USERS.$handle"
          ]
        }
        subscribe: {
          allow: [ "wh.inbox.$handle.>", "wh.ack.$handle.>", "wh.dlq.$handle", "_INBOX.>" ]
        }
      }
    }
BLOCK
