package console

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is console.yaml: written by `fleetctl render` for the console's
// host, read by fleet-console there.
type Config struct {
	Host            string      `yaml:"host"`
	Listen          string      `yaml:"listen"`            // loopback only; default 127.0.0.1:7880
	FleetdURL       string      `yaml:"fleetd_url"`        // fleetd's relay listener
	FleetdTokenFile string      `yaml:"fleetd_token_file"` // console-scope token for fleetd
	ClientTokenFile string      `yaml:"client_token_file"` // the token clients present here
	Orchestrator    string      `yaml:"orchestrator"`
	Repos           []string    `yaml:"repos"`
	Term            TermSize    `yaml:"term"` // default 220×50
	Agents          []AgentConf `yaml:"agents"`
}

// AgentConf is one roster agent as the console sees it.
type AgentConf struct {
	Name      string   `yaml:"name"`
	Host      string   `yaml:"host"`
	Container string   `yaml:"container"`
	Exec      []string `yaml:"exec"` // the docker prefix on the console's host, e.g. [docker]
	Live      bool     `yaml:"live"` // Host is the console's host
}

// LoadConfig reads, expands ~, fills defaults and validates; every error
// names its key.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7880"
	}
	if c.Term == (TermSize{}) {
		c.Term = TermSize{Cols: 220, Rows: 50}
	}
	home, _ := os.UserHomeDir()
	c.FleetdTokenFile = expand(c.FleetdTokenFile, home)
	c.ClientTokenFile = expand(c.ClientTokenFile, home)
	for i := range c.Repos {
		c.Repos[i] = expand(c.Repos[i], home)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("listen: %s is not a loopback address", c.Listen)
	}
	if !strings.HasPrefix(c.FleetdURL, "http://") && !strings.HasPrefix(c.FleetdURL, "https://") {
		return fmt.Errorf("fleetd_url: %q is not an http(s) URL", c.FleetdURL)
	}
	if c.FleetdTokenFile == "" {
		return fmt.Errorf("fleetd_token_file is required")
	}
	if c.ClientTokenFile == "" {
		return fmt.Errorf("client_token_file is required")
	}
	if c.Term.Cols <= 0 || c.Term.Rows <= 0 {
		return fmt.Errorf("term: cols and rows must be positive")
	}
	seen := map[string]bool{}
	for i, a := range c.Agents {
		switch {
		case a.Name == "":
			return fmt.Errorf("agents[%d]: name is required", i)
		case seen[a.Name]:
			return fmt.Errorf("agents: %q appears twice", a.Name)
		case a.Live && (a.Container == "" || len(a.Exec) == 0):
			return fmt.Errorf("agents: %q is live, so it needs container and exec", a.Name)
		}
		seen[a.Name] = true
	}
	if len(c.Agents) == 0 {
		return fmt.Errorf("agents: none")
	}
	return nil
}

func expand(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
