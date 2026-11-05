//go:build !(linux || darwin || freebsd || openbsd || netbsd)

package hostfiles

func freeSpace(string) int64 { return -1 }
