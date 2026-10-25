package render

import (
	"flag"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// fullYAML is a fleet with every credential an ordinary agent can be handed:
// a build host, GitHub tokens (fleet, per owner, packages), a git identity,
// SSH aliases and hidden paths. A sandbox must get none of it.
const fullYAML = fleetYAML + `build: {docker_host: "ssh://fleetbuild@192.0.2.20", key_env: FLEET_BUILD_KEY}
github: {token_env: GH_TOKEN_FLEET, owners: {example-org: GH_TOKEN_EXAMPLE_ORG}, packages_token_env: GH_PACKAGES, ssh_aliases: [gh-work]}
git: {name: "Example coding agent", email: build@example.com}
hide_from_agents: [secrets]
`

// sandboxes adds two sandbox agents, one on fleetd's host and one not.
const sandboxes = `  agent-09: {host: kali, token_env: CLAUDE_TOKEN_AGENT_09, sandbox: true}
  agent-10: {host: pc2,  token_env: CLAUDE_TOKEN_AGENT_10, sandbox: true}
`

func fullEnv() map[string]string {
	e := maps.Clone(env)
	maps.Copy(e, map[string]string{"FLEET_BUILD_KEY": "a2V5", "GH_TOKEN_FLEET": "gho_fleet", "GH_TOKEN_EXAMPLE_ORG": "gho_org",
		"GH_PACKAGES": "ghp_packages", "CLAUDE_TOKEN_AGENT_09": "sk-ant-oat01-A9", "CLAUDE_TOKEN_AGENT_10": "sk-ant-oat01-A10"})
	return e
}

func parse(t *testing.T, doc string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// withSandboxes renders the full fleet plus agent-09 and agent-10.
func withSandboxes(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(parse(t, insertAgents(fullYAML, sandboxes)), fullEnv(), dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// insertAgents puts more agents at the end of the agents: block.
func insertAgents(doc, more string) string {
	i := strings.Index(doc, "  agent-04:")
	j := i + strings.Index(doc[i:], "\n") + 1
	return doc[:j] + more + doc[j:]
}

// rawService is one service of a compose file as written, every key kept.
func rawService(t *testing.T, path, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	s, ok := doc.Services[name]
	if !ok {
		t.Fatalf("%s has no service %s", path, name)
	}
	return s
}

func keys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// The point of a sandbox is that nothing of the operator's is in it: its
// container mounts its own three volumes, no code directory, no shared
// volume, no hidden tmpfs, and has a PID namespace of its own.
func TestASandboxServiceHasExactlyItsOwnMounts(t *testing.T) {
	dir := withSandboxes(t)
	for host, name := range map[string]string{"kali": "agent-09", "pc2": "agent-10"} {
		s := rawService(t, filepath.Join(dir, "compose."+host+".yaml"), name)
		var vols []string
		for _, v := range s["volumes"].([]any) {
			vols = append(vols, v.(string))
		}
		want := []string{name + "-home:/home/op/.claude", name + "-work:/work", name + "-code:/work/code"}
		if !slices.Equal(vols, want) {
			t.Errorf("%s volumes = %v, want exactly %v", name, vols, want)
		}
		if _, ok := s["pid"]; ok {
			t.Errorf("%s shares a PID namespace: pid = %v", name, s["pid"])
		}
		if s["init"] != true {
			t.Errorf("%s init = %v, want true (tini as its own PID 1)", name, s["init"])
		}
		for _, k := range []string{"tmpfs", "depends_on"} {
			if _, ok := s[k]; ok {
				t.Errorf("%s has %s: %v", name, k, s[k])
			}
		}
		if s["mem_limit"] != "1536m" {
			t.Errorf("%s mem_limit = %v", name, s["mem_limit"])
		}
		c := loadCompose(t, filepath.Join(dir, "compose."+host+".yaml"))
		if _, ok := c.Volumes[name+"-code"]; !ok {
			t.Errorf("compose.%s.yaml does not declare %s-code", host, name)
		}
	}
}

// Its env file holds its Claude login and its one fleet credential, the
// sandbox key; its compose environment says where things are and nothing
// that reaches the operator's builds, GitHub or KARMAX. Both are exact sets:
// a key added later for ordinary agents must be decided on for sandboxes.
func TestASandboxEnvHoldsOnlyItsKeys(t *testing.T) {
	dir := withSandboxes(t)
	for host, name := range map[string]string{"kali": "agent-09", "pc2": "agent-10"} {
		file := readEnv(t, filepath.Join(dir, "agents", name+".env"))
		if got, want := keys(file), []string{"CLAUDE_CODE_OAUTH_TOKEN", "FLEET_SANDBOX_KEY", "OTEL_EXPORTER_OTLP_HEADERS"}; !slices.Equal(got, want) {
			t.Errorf("%s.env keys = %v, want exactly %v", name, got, want)
		}
		key := identity.SandboxKey("full", name)
		if file["FLEET_SANDBOX_KEY"] != key || file["OTEL_EXPORTER_OTLP_HEADERS"] != "Authorization=Bearer%20"+key {
			t.Errorf("%s.env: the sandbox key, and telemetry under it, want %s", name, key)
		}
		for k, v := range file {
			for _, bad := range []string{"relay", "full", "gho_", "ghp_", "a2V5", identity.Key("full", name)} {
				if strings.Contains(v, bad) {
					t.Errorf("%s.env %s holds %q", name, k, bad)
				}
			}
		}
		s := loadCompose(t, filepath.Join(dir, "compose."+host+".yaml")).Services[name]
		want := []string{"CLAUDE_CODE_ENABLE_TELEMETRY", "FLEETD_URL", "FLEET_AGENT", "FLEET_CLAUDE_MD", "FLEET_CODE", "FLEET_HOST",
			"FLEET_NAME", "FLEET_SANDBOX", "FLEET_SLOTS", "HOME", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL",
			"OTEL_LOGS_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_RESOURCE_ATTRIBUTES", "XDG_RUNTIME_DIR"}
		if got := keys(s.Environment); !slices.Equal(got, want) {
			t.Errorf("%s environment keys = %v, want exactly %v", name, got, want)
		}
		if s.Environment["FLEET_CODE"] != "/work/code" || s.Environment["FLEET_SANDBOX"] != "1" || s.Environment["FLEET_SLOTS"] != "1" {
			t.Errorf("%s FLEET_CODE, FLEET_SANDBOX, FLEET_SLOTS = %q, %q, %q", name, s.Environment["FLEET_CODE"], s.Environment["FLEET_SANDBOX"], s.Environment["FLEET_SLOTS"])
		}
		for _, k := range []string{"FLEET_TOKEN", "FLEET_AGENT_KEY", "GH_TOKEN", "GITHUB_TOKEN", "DOCKER_HOST", "FLEET_BUILD_KEY", "KARMAX_API_URL"} {
			if _, ok := file[k]; ok {
				t.Errorf("%s.env has %s", name, k)
			}
			if _, ok := s.Environment[k]; ok {
				t.Errorf("%s environment has %s", name, k)
			}
		}
	}
}

func TestASandboxIsOnTheSandboxNetwork(t *testing.T) {
	dir := withSandboxes(t)
	for host, name := range map[string]string{"kali": "agent-09", "pc2": "agent-10"} {
		path := filepath.Join(dir, "compose."+host+".yaml")
		s := rawService(t, path, name)
		if nets, _ := s["networks"].([]any); len(nets) != 1 || nets[0] != "fleet-sandbox" {
			t.Errorf("%s networks = %v", name, s["networks"])
		}
		if hosts, _ := s["extra_hosts"].([]any); len(hosts) != 1 || hosts[0] != "host.docker.internal:host-gateway" {
			t.Errorf("%s extra_hosts = %v", name, s["extra_hosts"])
		}
		b, _ := os.ReadFile(path)
		var doc struct {
			Networks map[string]struct {
				Name       string            `yaml:"name"`
				Driver     string            `yaml:"driver"`
				DriverOpts map[string]string `yaml:"driver_opts"`
				IPAM       struct {
					Config []struct {
						Subnet string `yaml:"subnet"`
					} `yaml:"config"`
				} `yaml:"ipam"`
			} `yaml:"networks"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		n, ok := doc.Networks["fleet-sandbox"]
		if !ok || n.Name != "fleet-sandbox" || n.Driver != "bridge" || len(n.IPAM.Config) != 1 || n.IPAM.Config[0].Subnet != "172.30.250.0/24" ||
			n.DriverOpts["com.docker.network.bridge.enable_icc"] != "false" {
			t.Errorf("compose.%s.yaml networks = %+v", host, doc.Networks)
		}
	}
	// An ordinary agent stays on the default network.
	if _, ok := rawService(t, filepath.Join(dir, "compose.kali.yaml"), "agent-01")["networks"]; ok {
		t.Error("agent-01 was moved to a network")
	}
}

// A fleet without sandboxes renders exactly as before, and adding sandboxes
// changes nothing of an ordinary agent's but the peers its brief lists.
func TestOrdinaryAgentsAreUnchanged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(parse(t, fullYAML), fullEnv(), dir); err != nil {
		t.Fatal(err)
	}
	golden := func(dir string) string {
		s := rawService(t, filepath.Join(dir, "compose.kali.yaml"), "agent-01")
		delete(s, "environment")
		out, err := yaml.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		e := loadCompose(t, filepath.Join(dir, "compose.kali.yaml")).Services["agent-01"].Environment
		delete(e, "FLEET_CLAUDE_MD")
		envOut, _ := yaml.Marshal(e)
		file, err := os.ReadFile(filepath.Join(dir, "agents", "agent-01.env"))
		if err != nil {
			t.Fatal(err)
		}
		all := string(out) + "--- environment\n" + string(envOut) + "--- agent-01.env\n" + string(file)
		return strings.ReplaceAll(all, dir, "<render>")
	}
	got := golden(dir)
	path := filepath.Join("testdata", "ordinary-agent.golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("agent-01 changed:\n%s\nwant:\n%s", got, want)
	}
	if got := golden(withSandboxes(t)); got != string(want) {
		t.Errorf("agent-01 changed when sandboxes were added:\n%s\nwant:\n%s", got, want)
	}
}

// The rules let a sandbox reach fleetd's relay and telemetry ports and
// nothing else of the host's or the LAN's: on fleetd's host as INPUT to the
// host itself, elsewhere as FORWARD to fleetd's address.
func TestTheNetworkRulesNameFleetdsPorts(t *testing.T) {
	dir := withSandboxes(t)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	kali, pc2 := read("sandbox-net.kali.sh"), read("sandbox-net.pc2.sh")
	for _, want := range []string{
		"SUBNET=172.30.250.0/24",
		"iptables -A FLEET-SANDBOX-IN -p tcp --dport 7879 -m comment --comment \"fleet-sandbox: fleetd relay\" -j ACCEPT",
		"iptables -A FLEET-SANDBOX-IN -p tcp --dport 4318 -m comment --comment \"fleet-sandbox: fleetd telemetry\" -j ACCEPT",
		"iptables -A FLEET-SANDBOX-IN -m comment --comment fleet-sandbox -j DROP",
		"10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16",
		// The tailnet and LAN are refused in raw, before tailscaled's FORWARD
		// jump (which accepts all that leaves by tailscale0) can let it out.
		"chain raw PREROUTING FLEET-SANDBOX-RAW",
		"iptables -t raw -A FLEET-SANDBOX-RAW -m addrtype --dst-type LOCAL",
		`iptables -t raw -A FLEET-SANDBOX-RAW -d "$net" -m comment --comment fleet-sandbox -j DROP`,
	} {
		if !strings.Contains(kali, want) {
			t.Errorf("kali's rules lack %q:\n%s", want, kali)
		}
	}
	if strings.Contains(kali, "FLEET-SANDBOX-FWD -d 192") {
		t.Errorf("kali runs fleetd, yet forwards to an address:\n%s", kali)
	}
	for _, want := range []string{
		"iptables -A FLEET-SANDBOX-FWD -d 192.168.1.10 -p tcp --dport 7879 -m comment --comment \"fleet-sandbox: fleetd relay\" -j RETURN",
		"iptables -A FLEET-SANDBOX-FWD -d 192.168.1.10 -p tcp --dport 4318 -m comment --comment \"fleet-sandbox: fleetd telemetry\" -j RETURN",
		"iptables -A FLEET-SANDBOX-IN -m comment --comment fleet-sandbox -j DROP",
	} {
		if !strings.Contains(pc2, want) {
			t.Errorf("pc2's rules lack %q:\n%s", want, pc2)
		}
	}
	if strings.Contains(pc2, "--dport 7879 -m comment --comment \"fleet-sandbox: fleetd relay\" -j ACCEPT") {
		t.Errorf("pc2 does not run fleetd, yet lets sandboxes reach its own port:\n%s", pc2)
	}
	// fleetd's own address goes before the private ranges are dropped.
	if i, j := strings.Index(pc2, "-d 192.168.1.10"), strings.Index(pc2, "192.168.0.0/16"); i < 0 || j < i {
		t.Errorf("pc2 drops the LAN before it allows fleetd:\n%s", pc2)
	}
	if !strings.Contains(pc2, "iptables -t raw -A FLEET-SANDBOX-RAW -d 192.168.1.10 -p tcp --dport 7879") {
		t.Errorf("pc2's raw rules do not let fleetd's address through:\n%s", pc2)
	}
	// This host's own addresses are FLEET-SANDBOX-IN's to decide, after the
	// raw table lets them by; fleetd's address before both.
	if i, j, k := strings.Index(pc2, "-t raw -A FLEET-SANDBOX-RAW -d 192.168.1.10"), strings.Index(pc2, "--dst-type LOCAL"), strings.Index(pc2, "-t raw -A FLEET-SANDBOX-RAW -d \"$net\""); !(i >= 0 && i < j && j < k) {
		t.Errorf("pc2's raw rules are out of order:\n%s", pc2)
	}
	unit := read("fleet-sandbox-net.pc2.service")
	for _, want := range []string{"After=docker.service", "Type=oneshot", "ExecStart=/bin/sh " + SandboxNetScript, "WantedBy=multi-user.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
}

// Running the rules again changes nothing: each run flushes the fleet's own
// chains and jumps to them once.
func TestTheNetworkRulesAreIdempotent(t *testing.T) {
	dir := withSandboxes(t)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "iptables.log")
	// A fake iptables that keeps the jumps it was given, so -C answers as
	// the real one would on a second run.
	fake := `#!/bin/sh
state=` + log + `.jumps
touch "$state"
t=filter
if [ "$1" = -t ]; then t=$2; shift 2; fi
echo "$*" >> ` + log + `
case "$1" in
-C) grep -qxF -e "$t $*" "$state" && exit 0; exit 1 ;;
-I) shift; echo "$t -C $1 $(echo "$*" | cut -d' ' -f3-)" >> "$state" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "iptables"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func() []string {
		_ = os.Remove(log)
		cmd := exec.Command("/bin/sh", filepath.Join(dir, "sandbox-net.pc2.sh"))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rules: %v\n%s", err, out)
		}
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	count := func(lines []string, prefix string) int {
		n := 0
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				n++
			}
		}
		return n
	}
	first, second := run(), run()
	if count(first, "-I ") != 3 || count(second, "-I ") != 0 {
		t.Errorf("jumps inserted: %d then %d, want 3 then 0\n%s", count(first, "-I "), count(second, "-I "), strings.Join(second, "\n"))
	}
	if count(second, "-F FLEET-SANDBOX-") != 3 || count(first, "-A ") != count(second, "-A ") {
		t.Errorf("a second run is not a flush and the same rules:\n%s", strings.Join(second, "\n"))
	}
}

// A host with no sandbox gets no rules, and loses any an earlier render left.
func TestNoSandboxNoRules(t *testing.T) {
	dir := withSandboxes(t)
	if err := Render(parse(t, fullYAML), fullEnv(), dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"sandbox-net.kali.sh", "fleet-sandbox-net.kali.service", "sandbox-net.pc2.sh"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", f, err)
		}
	}
}

// A sandbox reaches fleetd only through fleetd_url, which the rules must
// name as an address.
func TestASandboxHostNeedsFleetdsAddress(t *testing.T) {
	for _, tc := range []struct{ from, to, want string }{
		{`fleetd_url: "http://192.168.1.10:7879", `, ``, "no fleetd_url"},
		{`"http://192.168.1.10:7879"`, `"http://fleetd.example.com:7879"`, "IPv4 address"},
	} {
		doc := strings.Replace(insertAgents(fullYAML, sandboxes), tc.from, tc.to, 1)
		err := Render(parse(t, doc), fullEnv(), filepath.Join(t.TempDir(), "r"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s → %q: err = %v", tc.from, tc.to, err)
		}
	}
}

// A sandbox is told what it is and how code comes and goes, and nothing about
// the operator's code, builds, accounts or peers; it is no one's peer either.
func TestASandboxBriefSaysItIsASandbox(t *testing.T) {
	c := parse(t, insertAgents(fullYAML, sandboxes))
	b, err := Brief(c, "agent-09")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"You are a sandbox lent to a guest", "no GitHub access", "/work/code", "fleetctl sync agent-09:/work/", "fleetctl ask"} {
		if !strings.Contains(b, want) {
			t.Errorf("sandbox brief lacks %q", want)
		}
	}
	for _, bad := range []string{"/home/op/code", "kbuild", "karmax call", "memory recall", "fleetctl secret", "agent-01", "192.0.2.20", "example-org"} {
		if strings.Contains(b, bad) {
			t.Errorf("sandbox brief mentions %q", bad)
		}
	}
	o, err := Brief(c, "agent-01")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(o, "agent-09") || strings.Contains(o, "agent-10") {
		t.Errorf("an ordinary agent's brief lists a sandbox as a peer")
	}
}
