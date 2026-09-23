package store

// testHooks are pause points inside the multi-statement writes whose
// correctness depends on the lock order documented at lockServer. They exist
// for the deterministic overlap tests in this package: a test parks one
// transaction at a hook while it starts a second one, and asserts that the
// second either waits for the first to commit or observes what it committed.
// Without a pause the interleaving would depend on scheduling, and a test
// that passes by luck grades nothing.
//
// Every hook is nil outside tests and costs one nil check on the hot path.
var testHooks struct {
	// afterQueueCount runs in insertOutbound between counting the pending
	// envelopes and inserting the new one.
	afterQueueCount func()
	// afterSessionsEnded runs in StartSession between ending the server's
	// old sessions and inserting the new one.
	afterSessionsEnded func()
	// afterTokensCleared runs in IssueEnrollmentToken between deleting the
	// unused tokens and inserting the new one.
	afterTokensCleared func()
	// afterManifestChecked runs in DispatchAction between the manifest
	// revision recheck and the action insert.
	afterManifestChecked func()
	// afterOutboundRead runs in NextOutbound between reading the pending
	// envelopes and numbering them.
	afterOutboundRead func()
	// afterKVRead runs in KVSet between reading the key's revision and
	// writing the new row; afterKVWrite runs after the write, before commit.
	afterKVRead  func()
	afterKVWrite func()
	// afterBanTargets runs in a ban list change between choosing the servers
	// its bans.changed goes to and queueing them.
	afterBanTargets func()
}

func runHook(hook func()) {
	if hook != nil {
		hook()
	}
}
