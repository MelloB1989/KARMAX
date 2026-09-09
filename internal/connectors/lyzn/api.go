// The wire: one request helper, one error translator, and what this machine
// says about itself when it pairs.
package lyzn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// defaultAPI is CloudFront in front of LYZN's API.
//
// Overridable per install through the `api_url` field rather than by a build
// flag, which is what lets a preview stack and this package's tests point
// somewhere else without a second binary.
const defaultAPI = "https://api.lyzn.ai"

// A minute is generous for every call in this package — they are all small
// JSON — and short enough that a hung poll does not outlive its own ticker.
var httpClient = &http.Client{Timeout: 60 * time.Second}

// pairCodeAlphabet is LYZN's, verbatim: no I, no O, no 0, no 1, because this
// is read off one screen and typed into another.
const pairCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

const pairCodeLength = 6

// normaliseCode is what somebody actually typed, turned into what was minted.
//
// The server normalises the same way, so this is not the check that matters —
// it is the one that happens while the code is still on screen. Case is
// irrelevant and the spaces or hyphens people add to make six characters
// readable are not part of the code.
func normaliseCode(raw string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if r == ' ' || r == '-' || r == '_' {
			continue
		}
		if !strings.ContainsRune(pairCodeAlphabet, r) {
			return "", fmt.Errorf("lyzn: %q is not a character a pairing code uses — check it against the app", string(r))
		}
		b.WriteRune(r)
	}
	code := b.String()
	if len(code) != pairCodeLength {
		return "", fmt.Errorf("lyzn: a pairing code is %d characters, and that is %d", pairCodeLength, len(code))
	}
	return code, nil
}

func root(cr connectorkit.Credentials) string {
	raw := strings.TrimSpace(cr.Get(keyAPI))
	if raw == "" {
		return defaultAPI
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	return strings.TrimRight(raw, "/")
}

// token is the bearer credential, from wherever KARMAX stored it. The console
// lifts a field literally named `access_token` into its own column, so both
// places are checked rather than assuming which one a given install used.
func token(cr connectorkit.Credentials) string {
	if t := strings.TrimSpace(cr.Get(keyToken)); t != "" {
		return t
	}
	return strings.TrimSpace(cr.AccessToken)
}

type sendOption func(*sendConfig)

type sendConfig struct {
	anonymous bool
}

// withoutToken is for the claim, and only the claim: the pairing code is the
// credential there, and sending a bearer that does not exist yet would be a
// header with the word "Bearer" and nothing after it.
func withoutToken() sendOption { return func(c *sendConfig) { c.anonymous = true } }

// send makes one request and decodes the answer.
func send(ctx context.Context, cr connectorkit.Credentials, method, path string, body any, out any, opts ...sendOption) error {
	cfg := sendConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, root(cr)+path, reader)
	if err != nil {
		return err
	}
	if !cfg.anonymous {
		tok := token(cr)
		if tok == "" {
			return fmt.Errorf("lyzn: not paired — enter a pairing code from the app first")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lyzn: could not reach %s: %w", root(cr), err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return apiError(res.StatusCode, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// apiError turns a status into something the operator can act on.
//
// The three that carry real meaning are translated, because LYZN's own
// sentence for them describes the phone's situation rather than the laptop's:
// a 401 here means this machine was unpaired in the app, and a 402 means the
// account stopped paying for automation — neither is something to retry, and
// both are fixed somewhere other than KARMAX.
func apiError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	detail := strings.TrimSpace(e.Error)
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	if len(detail) > 400 {
		detail = detail[:400] + "…"
	}

	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("lyzn: this machine is no longer paired (401) — it was unpaired in the app, or the token was replaced. Pair it again with a new code")
	case http.StatusPaymentRequired:
		return fmt.Errorf("lyzn: this account's plan does not carry automatic execution (402): %s", detail)
	case http.StatusConflict:
		return fmt.Errorf("lyzn: another machine got to that task first (409)")
	case http.StatusNotFound:
		return fmt.Errorf("lyzn: no such task on this account (404) — it may have been dismissed in the app")
	case http.StatusBadRequest:
		return fmt.Errorf("lyzn: that request was refused (400): %s", detail)
	default:
		return fmt.Errorf("lyzn: request failed (%d): %s", status, detail)
	}
}

// -- what this machine says about itself ------------------------------------

type identity struct {
	Name         string
	Hostname     string
	OS           string
	Version      string
	Capabilities []string
}

func machine(cr connectorkit.Credentials) identity {
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	name := strings.TrimSpace(cr.Get(keyName))
	if name == "" {
		name = host
	}
	return identity{
		Name:         name,
		Hostname:     host,
		OS:           runtime.GOOS + "/" + runtime.GOARCH,
		Version:      buildVersion(),
		Capabilities: capabilities(),
	}
}

var (
	versionOnce sync.Once
	version     string
)

// buildVersion is advisory — LYZN shows it beside the machine's name — so a
// build with nothing stamped into it says so rather than guessing.
func buildVersion() string {
	versionOnce.Do(func() {
		version = "dev"
		if info, ok := debug.ReadBuildInfo(); ok {
			if v := strings.TrimSpace(info.Main.Version); v != "" && v != "(devel)" {
				version = v
			}
		}
	})
	return version
}

var (
	capsOnce sync.Once
	caps     []string
)

// capabilities is what this machine can actually do, found by looking rather
// than declared.
//
// LYZN treats the list as advisory — it hands out work by task status, not by
// capability — so this is for the person reading the app's list of machines,
// and the honest answer there is which harnesses are installed. Computed once:
// a binary does not appear halfway through a process's life, and this is on
// the path of every heartbeat.
func capabilities() []string {
	capsOnce.Do(func() {
		caps = []string{"karmax", "shell"}
		for _, harness := range []struct{ bin, name string }{
			{"claude", "claude-code"},
			{"codex", "codex"},
		} {
			if _, err := exec.LookPath(harness.bin); err == nil {
				caps = append(caps, harness.name)
			}
		}
	})
	return caps
}
