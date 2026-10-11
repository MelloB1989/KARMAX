package recreate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const composeFile = `name: fleet
services:
  fleet-anchor:
    image: img:2
    container_name: fleet-anchor
  agent-01:
    image: img:2
    container_name: agent-01
    hostname: agent-01
    cpus: "2"
    mem_limit: 4g
    volumes: ["/code:/code", "agent-01-work:/work"]
    depends_on: [fleet-anchor]
    env_file: [agents/agent-01.env]
    environment: {FLEET_NAME: agent-01, FLEET_CLAUDE_MD: BRIEF2}
  agent-02:
    image: img:2
    container_name: agent-02
    depends_on: [fleet-anchor]
    env_file: [agents/agent-02.env]
    environment: {FLEET_NAME: agent-02, FLEET_CLAUDE_MD: BRIEF1}
  agent-03:
    image: img:2
    container_name: agent-03
    depends_on: [fleet-anchor]
    env_file: [agents/agent-03.env]
    environment: {FLEET_NAME: agent-03, FLEET_CLAUDE_MD: BRIEF1}
`

// fakeDocker is a docker CLI: running containers with an image id, env and
// config-hash label; image ids by tag; compose hashes; and a log of every
// compose up.
type fakeDocker struct {
	mu         sync.Mutex
	running    map[string]container
	imageIDs   map[string]string
	hashes     string
	hashesFail bool
	calls      [][]string
}

type container struct {
	image string
	env   []string
	hash  string
	// optional, as `docker inspect` reports them; zero values match composeFile
	hostname string
	nanoCPUs int64
	memory   int64
	mounts   []string
}

func (f *fakeDocker) run(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	switch {
	case contains(argv, "inspect") && !contains(argv, "image"):
		c, ok := f.running[argv[len(argv)-1]]
		if !ok {
			return nil, errors.New("no such container")
		}
		var mounts []map[string]string
		for _, m := range c.mounts {
			mounts = append(mounts, map[string]string{"Destination": m})
		}
		b, _ := json.Marshal([]any{map[string]any{"Image": c.image, "Config": map[string]any{
			"Env": c.env, "Hostname": c.hostname, "Labels": map[string]string{"com.docker.compose.config-hash": c.hash}},
			"HostConfig": map[string]any{"NanoCpus": c.nanoCPUs, "Memory": c.memory}, "Mounts": mounts}})
		return b, nil
	case contains(argv, "image") && contains(argv, "inspect") && contains(argv, "{{json .Config.Env}}"):
		return []byte(`["PATH=/bin","LANG=C.UTF-8"]`), nil
	case contains(argv, "image") && contains(argv, "inspect"):
		id, ok := f.imageIDs[argv[len(argv)-1]]
		if !ok {
			return nil, errors.New("no such image")
		}
		return []byte(id + "\n"), nil
	case contains(argv, "config") && contains(argv, "--hash"):
		if f.hashesFail {
			return nil, errors.New("unknown flag: --hash")
		}
		return []byte(f.hashes), nil
	case contains(argv, "up"):
		return []byte("done\n"), nil
	}
	return nil, fmt.Errorf("unexpected %v", argv)
}

func (f *fakeDocker) ups() []string {
	var out []string
	for _, c := range f.calls {
		if contains(c, "up") {
			out = append(out, strings.Join(c[index(c, "up"):], " "))
		}
	}
	return out
}

func contains(a []string, s string) bool { return index(a, s) >= 0 }

func writeFleet(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o700)
	for _, n := range []string{"agent-01", "agent-02", "agent-03"} {
		_ = os.WriteFile(filepath.Join(dir, "agents", n+".env"), []byte("# x\nCLAUDE_CODE_OAUTH_TOKEN=tok-"+n+"\nFLEET_TOKEN=relay\n"), 0o600)
	}
	p := filepath.Join(dir, "compose.pc2.yaml")
	_ = os.WriteFile(p, []byte(composeFile), 0o600)
	return p
}

