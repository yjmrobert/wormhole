package cli

// Messages for refusals (exit 3), lookups that found nothing (exit 4) and
// unreachable dependencies (exit 5). Each ends with the command or config
// line that resolves it; errors_test.go keeps them that way.
const (
	// publishErrorFormat is a publish the bus did not accept.
	publishErrorFormat = "publish: %v (is the bus reachable? run: wormhole doctor)"
	// permissionFormat is the bus refusing a subject to this user.
	permissionFormat = "%v; check that handle %s in your config is your bus user, then: wormhole login"
	// threadOverFormat is a follow-up on a thread that no longer takes one.
	threadOverFormat = "thread %s is %s; start a new ask: wormhole ask ^%s \"...\""
	// nothingStagedFormat is an inbox verb on a thread with nothing waiting.
	nothingStagedFormat = "nothing is waiting on thread %s (state %s); see what is: wormhole inbox"
	// registryErrorFormat is a registry read that failed.
	registryErrorFormat = "registry: %v; check the bus with: wormhole doctor"
)
