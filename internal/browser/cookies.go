package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Cookie is one cookie from the browser's own jar.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
}

// Cookies returns the cookies the browser holds for host and the domains
// above it.
//
// Read over the browser-level DevTools socket rather than a page's, so it
// works with no tab open — a Mac keeps Chrome running with every window
// closed — and rather than from the profile's cookie file, which is encrypted
// with a key in the OS keychain. It is also the only way to see an httpOnly
// cookie such as Instagram's sessionid without it passing through a page or a
// model's transcript.
func (s *Session) Cookies(ctx context.Context, host string) ([]Cookie, error) {
	endpoint := s.Endpoint(ctx)
	if endpoint == "" {
		return nil, ErrNotRunning
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := getJSON(ctx, endpoint+"/json/version", &version); err != nil {
		return nil, err
	}
	if version.WebSocketDebuggerURL == "" {
		return nil, errors.New("the browser did not name its DevTools socket")
	}

	conn, err := dialCDP(ctx, version.WebSocketDebuggerURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	readCtx, stop := context.WithCancel(ctx)
	defer stop()
	go conn.readLoop(readCtx)

	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := conn.call(ctx, "Storage.getCookies", nil, &out); err != nil {
		return nil, fmt.Errorf("read the browser's cookies: %w", err)
	}

	host = strings.ToLower(strings.TrimSpace(host))
	var matched []Cookie
	for _, c := range out.Cookies {
		if cookieFor(c.Domain, host) {
			matched = append(matched, c)
		}
	}
	return matched, nil
}

// cookieFor reports whether a cookie set for domain is sent to host: the
// domain itself, or any host under it.
func cookieFor(domain, host string) bool {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return d != "" && host != "" && (host == d || strings.HasSuffix(host, "."+d))
}