// current: every container as the file wants it, but for what a test changes.
func current(f string) *fakeDocker {
	d := &fakeDocker{running: map[string]container{}, imageIDs: map[string]string{"img:2": "sha-2"},
		hashes: "fleet-anchor h0\nagent-01 h1\nagent-02 h2\nagent-03 h3\n"}
	d.running["fleet-anchor"] = container{image: "sha-2", hash: "h0"}
	defer func() {
		c := d.running["agent-01"]
		c.hostname, c.nanoCPUs, c.memory, c.mounts = "agent-01", 2e9, 4<<30, []string{"/code", "/work"}
		d.running["agent-01"] = c
	}()
	for i, n := range []string{"agent-01", "agent-02", "agent-03"} {
		brief := "BRIEF1"
		if n == "agent-01" {
			brief = "BRIEF2"
		}
		d.running[n] = container{image: "sha-2", hash: fmt.Sprintf("h%d", i+1), env: []string{
			"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=tok-" + n, "FLEET_TOKEN=relay", "FLEET_NAME=" + n, "FLEET_CLAUDE_MD=" + brief}}
	}
	return d
}

func plan(t *testing.T, d *fakeDocker, file string) []Change {
	t.Helper()
	cs, err := Planner{Prefix: []string{"docker", "--context", "pc2"}, File: file, Run: d.run}.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestPlanIsEmptyWhenNothingChanged(t *testing.T) {
	f := writeFleet(t)
	if cs := plan(t, current(f), f); len(cs) != 0 {
		t.Fatalf("plan = %+v, want nothing", cs)
	}
}

func TestPlanNamesTheReasonPerContainerAndNeverAValue(t *testing.T) {
	f := writeFleet(t)
	d := current(f)
	c := d.running["agent-01"]
	c.env = []string{"CLAUDE_CODE_OAUTH_TOKEN=OLD-SECRET", "FLEET_TOKEN=relay", "FLEET_NAME=agent-01", "FLEET_CLAUDE_MD=BRIEF2"}
	d.running["agent-01"] = c
	c = d.running["agent-02"]
	c.env = append(c.env[:4:4], "FLEET_CLAUDE_MD=OLDBRIEF")
	d.running["agent-02"] = c
	c = d.running["agent-03"]
	c.image = "sha-1"
	d.running["agent-03"] = c

	cs := plan(t, d, f)
	got := map[string]string{}
	for _, c := range cs {
		got[c.Service] = strings.Join(c.Reasons, "; ")
	}
	if got["agent-01"] != "env changed: CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("agent-01: %q", got["agent-01"])
	}
	if got["agent-02"] != "brief changed" {
		t.Errorf("agent-02: %q", got["agent-02"])
	}
	if got["agent-03"] != "image changed (img:2)" {
		t.Errorf("agent-03: %q", got["agent-03"])
	}
	var out bytes.Buffer
	Describe(&out, "pc2", cs)
	if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "tok-") || strings.Contains(out.String(), "OLDBRIEF") {
		t.Errorf("a value leaked: %s", out.String())
	}
	if !strings.Contains(out.String(), "agent-02 will be RECREATED — brief changed") {
		t.Errorf("describe: %s", out.String())
	}
}

func TestPlanCatchesComposeConfigChangesAndSurvivesAComposeWithoutHash(t *testing.T) {
	f := writeFleet(t)
	d := current(f)
	d.hashes = "fleet-anchor h0\nagent-01 h1\nagent-02 NEW\nagent-03 h3\n"
	cs := plan(t, d, f)
	if len(cs) != 1 || cs[0].Service != "agent-02" || !strings.HasPrefix(cs[0].Reasons[0], "compose config hash differs") {
		t.Fatalf("plan = %+v", cs)
	}
	d.hashesFail = true
	if cs := plan(t, d, f); len(cs) != 0 {
		t.Fatalf("without --hash: %+v", cs)
	}
}

