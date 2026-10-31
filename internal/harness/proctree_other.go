//go:build !linux

package harness

// proc is one process. Without /proc there is no tree to read, so only the
// session's own process group is known.
type proc struct{ pid, pgid int }

// descendants is unknown without /proc: Close reaches what stayed in the
// session's process group, and nothing that left it.
func descendants(int) []proc { return nil }

// leftovers is the session's process group, while anything is left in it.
func leftovers(leader int, _ []proc) []proc {
	if groupAlive(leader) {
		return []proc{{pid: leader, pgid: leader}}
	}
	return nil
}
