package secretsource

// unregisterForTest removes a kind a test registered, restoring the process-wide registry.
//
// The registry deliberately has no production unregister: a kind is wired at init and lives for
// the process, the same contract database/sql drivers keep, and an exported removal would be API
// surface inviting a caller to unwire an engine mid-flight. Tests still must clean up after
// themselves, because a leftover registration makes the package fail its own precondition on the
// second run in one process, so go test -count=2, the standard flake-shaker, failed on state the
// first run left behind rather than on anything wrong.
func unregisterForTest(kind string) {
	delete(resolvers, kind)
	delete(minters, kind)
}
