//go:build !windows

package cli

type processSnapshot struct {
	started string
	exited  bool
}

// Retain the start-identity contract used by persisted daemon records; exit
// state is a separate fact and must not change their stored representation.
func webVNCDaemonProcessStartIdentity(pid int) (string, error) {
	snapshot, err := inspectProcessSnapshot(pid)
	return snapshot.started, err
}
