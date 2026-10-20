// Package tailnet says who is calling over Tailscale: tailscaled's LocalAPI
// WhoIs maps a peer's address to its node and the login that owns it. That
// is the identity fleetd's tailnet listener trusts; there is no token.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Who is the person behind a tailnet address.
type Who struct {
	Login string `json:"login"` // you@example.com; empty for a tagged device
	Name  string `json:"name"`  // display name
	Node  string `json:"node"`  // the machine, e.g. laptop.tailnet.ts.net
}

// ErrNotPeer is an address tailscaled does not know: not a tailnet peer.
var ErrNotPeer = errors.New("not a tailnet peer")

// Local asks tailscaled over its LocalAPI socket, and remembers each answer
// for a minute: a node's owner does not change between two requests.
type Local struct {
	Socket string
	hc     *http.Client
	mu     sync.Mutex
	seen   map[string]seen
}

type seen struct {
	who Who
	at  time.Time
}

const remember = time.Minute

// WhoIs is who is calling from addr (host:port, as http.Request.RemoteAddr).
func (l *Local) WhoIs(ctx context.Context, addr string) (Who, error) {
	ip, _, err := net.SplitHostPort(addr)
	if err != nil {
		return Who{}, err
	}
	l.mu.Lock()
	if s, ok := l.seen[ip]; ok && time.Since(s.at) < remember {
		l.mu.Unlock()
		return s.who, nil
	}
	if l.hc == nil {
		sock := l.Socket
		l.hc = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		}}
	}
	hc := l.hc
	l.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://local-tailscaled.sock/localapi/v0/whois?addr="+url.QueryEscape(addr), nil)
	if err != nil {
		return Who{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Who{}, fmt.Errorf("tailscaled: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Who{}, fmt.Errorf("%s: %w", ip, ErrNotPeer)
	}
	if resp.StatusCode != http.StatusOK {
		return Who{}, fmt.Errorf("tailscaled whois %s: %s", ip, resp.Status)
	}
	var r struct {
		Node struct {
			Name string
			Tags []string
		}
		UserProfile struct{ LoginName, DisplayName string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Who{}, fmt.Errorf("tailscaled whois %s: %w", ip, err)
	}
	w := Who{Login: r.UserProfile.LoginName, Name: r.UserProfile.DisplayName, Node: strings.TrimSuffix(r.Node.Name, ".")}
	// A tagged device belongs to no person ("tagged-devices"): a subnet
	// router or a server, whose traffic may be anyone's.
	if len(r.Node.Tags) > 0 {
		w.Login = ""
	}
	l.mu.Lock()
	if l.seen == nil {
		l.seen = map[string]seen{}
	}
	l.seen[ip] = seen{w, time.Now()}
	l.mu.Unlock()
	return w, nil
}