func TestPlanDragsDependentsAlongAndCountsNewContainersAsCreates(t *testing.T) {
	f := writeFleet(t)
	d := current(f)
	a := d.running["fleet-anchor"]
	a.hash = "OLD"
	d.running["fleet-anchor"] = a
	delete(d.running, "agent-03")
	cs := plan(t, d, f)
	got := map[string]Change{}
	for _, c := range cs {
		got[c.Service] = c
	}
	if len(cs) != 4 || !strings.HasPrefix(got["fleet-anchor"].Reasons[0], "compose config hash differs") ||
		got["agent-01"].Reasons[0] != "depends on fleet-anchor" || !got["agent-03"].Create {
		t.Fatalf("plan = %+v", cs)
	}
}

func idler(d *fakeDocker, file string, states ...map[string]string) (*Idler, *bytes.Buffer, *[]time.Duration) {
	var out bytes.Buffer
	var sleeps []time.Duration
	i := 0
	return &Idler{
		Planner: Planner{Prefix: []string{"docker"}, File: file, Run: d.run},
		Agents:  map[string]bool{"agent-01": true, "agent-02": true, "agent-03": true},
		State: func(context.Context) (map[string]string, error) {
			s := states[min(i, len(states)-1)]
			i++
			return s, nil
		},
		Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
		Every: 10 * time.Second, Out: &out,
	}, &out, &sleeps
}

