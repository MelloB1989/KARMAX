// Package recreate says which containers `docker compose up` would recreate
// from a rendered compose file, and why, and recreates the agents among them
// only when they are not working.
//
// It compares what is running with what the file asks for, so the reasons are
// ours (image, brief, env); compose's own config hash catches the rest
// (volumes, limits, command). Env values are secrets: only names are reported.
package recreate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BriefKey is the env var that carries an agent's CLAUDE.md.
const BriefKey = "FLEET_CLAUDE_MD"

// Runner runs argv and returns its stdout.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// Change is one container `up` would recreate, or create.
type Change struct {
	Service   string
	Container string
	Create    bool     // no container yet: nothing running is disturbed
	Reasons   []string // why it is recreated
}

type spec struct {
	Image         string            `yaml:"image"`
	ContainerName string            `yaml:"container_name"`
	EnvFile       []string          `yaml:"env_file"`
	Environment   map[string]string `yaml:"environment"`
	DependsOn     []string          `yaml:"depends_on"`
}

// Planner inspects one host's containers through Prefix, e.g. [docker --context pc2].
type Planner struct {
	Prefix []string
	File   string // the rendered compose file
	Run    Runner
}

func (p Planner) argv(args ...string) []string {
	return append(append([]string{}, p.Prefix...), args...)
}

func (p Planner) compose(args ...string) []string {
	return p.argv(append([]string{"compose", "-p", "fleet", "-f", p.File}, args...)...)
}

