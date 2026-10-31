//go:build linux

package harness

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// proc is one process, told apart from a later one given the same pid by
// when it started.
type proc struct {
	pid, ppid, pgid int
	start           uint64
	zombie          bool
}

// readProc reads one process from /proc/<pid>/stat.
func readProc(pid int) (proc, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return proc{}, false
	}
	// The command name (field 2) is in parentheses and may itself hold spaces
	// or parentheses; every field after the last ')' is fixed.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return proc{}, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return proc{}, false
	}
	// f[0] is field 3 (state): ppid is 4, pgrp 5, starttime 22.
	ppid, _ := strconv.Atoi(f[1])
	pgid, _ := strconv.Atoi(f[2])
	start, _ := strconv.ParseUint(f[19], 10, 64)
	return proc{pid: pid, ppid: ppid, pgid: pgid, start: start, zombie: f[0] == "Z"}, true
}

func allProcs() []proc {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]proc, 0, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p, ok := readProc(pid); ok {
			out = append(out, p)
		}
	}
	return out
}

// descendants is every process below root, whatever group or session it has
// moved to since. Read while root is still their parent: once it exits they
// belong to init, and nothing ties them to the session any more.
//
// The real CLI starts each shell command it runs in a session of its own, so
// a signal to its process group never reaches one; this is how Close finds
// them.
func descendants(root int) []proc {
	children := map[int][]proc{}
	for _, p := range allProcs() {
		children[p.ppid] = append(children[p.ppid], p)
	}
	var out []proc
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		for _, c := range children[queue[0]] {
			out = append(out, c)
			queue = append(queue, c.pid)
		}
	}
	return out
}

// leftovers is what is still running of tree, the processes a session had
// started, and of the process group the session led. Zombies are left out:
// they have finished and only wait to be reaped. A process in tree counts
// only while its start time matches, so a pid the kernel has since handed to
// someone else is never signalled.
func leftovers(leader int, tree []proc) []proc {
	now := map[int]proc{}
	var out []proc
	for _, p := range allProcs() {
		if p.zombie {
			continue
		}
		now[p.pid] = p
		if p.pgid == leader {
			out = append(out, p)
		}
	}
	for _, t := range tree {
		if p, ok := now[t.pid]; ok && p.start == t.start && p.pgid != leader {
			out = append(out, p)
		}
	}
	return out
}
