package integration

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Joining the registry without rewriting what already works.
//
// A comms channel and a connector are different things that happen to need the
// same three answers — what do you need, how do I log you in, are you working.
// These adapters supply those answers for code that predates the question,
// rather than making every existing integration implement a new interface
// before anything can be connected.

// Simple is an integration described by values rather than a type.
//
// Most integrations have nothing to say beyond their manifest and a health
// check, and making each one a named struct with three methods is ceremony that
// obscures how little is actually there.
type Simple struct {
	Meta      Manifest
	Method    connectorkit.AuthMethod
	CheckFunc func(ctx context.Context, c connectorkit.Credentials) error
}

func (s Simple) Manifest() Manifest            { return s.Meta }
func (s Simple) Auth() connectorkit.AuthMethod { return s.Method }
func (s Simple) Health(ctx context.Context, c connectorkit.Credentials) error {
	if s.CheckFunc == nil {
		return nil
	}
	return s.CheckFunc(ctx, c)
}

// APIKey describes an integration that needs one or more keys pasted in.
func APIKey(m Manifest, keyField string, check func(context.Context, connectorkit.Credentials) error) Simple {
	m.Kind = orDefault(m.Kind, KindChannel)
	return Simple{
		Meta:      m,
		Method:    connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: keyField},
		CheckFunc: check,
	}
}

// CLISession describes an integration whose session belongs to a host binary.
//
// KARMAX cannot log these in and should not pretend to: wacli holds a WhatsApp
// pairing and gws holds a Google session, each in its own store, and the honest
// thing is to check and report rather than to keep a second copy of a secret we
// do not own.
func CLISession(m Manifest, binary string, check func(context.Context, connectorkit.Credentials) error) Simple {
	m.Kind = orDefault(m.Kind, KindChannel)
	return Simple{
		Meta:      m,
		Method:    connectorkit.AuthMethod{Kind: connectorkit.AuthCLI, CLIBinary: binary},
		CheckFunc: check,
	}
}

// FromConnector adapts a connectorkit.Connector, which already answers all
// three questions in its own vocabulary.
func FromConnector(c connectorkit.Connector, account string) Integration {
	m := c.Manifest()
	return Simple{
		Meta: Manifest{
			ID: connectorID(m.ID, account), Name: m.Name, Description: m.Description,
			Kind: KindConnector, Config: m.Config, Account: account,
		},
		Method:    c.Auth(),
		CheckFunc: c.Health,
	}
}

// connectorID keys an integration, with the account when there is one.
//
// Several logins to the same provider is the GitHub case: a work account and a
// personal one, both enabled, each with its own credential. The account is part
// of the identity rather than a field inside it, so the credential store keeps
// them apart for free.
func connectorID(base, account string) string {
	if strings.TrimSpace(account) == "" {
		return base
	}
	return base + ":" + account
}

// SplitID separates an integration id into provider and account.
func SplitID(id string) (provider, account string) {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

// CheckBinarySession runs a host binary's own status command.
//
// The output is deliberately included in the error: `gws` explains an expired
// Google token in its stdout, and swallowing that turns an actionable message
// into "exit status 2".
func CheckBinarySession(binary string, args ...string) func(context.Context, connectorkit.Credentials) error {
	return func(ctx context.Context, _ connectorkit.Credentials) error {
		path := binary
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("no path configured for this integration's binary")
		}
		if _, err := exec.LookPath(path); err != nil {
			return fmt.Errorf("%s is not on this machine (%v)", path, err)
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, path, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v — %s", path, strings.Join(args, " "), err,
				trunc(strings.TrimSpace(string(out)), 300))
		}
		return nil
	}
}

func orDefault(k, fallback Kind) Kind {
	if k == "" {
		return fallback
	}
	return k
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
