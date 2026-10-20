package main

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/server"
)

func TestAConsoleTokenValueMayNotBeTheFullOrRelayOne(t *testing.T) {
	cfg, err := config.Parse([]byte("home: /h\nhosts: {k: {exec: [docker]}}\norchestrator: {host: k, token_env: O}\n" +
		"console: {host: k, fleetd_url: 'http://f:1', token_env: CONSOLE_TOKEN}\nagents: {agent-01: {host: k, token_env: T}}"))
	if err != nil {
		t.Fatal(err)
	}
	for name, tk := range map[string]server.Tokens{
		"full":  {Full: "s3cret", Relay: "r", Console: "s3cret"},
		"relay": {Full: "f", Relay: "s3cret", Console: "s3cret"},
	} {
		err := checkTokens(tk, cfg)
		if err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: err = %v (want a refusal that does not echo the value)", name, err)
		}
	}
	if err := checkTokens(server.Tokens{Full: "f", Relay: "r", Console: "c"}, cfg); err != nil {
		t.Errorf("distinct tokens refused: %v", err)
	}
	// No console token set (or an unset relay token) is not a collision.
	if err := checkTokens(server.Tokens{Full: "f"}, cfg); err != nil {
		t.Errorf("unset tokens refused: %v", err)
	}
}

// The tailnet listener binds tailscale0's address even when a boot has not
// given it to the host yet (192.0.2.1 is an address no host has).
func TestTheTailnetListenerBindsAnAddressNotYetUp(t *testing.T) {
	ln, err := (&net.ListenConfig{Control: freebind}).Listen(context.Background(), "tcp", "192.0.2.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}
