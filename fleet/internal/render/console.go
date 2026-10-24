package render

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/console"
)

// ConsoleConfig is the console.yaml for fleet.yaml's console: section: every
// roster agent, live where it runs on the console's own host. Its token file
// paths are the console host's own, under console.dir.
func ConsoleConfig(c *config.Config) (*console.Config, error) {
	k := c.Console
	if k == nil {
		return nil, fmt.Errorf("fleet.yaml has no console: section")
	}
	out := &console.Config{
		Host:            k.Host,
		Listen:          k.Listen,
		FleetdURL:       k.FleetdURL,
		FleetdTokenFile: k.Dir + "/fleetd.token",
		ClientTokenFile: k.Dir + "/client.token",
		Orchestrator:    c.Orchestrator.Name,
		Repos:           k.Repos,
	}
	for _, w := range c.Workers() {
		ac := console.AgentConf{Name: w.Name, Host: w.Host, Container: w.Container, Live: w.Host == k.Host}
		if w.Slot > 1 {
			ac.Agent, ac.Slot = w.Agent, w.Slot
		}
		if ac.Live {
			// The console runs on this host: its docker is the local one,
			// whatever prefix the fleetd host uses to reach it.
			ac.Exec = []string{"docker"}
		}
		out.Agents = append(out.Agents, ac)
	}
	return out, nil
}

// writeConsole writes <dir>/<console host>/console/console.yaml and, 0600,
// fleetd.token beside it. Nothing when fleet.yaml has no console: section.
func writeConsole(c *config.Config, env map[string]string, dir string) error {
	k := c.Console
	if k == nil {
		return nil
	}
	tok := strings.TrimSpace(env[k.TokenEnv])
	if tok == "" {
		return fmt.Errorf("the env file %s lacks %s (the console's token for fleetd)", c.EnvFile, k.TokenEnv)
	}
	cc, err := ConsoleConfig(c)
	if err != nil {
		return err
	}
	doc, err := yaml.Marshal(cc)
	if err != nil {
		return err
	}
	cdir := filepath.Join(dir, k.Host, "console")
	if err := privateDir(filepath.Join(dir, k.Host)); err != nil {
		return err
	}
	if err := privateDir(cdir); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cdir, "console.yaml"), doc, 0o600); err != nil {
		return err
	}
	return writeSecret(filepath.Join(cdir, "fleetd.token"), tok+"\n")
}

// writeSecret writes a 0600 file atomically.
func writeSecret(path, body string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
