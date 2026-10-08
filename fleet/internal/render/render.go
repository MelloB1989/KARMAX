// Package render turns fleet.yaml and the env file into what Docker runs:
// one 0600 env file per container (its own token, nothing else's), one
// compose file per host, and each agent's CLAUDE.md.
//
// Tokens go only into the env files, which are refused a directory anyone
// but you can read. They are never in a compose file, an image or argv.
package render

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/MelloB1989/karmax/fleet/internal/config"
)

//go:embed brief.md.tmpl
var briefTmpl string

var brief = template.Must(template.New("brief").Parse(briefTmpl))

// Brief renders an agent's CLAUDE.md.
func Brief(c *config.Config, name string) (string, error) {
	a, ok := c.Agents[name]
	if !ok {
		return "", fmt.Errorf("no agent %q", name)
	}
	var local, remote []string
	for _, n := range c.AgentNames() {
		if n == name {
			continue
		}
		if c.Agents[n].Host == a.Host {
			local = append(local, n)
		} else {
			remote = append(remote, n)
		}
	}
	var b strings.Builder
	err := brief.Execute(&b, map[string]any{
		"Agent": name, "Host": a.Host, "Code": c.Code, "Orchestrator": c.Orchestrator.Name,
		"OrchestratorLocal": c.Orchestrator.Host == a.Host, "Local": local, "Remote": remote,
	})
	return b.String(), err
}

// Render writes everything under dir.
func Render(c *config.Config, env map[string]string, dir string) error {
	if err := privateDir(dir); err != nil {
		return err
	}
	if err := privateDir(filepath.Join(dir, "agents")); err != nil {
		return err
	}
	need := []string{c.Orchestrator.TokenEnv}
	for _, n := range c.AgentNames() {
		need = append(need, c.Agents[n].TokenEnv)
	}
	var missing []string
	for _, k := range need {
		if strings.TrimSpace(env[k]) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the env file %s lacks %s (run `claude setup-token` while logged into each subscription)",
			c.EnvFile, strings.Join(missing, ", "))
	}

	for _, n := range c.AgentNames() {
		if err := writeEnv(filepath.Join(dir, "agents", n+".env"), map[string]string{
			"CLAUDE_CODE_OAUTH_TOKEN": env[c.Agents[n].TokenEnv],
			"FLEET_TOKEN":             env[c.RelayTokenEnv],
		}); err != nil {
			return err
		}
	}
	if err := writeEnv(filepath.Join(dir, "agents", c.Orchestrator.Container+".env"), map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": env[c.Orchestrator.TokenEnv],
		"FLEET_TOKEN":             env[c.TokenEnv],
		"KARMAX_API_TOKEN":        env["KARMAX_API_TOKEN"],
	}); err != nil {
		return err
	}

	for _, h := range c.HostNames() {
		doc, err := Compose(c, h, dir)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "compose."+h+".yaml"), doc, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// privateDir makes dir 0700, refusing one that others can already read.
func privateDir(dir string) error {
	st, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by others (%v); tokens are rendered only into a 0700 directory — chmod 700 it", dir, st.Mode().Perm())
	}
	return nil
}

