package runtime

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/internal/integrations"
	"github.com/MelloB1989/karmax/internal/sources"
	posthogsrc "github.com/MelloB1989/karmax/internal/sources/posthog"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// postHogSourceConfig reads the PostHog source's settings, and reports false
// when KARMAX_SOURCE_POSTHOG=off.
//
//	KARMAX_SOURCE_POSTHOG_EVERY           how often alerts are polled (default 5m)
//	KARMAX_SOURCE_POSTHOG_ERRORS_EVERY    how often exceptions are counted (default 15m)
//	KARMAX_SOURCE_POSTHOG_WINDOW          "recent" for an error (default 1h)
//	KARMAX_SOURCE_POSTHOG_BASELINE        what recent is set beside (default 168h)
//	KARMAX_SOURCE_POSTHOG_SPIKE_FACTOR    a spike is this many times the usual rate (default 4)
//	KARMAX_SOURCE_POSTHOG_SPIKE_MIN       and at least this many in the window (default 20)
//	KARMAX_SOURCE_POSTHOG_SPIKE_COOLDOWN  one spike per issue per this long (default 6h)
//	KARMAX_SOURCE_POSTHOG_ANOMALY=off     no daily metric check
//	KARMAX_SOURCE_POSTHOG_METRICS         events the daily check reads, comma-separated (default: the busiest)
//	KARMAX_SOURCE_POSTHOG_ANOMALY_TOP     how many busiest events (default 5)
//	KARMAX_SOURCE_POSTHOG_ANOMALY_CHANGE  a move of this fraction of a usual day (default 0.5)
//	KARMAX_SOURCE_POSTHOG_ANOMALY_MIN     events a usual day needs to be judged (default 50)
func postHogSourceConfig(getenv func(string) string) (posthogsrc.Config, string, bool) {
	get := func(k string) string { return strings.TrimSpace(getenv("KARMAX_SOURCE_POSTHOG" + k)) }
	if strings.EqualFold(get(""), "off") {
		return posthogsrc.Config{}, "", false
	}
	num := func(k string) float64 { f, _ := strconv.ParseFloat(get(k), 64); return f }
	cfg := posthogsrc.Config{
		ErrorsEvery:   durationSetting(get("_ERRORS_EVERY")),
		Window:        durationSetting(get("_WINDOW")),
		Baseline:      durationSetting(get("_BASELINE")),
		SpikeFactor:   num("_SPIKE_FACTOR"),
		SpikeMin:      int(num("_SPIKE_MIN")),
		SpikeCooldown: durationSetting(get("_SPIKE_COOLDOWN")),
		AnomalyOff:    strings.EqualFold(get("_ANOMALY"), "off"),
		Metrics:       splitCSV(get("_METRICS")),
		AnomalyTop:    int(num("_ANOMALY_TOP")),
		AnomalyChange: num("_ANOMALY_CHANGE"),
		AnomalyMin:    int(num("_ANOMALY_MIN")),
	}
	every := get("_EVERY")
	if durationSetting(every) <= 0 {
		every = "5m"
	}
	return cfg, every, true
}

// registerPostHogSource registers the loop that turns PostHog into events.
func (rt *KarmaxRuntime) registerPostHogSource(reg *integration.Registry) {
	cfg, every, on := postHogSourceConfig(os.Getenv)
	if !on {
		rt.log.Info("posthog source is off (KARMAX_SOURCE_POSTHOG=off)")
		return
	}
	agentID := rt.primaryAgentID()
	src := posthogsrc.New(cfg, rt.store, func(e sources.Event) error {
		return rt.bus.Publish(e.BusEvent(agentID))
	})
	var lastOff string
	loopkit.Register(loopkit.Loop{
		Name: "posthog-source",
		Description: "Watches every PostHog project the operator's key can see: new and spiking errors and firing " +
			"insight alerts (urgent), and once a day a key metric that moved far from its week.",
		Schedule: loopkit.Every(every),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			res, err := src.Tick(ctx, integrations.PostHogAccounts(reg))
			if res.Off != lastOff {
				if res.Off != "" {
					k.Logf("posthog-source: %s", res.Off)
				} else {
					k.Logf("posthog-source: on")
				}
				lastOff = res.Off
			}
			if res.Published > 0 {
				k.Logf("posthog-source: %d projects, %d events", res.Projects, res.Published)
			}
			return err
		},
	})
	rt.log.Info("posthog source registered", zap.String("every", every))
}