func TestWhenIdleRecreatesEachAgentAloneOnlyOnceNotWorking(t *testing.T) {
	f := writeFleet(t)
	d := current(f)
	for _, n := range []string{"agent-02", "agent-03"} {
		c := d.running[n]
		c.image = "sha-1"
		d.running[n] = c
	}
	cs := plan(t, d, f)
	// agent-02 works for two polls, agent-03 is idle throughout.
	i, out, sleeps := idler(d, f,
		map[string]string{"agent-01": "standby", "agent-02": "working", "agent-03": "waiting"},
		map[string]string{"agent-02": "working", "agent-03": "waiting"},
		map[string]string{"agent-02": "waiting", "agent-03": "waiting"},
	)
	if err := i.Up(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	ups := d.ups()
	want := []string{"up -d --no-deps agent-02", "up -d --no-deps agent-03", "up -d --remove-orphans"}
	if strings.Join(ups, "|") != strings.Join(want, "|") {
		t.Fatalf("ups = %q, want %q", ups, want)
	}
	if len(*sleeps) != 2 || !strings.Contains(out.String(), "waiting for agent-02 to stop working") {
		t.Errorf("sleeps %v, out:\n%s", *sleeps, out)
	}
	if strings.Count(out.String(), "waiting for agent-02") != 1 {
		t.Errorf("the wait was announced more than once:\n%s", out)
	}
}

func TestWhenIdleNeverRecreatesAWorkingAgentAndStopsWhenCancelled(t *testing.T) {
	f := writeFleet(t)
	d := current(f)
	c := d.running["agent-02"]
	c.image = "sha-1"
	d.running["agent-02"] = c
	i, _, _ := idler(d, f, map[string]string{"agent-02": "working"})
	ctx, cancel := context.WithCancel(context.Background())
	i.Sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	if err := i.Up(ctx, plan(t, d, f)); err == nil || !strings.Contains(err.Error(), "agent-02") {
		t.Fatalf("err = %v, want a cancelled wait naming agent-02", err)
	}
	if ups := d.ups(); len(ups) != 0 {
		t.Fatalf("ran %q while the agent was working", ups)
	}
}

func TestWhenIdleWaitsForEveryAgentAtOnceWhenTheAnchorIsRecreated(t *testing.T) {
	f := writeFleet(t)
	d := current(f)
	a := d.running["fleet-anchor"]
	a.hash = "OLD"
	d.running["fleet-anchor"] = a
	all := func(s string) map[string]string {
		return map[string]string{"agent-01": s, "agent-02": s, "agent-03": s}
	}
	busy := all("standby")
	busy["agent-03"] = "working"
	i, _, sleeps := idler(d, f, busy, all("standby"))
	if err := i.Up(context.Background(), plan(t, d, f)); err != nil {
		t.Fatal(err)
	}
	if ups := d.ups(); len(ups) != 1 || ups[0] != "up -d --remove-orphans" || len(*sleeps) != 1 {
		t.Fatalf("ups %q sleeps %v: want one plain up after everyone was idle", ups, *sleeps)
	}
}

func TestAnAgentFleetdDoesNotListIsNotKnownIdle(t *testing.T) {
	f := writeFleet(t)
	d := current(f)
	c := d.running["agent-02"]
	c.image = "sha-1"
	d.running["agent-02"] = c
	i, _, sleeps := idler(d, f, map[string]string{}, map[string]string{"agent-02": "standby"})
	if err := i.Up(context.Background(), plan(t, d, f)); err != nil || len(*sleeps) != 1 {
		t.Fatalf("err %v sleeps %v", err, *sleeps)
	}
}

func index(a []string, s string) int {
	for i, x := range a {
		if x == s {
			return i
		}
	}
	return -1
}

func TestPlanSaysWhatChangedBeyondEnvWithoutValues(t *testing.T) {
	f := writeFleet(t)
	reasons := func(mod func(*container)) string {
		d := current(f)
		c := d.running["agent-01"]
		mod(&c)
		d.running["agent-01"] = c
		var out []string
		for _, ch := range plan(t, d, f) {
			if ch.Service == "agent-01" {
				out = ch.Reasons
			}
		}
		return strings.Join(out, "; ")
	}
	if got := reasons(func(*container) {}); got != "" {
		t.Fatalf("an unchanged container: %q (PATH is baked into the image, not removed)", got)
	}
	if got := reasons(func(c *container) { c.env = append(c.env, "OTEL_OLD_KEY=secret-value") }); got != "env removed: OTEL_OLD_KEY" {
		t.Errorf("a key the render dropped: %q", got)
	}
	if got := reasons(func(c *container) { c.nanoCPUs = 1e9 }); got != "cpus limit changed" {
		t.Errorf("cpus: %q", got)
	}
	if got := reasons(func(c *container) { c.memory = 2 << 30 }); got != "memory limit changed" {
		t.Errorf("memory: %q", got)
	}
	if got := reasons(func(c *container) { c.mounts = []string{"/code"} }); got != "volumes changed" {
		t.Errorf("volumes: %q", got)
	}
	if got := reasons(func(c *container) { c.hostname = "other" }); got != "hostname changed" {
		t.Errorf("hostname: %q", got)
	}
}

// Real label and hash pairs from PC2 (compose 2.40.3, docker 29.9.0), taken
// right after an `up`: fleet-anchor's label equals `compose config --hash`;
// the agents' labels did not — and every field the plan compares matched.
func TestPlanWithRealShapedHashPairs(t *testing.T) {
	f := writeFleet(t)
	d := current(f)
	d.hashes = "agent-01 7148b51e9a3549b88531f6074c245faef557021f3cf537b4a711667bc2e748c1\n" +
		"fleet-anchor de763291cebfcf667d52bb6a14df7c1941ac0ab66732b7d1694eec98680fa575\n"
	a := d.running["fleet-anchor"]
	a.hash = "de763291cebfcf667d52bb6a14df7c1941ac0ab66732b7d1694eec98680fa575"
	d.running["fleet-anchor"] = a
	c := d.running["agent-01"]
	c.hash = "efc43d39f8d437dd8dbdf5fb1d43ee341875e04a4976e30a6f5b87b71af7e8a2"
	d.running["agent-01"] = c
	cs := plan(t, d, f)
	if len(cs) != 1 || cs[0].Service != "agent-01" {
		t.Fatalf("plan = %+v: the anchor (equal pair) must say nothing", cs)
	}
	r := cs[0].Reasons[0]
	if !strings.HasPrefix(r, "compose config hash differs") || !strings.Contains(r, "match the container") {
		t.Errorf("reason = %q: it must say the hash differs while everything compared matches, not just 'changed'", r)
	}
}
