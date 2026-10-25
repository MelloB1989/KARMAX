package daemon

// KnockCount is how many untagged devices asked for access, for the watch
// snapshot. A stand-in until the access work merges: its own KnockCount
// (internal/daemon/access.go) replaces this file, which the merge deletes.
func (f *Fleet) KnockCount() int { return 0 }