func writeEnv(path string, kv map[string]string) error {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Rendered by fleetctl render. Secrets: keep 0600.\n")
	for _, k := range keys {
		if kv[k] == "" {
			continue
		}
		if strings.ContainsAny(kv[k], "\n\r") {
			return fmt.Errorf("%s: %s has a newline", path, k)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, kv[k])
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type service struct {
	Image         string            `yaml:"image"`
	ContainerName string            `yaml:"container_name"`
	Hostname      string            `yaml:"hostname,omitempty"`
	PID           string            `yaml:"pid,omitempty"`
	DependsOn     []string          `yaml:"depends_on,omitempty"`
	Restart       string            `yaml:"restart"`
	Init          *bool             `yaml:"init,omitempty"`
	Entrypoint    []string          `yaml:"entrypoint,omitempty"`
	Command       []string          `yaml:"command,omitempty"`
	EnvFile       []string          `yaml:"env_file,omitempty"`
	Environment   map[string]string `yaml:"environment,omitempty"`
	ExtraHosts    []string          `yaml:"extra_hosts,omitempty"`
	Volumes       []string          `yaml:"volumes,omitempty"`
	CPUs          string            `yaml:"cpus,omitempty"`
	MemLimit      string            `yaml:"mem_limit,omitempty"`
	StopGrace     string            `yaml:"stop_grace_period,omitempty"`
}

type composeDoc struct {
	Name     string             `yaml:"name"`
	Services map[string]service `yaml:"services"`
	Volumes  map[string]any     `yaml:"volumes"`
}

// Compose is one host's compose file: the anchor every container shares a
// PID namespace with, the host's agents, and on the orchestrator's host the
// karmax-brain container its harness sessions run in.
func Compose(c *config.Config, host, dir string) ([]byte, error) {
	h := c.Hosts[host]
	doc := composeDoc{Name: "fleet", Services: map[string]service{}, Volumes: map[string]any{
		"fleet-peers": map[string]any{}, "fleet-socks": map[string]any{},
	}}
	doc.Services["fleet-anchor"] = service{
		Image: c.Image, ContainerName: "fleet-anchor", Restart: "unless-stopped",
		Entrypoint: []string{"/usr/bin/tini", "--"}, Command: []string{"sleep", "infinity"},
	}
	shared := []string{
		c.Code + ":" + c.Code,
		"fleet-peers:" + c.Home + "/.claude/sessions",
		"fleet-socks:" + c.SockDir,
	}
	common := func(name string) map[string]string {
		e := map[string]string{
			"FLEET_HOST":      host,
			"XDG_RUNTIME_DIR": c.SockDir,
			"HOME":            c.Home,
		}
		if h.FleetdURL != "" {
			e["FLEETD_URL"] = h.FleetdURL
		}
		if h.KarmaxURL != "" {
			e["KARMAX_API_URL"] = h.KarmaxURL
		}
		if h.OTLPURL != "" {
			e["CLAUDE_CODE_ENABLE_TELEMETRY"] = "1"
			e["OTEL_METRICS_EXPORTER"] = "otlp"
			e["OTEL_LOGS_EXPORTER"] = "otlp"
			e["OTEL_EXPORTER_OTLP_PROTOCOL"] = "http/json"
			e["OTEL_EXPORTER_OTLP_ENDPOINT"] = h.OTLPURL
			e["OTEL_RESOURCE_ATTRIBUTES"] = "fleet.agent=" + name + ",fleet.host=" + host
		}
		return e
	}
	for _, name := range c.AgentsOn(host) {
		a := c.Agents[name]
		b, err := Brief(c, name)
		if err != nil {
			return nil, err
		}
		env := common(name)
		env["FLEET_AGENT"] = name
		env["FLEET_CLAUDE_MD"] = base64.StdEncoding.EncodeToString([]byte(b))
		if a.Model != "" {
			env["FLEET_MODEL"] = a.Model
		}
		doc.Services[name] = service{
			Image: c.Image, ContainerName: a.Container, Hostname: name, PID: "service:fleet-anchor",
			DependsOn: []string{"fleet-anchor"}, Restart: "unless-stopped", StopGrace: "20s",
			EnvFile:     []string{filepath.Join(dir, "agents", name+".env")},
			Environment: env, ExtraHosts: []string{"host.docker.internal:host-gateway"},
			Volumes: append(append([]string{}, shared...),
				name+"-home:"+c.Home+"/.claude", name+"-work:/work"),
			CPUs: a.CPUs, MemLimit: a.Memory,
		}
		doc.Volumes[name+"-home"] = map[string]any{}
		doc.Volumes[name+"-work"] = map[string]any{}
	}
	if c.Orchestrator.Host == host {
		o := c.Orchestrator
		karmax := filepath.Join(c.Home, ".karmax")
		doc.Services[o.Container] = service{
			Image: c.Image, ContainerName: o.Container, Hostname: o.Container, PID: "service:fleet-anchor",
			DependsOn: []string{"fleet-anchor"}, Restart: "unless-stopped",
			Entrypoint: []string{"/usr/bin/tini", "--"}, Command: []string{"sleep", "infinity"},
			EnvFile:     []string{filepath.Join(dir, "agents", o.Container+".env")},
			Environment: common(o.Name), ExtraHosts: []string{"host.docker.internal:host-gateway"},
			// The harness runs `docker exec -w <workdir>`: the sessions root is
			// mounted at its own path, and the brief every session inherits
			// beside it, read-only.
			Volumes: append(append([]string{}, shared...),
				o.Container+"-home:"+c.Home+"/.claude",
				karmax+"/sessions:"+karmax+"/sessions",
				karmax+"/CLAUDE.md:"+karmax+"/CLAUDE.md:ro"),
		}
		doc.Volumes[o.Container+"-home"] = map[string]any{}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Rendered by fleetctl render from fleet.yaml — edit that, not this.\n"), out...), nil
}
