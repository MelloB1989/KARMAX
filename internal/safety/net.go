package safety

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Where a loop may send an HTTP request.
//
// Marketplace loops are third-party code running in KARMAX's process, and the
// interesting targets are local: wacli's API on :8765 and KARMAX's own on
// :9091. So loopback is blocked here even though the mesh allows it — the mesh
// is talking to a peer the operator chose, a loop is not.

// metadataHosts are never reachable, whatever the configuration says. These are
// the cloud credential endpoints; there is no legitimate reason for a loop.
var metadataHosts = map[string]bool{
	"169.254.169.254":          true,
	"metadata.google.internal": true,
	"metadata.goog":            true,
}

// AllowPrivateHTTP is the operator's escape hatch, for a loop that genuinely
// needs to reach something on the LAN.
func AllowPrivateHTTP() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("KARMAX_ALLOW_PRIVATE_HTTP")), "true")
}

// CheckURL refuses a request a loop should not be able to make.
//
// Every resolved address is checked, not just the first: a name that resolves
// to one public and one private address would otherwise slip through, which is
// how DNS rebinding works.
func CheckURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("safety: %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("safety: scheme %q is not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("safety: URL has no host")
	}
	if metadataHosts[strings.ToLower(host)] {
		return fmt.Errorf("safety: %s is a cloud metadata endpoint", host)
	}

	if ip := net.ParseIP(host); ip != nil {
		return checkIP(ip)
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("safety: could not resolve %s: %w", host, err)
	}
	for _, ip := range addrs {
		if err := checkIP(ip); err != nil {
			return err
		}
	}
	return nil
}

func checkIP(ip net.IP) error {
	if metadataHosts[ip.String()] {
		return fmt.Errorf("safety: %s is a cloud metadata endpoint", ip)
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return fmt.Errorf("safety: %s is link-local", ip)
	}
	if ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("safety: %s is not a routable destination", ip)
	}
	if AllowPrivateHTTP() {
		return nil
	}
	if ip.IsLoopback() {
		return fmt.Errorf("safety: %s is loopback, where KARMAX's own APIs live (set KARMAX_ALLOW_PRIVATE_HTTP=true to permit)", ip)
	}
	if ip.IsPrivate() {
		return fmt.Errorf("safety: %s is a private address (set KARMAX_ALLOW_PRIVATE_HTTP=true to permit)", ip)
	}
	// 100.64.0.0/10, carrier-grade NAT and the Tailscale range.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return fmt.Errorf("safety: %s is in the shared address space (set KARMAX_ALLOW_PRIVATE_HTTP=true to permit)", ip)
	}
	return nil
}
