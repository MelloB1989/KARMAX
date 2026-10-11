package console

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodConfig = `
host: pc2
fleetd_url: http://fleetd.lan:7879
fleetd_token_file: ~/.config/fleet-console/fleetd.token
client_token_file: /etc/fc/client.token
orchestrator: mira
repos: [/srv/code/app]
agents:
  - {name: agent-01, host: pc2, container: agent-01, exec: [docker], live: true}
  - {name: agent-05, host: kali, live: false}
`

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "console.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(p)
}

func TestLoadConfigFillsDefaultsAndExpandsHome(t *testing.T) {
	c, err := load(t, goodConfig)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:7880" {
		t.Errorf("listen = %q", c.Listen)
	}
	if c.Term != (TermSize{Cols: 220, Rows: 50}) {
		t.Errorf("term = %+v", c.Term)
	}
	home, _ := os.UserHomeDir()
	if c.FleetdTokenFile != filepath.Join(home, ".config/fleet-console/fleetd.token") {
		t.Errorf("fleetd_token_file = %q", c.FleetdTokenFile)
	}
}

func TestLoadConfigNamesTheBadKey(t *testing.T) {
	cases := map[string]struct{ from, to, key string }{
		"not loopback":  {"host: pc2\n", "host: pc2\nlisten: 0.0.0.0:7880\n", "listen"},
		"bad url":       {"http://fleetd.lan:7879", "fleetd.lan:7879", "fleetd_url"},
		"no token":      {"client_token_file: /etc/fc/client.token\n", "", "client_token_file"},
		"dup agent":     {"name: agent-05", "name: agent-01", "agents"},
		"live no exec":  {"exec: [docker], ", "", "agents"},
		"live no image": {"container: agent-01, ", "", "agents"},
	}
	for name, c := range cases {
		_, err := load(t, strings.Replace(goodConfig, c.from, c.to, 1))
		if err == nil || !strings.Contains(err.Error(), c.key) {
			t.Errorf("%s: err = %v, want one naming %s", name, err, c.key)
		}
	}
}
