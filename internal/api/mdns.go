package api

import (
	"os"

	"github.com/grandcat/zeroconf"
	"go.uber.org/zap"
)

type mdnsAd struct {
	server *zeroconf.Server
}

// advertiseMDNS publishes a _karmax._tcp service on the local network so the
// phone app can auto-discover the daemon over WiFi. Best-effort: a failure is
// logged but never fatal (Tailscale / manual address still work).
func advertiseMDNS(port int, log *zap.Logger) *mdnsAd {
	instance := "KARMAX"
	if host, err := os.Hostname(); err == nil && host != "" {
		instance = "KARMAX (" + host + ")"
	}

	server, err := zeroconf.Register(
		instance,
		"_karmax._tcp",
		"local.",
		port,
		[]string{"service=karmax", "version=" + Version},
		nil,
	)
	if err != nil {
		log.Warn("mDNS advertise failed (LAN auto-discovery unavailable)", zap.Error(err))
		return nil
	}

	log.Info("mDNS advertising _karmax._tcp", zap.Int("port", port))
	return &mdnsAd{server: server}
}

func (m *mdnsAd) stop() {
	if m != nil && m.server != nil {
		m.server.Shutdown()
	}
}
