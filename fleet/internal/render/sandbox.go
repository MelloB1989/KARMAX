package render

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/identity"
)

// A sandbox agent is lent to guests, so nothing of the operator's is in its
// container (docs/superpowers/specs/2026-10-25-fleetctl-tui-access-design.md,
// 2): no code directory, no shared volume or PID namespace, no build docker,
// GitHub, git identity or KARMAX, and no relay token or agent key. Its one
// fleet credential is its sandbox key, and it runs on fleet-sandbox, which
// reaches only the internet and fleetd (sandbox-net.sh.tmpl).

// SandboxNetwork is the bridge sandboxes run on, in every host's compose file.
const SandboxNetwork = "fleet-sandbox"

// sandboxCode is where a sandbox's code lives: a volume of its own, filled
// with fleetctl sync.
const sandboxCode = "/work/code"

// sandboxEnv is a sandbox's env file: its Claude login and its sandbox key,
// which also carries its telemetry. Nothing else.
func sandboxEnv(c *config.Config, env map[string]string, name string) map[string]string {
	key := identity.SandboxKey(env[c.TokenEnv], name)
	kv := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": env[c.Agents[name].TokenEnv],
		"FLEET_SANDBOX_KEY":       key,
	}
	if c.Hosts[c.Agents[name].Host].OTLPURL != "" && key != "" {
		kv["OTEL_EXPORTER_OTLP_HEADERS"] = "Authorization=Bearer%20" + key
	}
	return kv
}

// sandboxService adds a sandbox's service, its volumes and the network to doc.
// env is what every container is told (common); the code directory in it
// becomes the sandbox's own, and KARMAX's address goes.
func sandboxService(c *config.Config, doc *composeDoc, name string, env map[string]string, brief, dir string) {
	a := c.Agents[name]
	delete(env, "KARMAX_API_URL")
	env["FLEET_CODE"] = sandboxCode
	env["FLEET_AGENT"] = name
	env["FLEET_SANDBOX"] = "1" // agent-run makes /work/code instead of waiting for a share
	env["FLEET_SLOTS"] = strconv.Itoa(max(a.Slots, 1))
	env["FLEET_CLAUDE_MD"] = base64.StdEncoding.EncodeToString([]byte(brief))
	if a.Model != "" {
		env["FLEET_MODEL"] = a.Model
	}
	tini := true // its own PID namespace: tini is its PID 1
	doc.Services[name] = service{
		Image: c.Image, ContainerName: a.Container, Hostname: name, Init: &tini,
		Restart: "unless-stopped", StopGrace: "20s",
		EnvFile:     []string{filepath.Join(dir, "agents", name+".env")},
		Environment: env, ExtraHosts: []string{"host.docker.internal:host-gateway"},
		Volumes: []string{name + "-home:" + c.Home + "/.claude", name + "-work:/work", name + "-code:" + sandboxCode},
		CPUs:    a.CPUs, MemLimit: a.Memory,
		Networks: []string{SandboxNetwork},
	}
	for _, v := range []string{"-home", "-work", "-code"} {
		doc.Volumes[name+v] = map[string]any{}
	}
	if doc.Networks == nil {
		doc.Networks = map[string]any{}
	}
	// A fixed subnet, which the host's rules match; no traffic between the
	// containers on it.
	doc.Networks[SandboxNetwork] = map[string]any{
		"name": SandboxNetwork, "driver": "bridge",
		"driver_opts": map[string]string{
			"com.docker.network.bridge.name":       SandboxNetwork,
			"com.docker.network.bridge.enable_icc": "false",
		},
		"ipam": map[string]any{"config": []map[string]string{{"subnet": c.SandboxNet.Subnet}}},
	}
}

//go:embed sandbox-net.sh.tmpl
var sandboxNetTmpl string

//go:embed fleet-sandbox-net.service.tmpl
var sandboxUnitTmpl string

var (
	sandboxNet  = template.Must(template.New("sandbox-net").Parse(sandboxNetTmpl))
	sandboxUnit = template.Must(template.New("sandbox-unit").Parse(sandboxUnitTmpl))
)

// SandboxNetScript is where the unit runs the rules from, once installed.
const SandboxNetScript = "/usr/local/sbin/fleet-sandbox-net.sh"

// port is one of fleetd's ports a sandbox may reach: on this host itself
// (INPUT), or at fleetd's address on another (FORWARD).
type port struct{ Addr, Port, What string }

// fleetdPorts is where a host's sandboxes reach fleetd: its relay listener
// (fleetd_url) and its telemetry receiver (otlp_url). host.docker.internal is
// the host itself; anything else must be an address, which the rules name.
func fleetdPorts(c *config.Config, host string) (input, forward []port, err error) {
	h := c.Hosts[host]
	if h.FleetdURL == "" {
		return nil, nil, fmt.Errorf("host %s runs a sandbox agent but has no fleetd_url: a sandbox reaches fleetd only through it", host)
	}
	for _, e := range []struct{ url, what string }{{h.FleetdURL, "relay"}, {h.OTLPURL, "telemetry"}} {
		if e.url == "" {
			continue
		}
		u, err := url.Parse(e.url)
		if err != nil || u.Hostname() == "" || u.Port() == "" {
			return nil, nil, fmt.Errorf("host %s: %s %q: want http://<address>:<port> for the sandbox rules", host, e.what, e.url)
		}
		switch ip := net.ParseIP(u.Hostname()); {
		case u.Hostname() == "host.docker.internal":
			input = append(input, port{Port: u.Port(), What: e.what})
		case ip == nil || ip.To4() == nil:
			return nil, nil, fmt.Errorf("host %s: %s %q: the sandbox rules need fleetd's IPv4 address, not a name", host, e.what, e.url)
		default:
			forward = append(forward, port{Addr: ip.String(), Port: u.Port(), What: e.what})
		}
	}
	return input, forward, nil
}

// hasSandbox reports whether a host runs any sandbox agent.
func hasSandbox(c *config.Config, host string) bool {
	for _, n := range c.AgentsOn(host) {
		if c.Agents[n].Sandbox {
			return true
		}
	}
	return false
}

// writeSandboxNet writes a host's rules and their unit beside its compose
// file, or removes ones left from an earlier render when it runs no sandbox.
func writeSandboxNet(c *config.Config, host, dir string) error {
	script := filepath.Join(dir, "sandbox-net."+host+".sh")
	unit := filepath.Join(dir, "fleet-sandbox-net."+host+".service")
	if !hasSandbox(c, host) {
		for _, p := range []string{script, unit} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	input, forward, err := fleetdPorts(c, host)
	if err != nil {
		return err
	}
	var s, u strings.Builder
	data := map[string]any{"Host": host, "Subnet": c.SandboxNet.Subnet, "Input": input, "Forward": forward, "Script": SandboxNetScript}
	if err := sandboxNet.Execute(&s, data); err != nil {
		return err
	}
	if err := sandboxUnit.Execute(&u, data); err != nil {
		return err
	}
	if err := os.WriteFile(script, []byte(s.String()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(unit, []byte(u.String()), 0o600)
}
