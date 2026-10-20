package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// serverFile is where connect keeps the fleetd it reached: for a machine
// that is not a fleet host, the one every command talks to.
func serverFile() (string, error) {
	if p := os.Getenv("FLEETCTL_SERVER_FILE"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "fleetctl", "server"), nil
}

// savedServer is the fleetd connect saved, "" when none.
func savedServer() string {
	p, err := serverFile()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// connectCmd: connect <url>. Asks that fleetd who you are — on its tailnet
// listener, Tailscale says, no token — and saves it as this machine's fleetd.
func connectCmd(args []string) error {
	const use = "usage: fleetctl connect http://<fleetd host>:7878"
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(use)
	}
	u := strings.TrimRight(args[0], "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	c := &client{base: u, timeout: 20 * time.Second}
	var who struct{ Scope, Name string }
	if err := c.do("GET", "/v1/whoami", nil, &who); err != nil {
		return err
	}
	p, err := serverFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(u+"\n"), 0o644); err != nil {
		return err
	}
	switch who.Scope {
	case "member":
		fmt.Printf("connected to %s as %s: you run tasks as yourself, and see and close your own\n", u, who.Name)
	case "full":
		fmt.Printf("connected to %s as %s, an operator\n", u, who.Name)
	default:
		fmt.Printf("connected to %s as %s (scope %s)\n", u, who.Name, who.Scope)
	}
	return nil
}
