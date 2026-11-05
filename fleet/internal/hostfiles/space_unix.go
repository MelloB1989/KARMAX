//go:build linux || darwin || freebsd || openbsd || netbsd

package hostfiles

import "golang.org/x/sys/unix"

// freeSpace is the bytes free to this user on dir's disk; -1 unknown.
func freeSpace(dir string) int64 {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
