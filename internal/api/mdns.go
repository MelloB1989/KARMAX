package api

import (
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"
	"go.uber.org/zap"
)

type mdnsAd struct {
	server *zeroconf.Server
}

// mdnsHost is the stable mDNS hostname the daemon claims, so the app can reach
// it at a predictable "<host>.local" regardless of the machine's real hostname.
// Override with KARMAX_MDNS_HOST (bare name, no ".local").
func mdnsHost() string {
	if h := strings.TrimSpace(os.Getenv("KARMAX_MDNS_HOST")); h != "" {
		return strings.TrimSuffix(h, ".local")
	}
	return "karmax"
}

// advertiseMDNS publishes a _karmax._tcp service AND a stable "karmax.local"
// hostname on the local network so the phone app can auto-discover and connect
// to the daemon over WiFi without knowing its IP.
//
// It uses RegisterProxy (rather than Register) specifically so the advertised
// hostname is "karmax.local" — with plain Register the A record follows the
// machine's real hostname (e.g. "mellob.local"), which the app can't predict.
// Best-effort: a failure is logged but never fatal (Tailscale / manual address
// still work).
func advertiseMDNS(port int, log *zap.Logger) *mdnsAd {
	host := mdnsHost()

	instance := "KARMAX"
	if h, err := os.Hostname(); err == nil && h != "" {
		instance = "KARMAX (" + h + ")"
	}

	ips := localIPv4s()
	txt := []string{
		"service=karmax",
		"version=" + Version,
		"port=" + strconv.Itoa(port),
		"path=/api",
	}

	server, err := zeroconf.RegisterProxy(
		instance,
		"_karmax._tcp",
		"local.",
		port,
		host, // advertises "<host>.local" (default karmax.local)
		ips,
		txt,
		nil,
	)
	if err != nil {
		// Fall back to plain Register so at least service discovery works even
		// if proxy registration is rejected on this platform.
		log.Warn("mDNS proxy advertise failed; falling back to plain Register", zap.Error(err))
		server, err = zeroconf.Register(instance, "_karmax._tcp", "local.", port, txt, nil)
		if err != nil {
			log.Warn("mDNS advertise failed (LAN auto-discovery unavailable)", zap.Error(err))
			return nil
		}
	}

	log.Info("mDNS advertising",
		zap.String("service", "_karmax._tcp"),
		zap.String("hostname", host+".local"),
		zap.Int("port", port),
		zap.Strings("ips", ips),
	)
	return &mdnsAd{server: server}
}

// localIPv4s returns the host's real LAN IPv4 addresses for the mDNS A records.
// It skips loopback, down, and virtual interfaces (docker/bridge/veth/vpn) —
// mDNS is link-local, so only physical WiFi/Ethernet addresses are reachable
// from a phone on the same network; advertising docker/Tailscale IPs would just
// make the client try dead routes first.
func localIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualIface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				out = append(out, ip4.String())
			}
		}
	}
	return out
}

// isVirtualIface reports whether an interface name looks like a container /
// bridge / VPN device rather than a real WiFi/Ethernet NIC.
func isVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"docker", "br-", "veth", "virbr", "tailscale", "tun", "utun", "wg", "zt", "vmnet", "vboxnet"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

func (m *mdnsAd) stop() {
	if m != nil && m.server != nil {
		m.server.Shutdown()
	}
}
