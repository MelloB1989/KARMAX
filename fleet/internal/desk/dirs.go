package desk

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxDirs bounds a completion.
const maxDirs = 30

// expand is a path as typed: ~ is home, and a relative one is under it.
func (m *Manager) expand(p string) string {
	switch {
	case p == "" || p == "~":
		return m.Home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(m.Home, p[2:])
	case !filepath.IsAbs(p):
		return filepath.Join(m.Home, p)
	}
	return filepath.Clean(p)
}

// Dirs completes a directory for the launch form: nothing typed is home,
// the code and the latest sessions' directories; a directory is its
// subdirectories; anything else is the directories beside it that start so,
// in any case. Dot-directories only when what is typed starts with a dot.
func (m *Manager) Dirs(prefix string) []string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return m.roots()
	}
	p := m.expand(prefix)
	base := prefix[strings.LastIndexByte(prefix, '/')+1:]
	dir, match := p, ""
	if fi, err := os.Stat(p); strings.HasSuffix(prefix, "/") || err == nil && fi.IsDir() && base != "." && base != ".." {
		base = ""
	} else {
		dir, match = filepath.Dir(p), base
		if strings.HasSuffix(prefix, "/.") { // expand cleaned it away
			dir = p
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(n), strings.ToLower(match)) {
			continue
		}
		full := filepath.Join(dir, n)
		if fi, err := os.Stat(full); err != nil || !fi.IsDir() { // a link to a directory is one
			continue
		}
		out = append(out, full)
	}
	slices.SortStableFunc(out, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	if len(out) > maxDirs {
		out = out[:maxDirs]
	}
	return out
}

// roots is where a session is likely to start: home, the code, and the
// directories of the latest sessions.
func (m *Manager) roots() []string {
	var out []string
	add := func(d string) {
		if d == "" || slices.Contains(out, d) || len(out) >= maxDirs {
			return
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	add(m.Home)
	add(filepath.Join(m.Home, "code"))
	m.mu.Lock()
	m.load()
	recent := make([]string, 0, len(m.st.Sessions))
	for i := len(m.st.Sessions) - 1; i >= 0; i-- {
		recent = append(recent, m.st.Sessions[i].Dir)
	}
	m.mu.Unlock()
	for _, d := range recent {
		add(d)
	}
	return out
}
