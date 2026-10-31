package runtime

import (
	"strings"
	"testing"
	"time"
)

// Every PostHog threshold is configuration, and one switch turns it off.
func TestPostHogSourceSettingsComeFromTheEnvironment(t *testing.T) {
	env := map[string]string{
		"KARMAX_SOURCE_POSTHOG_EVERY":          "10m",
		"KARMAX_SOURCE_POSTHOG_ERRORS_EVERY":   "30m",
		"KARMAX_SOURCE_POSTHOG_WINDOW":         "2h",
		"KARMAX_SOURCE_POSTHOG_BASELINE":       "72h",
		"KARMAX_SOURCE_POSTHOG_SPIKE_FACTOR":   "2.5",
		"KARMAX_SOURCE_POSTHOG_SPIKE_MIN":      "5",
		"KARMAX_SOURCE_POSTHOG_SPIKE_COOLDOWN": "3h",
		"KARMAX_SOURCE_POSTHOG_ANOMALY":        "off",
		"KARMAX_SOURCE_POSTHOG_METRICS":        "signed_up, order_placed",
		"KARMAX_SOURCE_POSTHOG_ANOMALY_TOP":    "8",
		"KARMAX_SOURCE_POSTHOG_ANOMALY_CHANGE": "0.3",
		"KARMAX_SOURCE_POSTHOG_ANOMALY_MIN":    "10",
	}
	cfg, every, on := postHogSourceConfig(func(k string) string { return env[k] })
	if !on || every != "10m" {
		t.Fatalf("on=%v every=%q", on, every)
	}
	if cfg.ErrorsEvery != 30*time.Minute || cfg.Window != 2*time.Hour || cfg.Baseline != 72*time.Hour ||
		cfg.SpikeFactor != 2.5 || cfg.SpikeMin != 5 || cfg.SpikeCooldown != 3*time.Hour || !cfg.AnomalyOff ||
		strings.Join(cfg.Metrics, ",") != "signed_up,order_placed" || cfg.AnomalyTop != 8 ||
		cfg.AnomalyChange != 0.3 || cfg.AnomalyMin != 10 {
		t.Errorf("config %+v", cfg)
	}
	if _, every, on := postHogSourceConfig(func(string) string { return "" }); !on || every != "5m" {
		t.Errorf("defaults: on=%v every=%q", on, every)
	}
	env["KARMAX_SOURCE_POSTHOG"] = "off"
	if _, _, on := postHogSourceConfig(func(k string) string { return env[k] }); on {
		t.Error("KARMAX_SOURCE_POSTHOG=off left the source on")
	}
}
