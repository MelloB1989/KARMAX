package googleacct

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// call is one command the host was asked to run.
type call struct {
	bin  string
	env  []string
	args []string
}

func (c call) String() string { return c.bin + " " + strings.Join(c.args, " ") }

func (c call) envValue(k string) string {
	for _, kv := range c.env {
		if v, ok := strings.CutPrefix(kv, k+"="); ok {
			return v
		}
	}
	return ""
}

// reply is what a matching command answers.
type reply struct {
	out, errOut string
	code        int
}

// fakeCLI answers commands by the first rule whose every word appears in the
// command line (and whose config dir matches, for gws).
type fakeCLI struct {
	mu    sync.Mutex
	rules []rule
	calls []call
}

type rule struct {
	bin   string
	dir   string // gws config dir, "" for any
	words []string
	r     reply
}

func (f *fakeCLI) on(bin, dir, match string, r reply) {
	f.rules = append(f.rules, rule{bin: bin, dir: dir, words: strings.Fields(match), r: r})
}

func (f *fakeCLI) run(_ context.Context, env []string, bin string, args ...string) ([]byte, []byte, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := call{bin: bin, env: env, args: args}
	f.calls = append(f.calls, c)
	line := strings.Join(args, " ")
	for _, r := range f.rules {
		if r.bin != bin || (r.dir != "" && c.envValue("GOOGLE_WORKSPACE_CLI_CONFIG_DIR") != r.dir) {
			continue
		}
		ok := true
		for _, w := range r.words {
			if !strings.Contains(line, w) {
				ok = false
				break
			}
		}
		if ok {
			var err error
			if r.r.code != 0 {
				err = fmt.Errorf("exit status %d", r.r.code)
			}
			return []byte(r.r.out), []byte(r.r.errOut), r.r.code, err
		}
	}
	return nil, []byte("no rule for: " + bin + " " + line), 1, fmt.Errorf("exit status 1")
}

func (f *fakeCLI) find(t *testing.T, bin string, words ...string) call {
	t.Helper()
	for i := len(f.calls) - 1; i >= 0; i-- {
		c := f.calls[i]
		if c.bin != bin {
			continue
		}
		line := strings.Join(c.args, " ")
		ok := true
		for _, w := range words {
			if !strings.Contains(line, w) {
				ok = false
				break
			}
		}
		if ok {
			return c
		}
	}
	t.Fatalf("no %s call with %v; calls:\n%v", bin, words, f.calls)
	return call{}
}

func hostWith(f *fakeCLI, gwsDirs ...string) *Host {
	return &Host{Gog: "gog", Gws: "gws", GwsDirs: func() []string { return gwsDirs }, Run: f.run,
		HasLogin: func(string) bool { return true }}
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
	}
	return ""
}