// Plan lists what `up` would recreate, sorted by service. Services with
// nothing to change are left out.
func (p Planner) Plan(ctx context.Context) ([]Change, error) {
	raw, err := os.ReadFile(p.File)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Services map[string]spec `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", p.File, err)
	}
	hashes := p.hashes(ctx)
	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	by := map[string]*Change{}
	for _, n := range names {
		s := doc.Services[n]
		want, err := s.env(filepath.Dir(p.File))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		c := &Change{Service: n, Container: s.ContainerName}
		if c.Container == "" {
			c.Container = "fleet-" + n + "-1"
		}
		out, err := p.Run(ctx, p.argv("inspect", c.Container))
		if err != nil {
			c.Create = true
			by[n] = c
			continue
		}
		var got []struct {
			Image  string
			Config struct {
				Env    []string
				Labels map[string]string
			}
		}
		if err := json.Unmarshal(out, &got); err != nil || len(got) != 1 {
			return nil, fmt.Errorf("inspect %s: unreadable", c.Container)
		}
		if id, err := p.Run(ctx, p.argv("image", "inspect", "--format", "{{.Id}}", s.Image)); err == nil &&
			strings.TrimSpace(string(id)) != got[0].Image {
			c.Reasons = append(c.Reasons, "image changed ("+s.Image+")")
		}
		have := map[string]string{}
		for _, kv := range got[0].Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			have[k] = v
		}
		var keys []string
		for k, v := range want {
			if hv, ok := have[k]; !ok || hv != v {
				if k == BriefKey {
					c.Reasons = append(c.Reasons, "brief changed")
				} else {
					keys = append(keys, k)
				}
			}
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			c.Reasons = append(c.Reasons, "env changed: "+strings.Join(keys, ", "))
		}
		if h, ok := hashes[n]; ok && len(c.Reasons) == 0 && got[0].Config.Labels["com.docker.compose.config-hash"] != h {
			c.Reasons = append(c.Reasons, "compose config changed")
		}
		by[n] = c
	}

	// A service recreated drags the services that depend on it along.
	for changed := true; changed; {
		changed = false
		for _, n := range names {
			c := by[n]
			for _, d := range doc.Services[n].DependsOn {
				if dc := by[d]; dc != nil && !dc.Create && len(dc.Reasons) > 0 && !c.Create && !hasReason(c, "depends on "+d) {
					c.Reasons = append(c.Reasons, "depends on "+d)
					changed = true
				}
			}
		}
	}
	var out []Change
	for _, n := range names {
		if c := by[n]; c.Create || len(c.Reasons) > 0 {
			out = append(out, *c)
		}
	}
	return out, nil
}

func hasReason(c *Change, r string) bool {
	for _, x := range c.Reasons {
		if x == r {
			return true
		}
	}
	return false
}

// hashes is compose's own config hash per service; empty if this compose
// cannot say.
func (p Planner) hashes(ctx context.Context) map[string]string {
	out, err := p.Run(ctx, p.compose("config", "--hash", "*"))
	m := map[string]string{}
	if err != nil {
		return m
	}
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

// env is what the container's environment must hold: env_file, then
// environment over it.
func (s spec) env(dir string) (map[string]string, error) {
	want := map[string]string{}
	for _, f := range s.EnvFile {
		if !filepath.IsAbs(f) {
			f = filepath.Join(dir, f)
		}
		kv, err := readEnv(f)
		if err != nil {
			return nil, err
		}
		for k, v := range kv {
			want[k] = v
		}
	}
	for k, v := range s.Environment {
		want[k] = v
	}
	return want, nil
}

func readEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if k, v, ok := strings.Cut(l, "="); ok && l != "" && !strings.HasPrefix(l, "#") {
			m[strings.TrimSpace(k)] = v
		}
	}
	return m, nil
}

// Describe prints the plan, one line per container.
func Describe(w io.Writer, host string, cs []Change) {
	any := false
	for _, c := range cs {
		if c.Create {
			fmt.Fprintf(w, "  %s: %s will be created\n", host, c.Container)
			continue
		}
		any = true
		fmt.Fprintf(w, "  %s: %s will be RECREATED — %s\n", host, c.Container, strings.Join(c.Reasons, "; "))
	}
	if !any && len(cs) == 0 {
		fmt.Fprintf(w, "  %s: nothing to recreate\n", host)
	}
}

// Idler recreates agents one at a time, each once it is not working.
type Idler struct {
	Planner
	Agents map[string]bool                                      // service names that are roster agents
	State  func(ctx context.Context) (map[string]string, error) // agent -> fleetd state
	Sleep  func(ctx context.Context, d time.Duration) error
	Every  time.Duration
	Out    io.Writer
}

// Up applies cs: every agent is recreated alone, once fleetd says it is not
// working; anything that is not an agent (and anything that drags agents
// along) goes when all the agents it touches are idle at once. A final plain
// `up` brings up the rest.
func (i Idler) Up(ctx context.Context, cs []Change) error {
	var solo, group []Change
	dragged := map[string]bool{}
	for _, c := range cs {
		if c.Create {
			continue
		}
		if !i.Agents[c.Service] {
			group = append(group, c)
		}
	}
	for _, c := range cs {
		if c.Create || !i.Agents[c.Service] {
			continue
		}
		if hasDependsReason(c, group) {
			dragged[c.Service] = true
			continue
		}
		solo = append(solo, c)
	}
	for _, c := range solo {
		if err := i.waitIdle(ctx, []string{c.Service}); err != nil {
			return err
		}
		fmt.Fprintf(i.Out, "recreating %s\n", c.Container)
		if err := i.up(ctx, "--no-deps", c.Service); err != nil {
			return err
		}
	}
	if len(group) > 0 {
		var agents []string
		for a := range dragged {
			agents = append(agents, a)
		}
		sort.Strings(agents)
		if err := i.waitIdle(ctx, agents); err != nil {
			return err
		}
	}
	return i.up(ctx, "--remove-orphans")
}

func hasDependsReason(c Change, group []Change) bool {
	for _, g := range group {
		if hasReason(&c, "depends on "+g.Service) {
			return true
		}
	}
	return false
}

func (i Idler) up(ctx context.Context, args ...string) error {
	argv := i.compose(append([]string{"up", "-d"}, args...)...)
	fmt.Fprintln(i.Out, "$", strings.Join(argv, " "))
	out, err := i.Run(ctx, argv)
	_, _ = i.Out.Write(out)
	return err
}

// waitIdle returns when none of agents is working (all at once).
func (i Idler) waitIdle(ctx context.Context, agents []string) error {
	if len(agents) == 0 {
		return nil
	}
	announced := ""
	for {
		st, err := i.State(ctx)
		if err != nil {
			return fmt.Errorf("fleetd status: %w", err)
		}
		var busy []string
		for _, a := range agents {
			if s, ok := st[a]; !ok || s == "working" {
				// An agent fleetd does not list is not known to be idle.
				busy = append(busy, a)
			}
		}
		if len(busy) == 0 {
			return nil
		}
		if msg := "waiting for " + strings.Join(busy, ", ") + " to stop working"; msg != announced {
			fmt.Fprintln(i.Out, msg)
			announced = msg
		}
		if err := i.Sleep(ctx, i.Every); err != nil {
			return fmt.Errorf("%s: %w", announced, err)
		}
	}
}
