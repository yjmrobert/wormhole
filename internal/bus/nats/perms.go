package nats

import (
	"github.com/nats-io/nats-server/v2/server"

	"github.com/yjmrobert/wormhole/internal/bus"
)

// UserPermissions is the per-user ACL every Wormhole user gets, whether the
// bus is `wormhole serve` or a nats-server configured from
// deploy/nats/accounts.conf.example: publish only as oneself, subscribe
// only to one's own traffic, and the JetStream API subjects for one's own
// durable consumer and the registry bucket. A test parses the example and
// asserts the two agree.
func UserPermissions(handle string) *server.Permissions {
	consumer := ConsumerName(handle)
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
			"$JS.API.STREAM.INFO.KV_" + BucketName,
			"$JS.API.CONSUMER.CREATE.KV_" + BucketName + ".>",
			"$JS.API.CONSUMER.INFO.KV_" + BucketName + ".>",
			"$JS.API.CONSUMER.MSG.NEXT.KV_" + BucketName + ".>",
			"$JS.API.CONSUMER.DELETE.KV_" + BucketName + ".>",
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
