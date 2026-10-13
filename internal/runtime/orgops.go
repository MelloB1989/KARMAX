package runtime

import (
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/orgops"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// setupOrgOps builds the org engineering office when karmax.yaml turns it on:
// its loops join the loop registry and its tools join the agent's toolset, so
// it runs on the same bus, store, harness and chat channels as everything else.
func setupOrgOps(cfg *config.KarmaxConfig, b *bus.Log, s *store.Store, toolReg *tools.Registry, log *zap.Logger) *orgops.Service {
	if !cfg.OrgOps.Enabled {
		return nil
	}
	oc := cfg.OrgOps.WithDefaults()
	if err := oc.Validate(); err != nil {
		log.Error("org_ops is misconfigured; the org office is off", zap.Error(err))
		return nil
	}
	gh := orgops.NewGitHub(oc.GitHub.APIURL, orgops.TokenFrom(oc.GitHub))
	agentID := agentIDOf(cfg)
	svc := orgops.New(oc, gh, s, func(kind string, payload map[string]any) error {
		return b.Publish(bus.NewEvent(bus.EventKind(kind), agentID, payload))
	})
	for _, l := range svc.Loops() {
		loopkit.Register(l)
	}
	for _, t := range svc.Tools() {
		toolReg.Register(t)
	}
	log.Info("org office enabled",
		zap.String("org", oc.Org), zap.String("github", oc.GitHub.APIURL),
		zap.Int("probes", len(oc.Probes)), zap.String("fixer", oc.Fixer.Dispatcher))
	return svc
}
