package nats

import (
	"github.com/nats-io/nats-server/v2/server"

	"github.com/yjmrobert/wormhole/internal/bus"
)

// UserPermissions is what one Wormhole user may publish and subscribe to:
// exactly the allow lists deploy/nats/accounts.conf.example grants each
// user, with the handle substituted. The embedded server (wormhole serve)
// uses it directly; a test parses the template and asserts the two agree.
func UserPermissions(handle string) *server.Permissions {
	consumer := ConsumerName(handle)
	kv := "KV_" + BucketName
	return &server.Permissions{
		Publish: &server.SubjectPermission{Allow: []string{
			bus.Prefix + "." + bus.KindInbox + ".*." + handle,
			bus.Prefix + "." + bus.KindAck + ".*." + handle,
			bus.DLQSubject(handle),
			"$JS.API.INFO",
			"$JS.API.STREAM.INFO." + StreamName,
			"$JS.API.CONSUMER.CREATE." + StreamName + "." + consumer,
			"$JS.API.CONSUMER.CREATE." + StreamName + "." + consumer + ".>",
			"$JS.API.CONSUMER.DURABLE.CREATE." + StreamName + "." + consumer,
			"$JS.API.CONSUMER.INFO." + StreamName + "." + consumer,
			"$JS.API.CONSUMER.MSG.NEXT." + StreamName + "." + consumer,
			"$JS.API.CONSUMER.DELETE." + StreamName + "." + consumer,
			"$JS.ACK." + StreamName + "." + consumer + ".>",
			"$JS.API.STREAM.INFO." + kv,
			"$JS.API.CONSUMER.CREATE." + kv + ".>",
			"$JS.API.CONSUMER.INFO." + kv + ".>",
			"$JS.API.CONSUMER.MSG.NEXT." + kv + ".>",
			"$JS.API.CONSUMER.DELETE." + kv + ".>",
			"$KV." + BucketName + "." + handle,
		}},
		Subscribe: &server.SubjectPermission{Allow: []string{
			bus.Prefix + "." + bus.KindInbox + "." + handle + ".>",
			bus.Prefix + "." + bus.KindAck + "." + handle + ".>",
			bus.DLQSubject(handle),
			"_INBOX.>",
		}},
	}
}
