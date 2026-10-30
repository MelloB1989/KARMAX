package runtime

import (
	"context"
	"strings"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/sources"
	awssource "github.com/MelloB1989/karmax/internal/sources/aws"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// registerAWSSource turns on the AWS event source: every usable profile in
// ~/.aws/config has its CloudWatch alarms and spend read, and what happened
// reaches agentID as a webhook.fired event the router, reflex screening and
// memory already handle. The agent gets aws.read to look deeper.
//
// Off with KARMAX_SOURCE_AWS=off; the other settings are in
// awssource.ConfigFromEnv. Health is written per profile ("aws:<profile>")
// and for the source as a whole ("aws") to the connector health table, and
// a login newly needed is told once in the app, never on WhatsApp.
func registerAWSSource(getenv func(string) string, home string, s *store.Store, b *bus.Log, agentID string,
	reg *tools.Registry, log *zap.Logger) bool {
	cfg, on := awssource.ConfigFromEnv(getenv, home)
	if !on {
		log.Info("AWS source off (KARMAX_SOURCE_AWS=off)")
		return false
	}
	src := awssource.New(awssource.Options{
		Config:  cfg,
		Dial:    awssource.SDKDialer(cfg),
		KV:      awssource.KV{Get: s.KVGet, Set: s.KVSet},
		Publish: func(e sources.Event) error { return b.Publish(e.BusEvent(agentID)) },
	})
	reg.Register(awssource.NewReadTool(src))
	health := awsHealth{s: s, agentID: agentID, log: log}

	loopkit.Register(loopkit.Loop{
		Name: "aws-alarms",
		Description: "Reads every usable AWS profile's CloudWatch alarms and tells KARMAX when one goes into ALARM (urgent) or back to OK. " +
			"Each account and region is read once; a profile whose login expired is skipped and its health names the command that fixes it.",
		Schedule: loopkit.Every("5m"),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			rep, err := src.PollAlarms(ctx)
			health.record(src.Health(), rep.Fixes, true)
			if rep.Published > 0 || err != nil {
				k.Logf("aws-alarms: %d account/regions, %d alarms (%d in ALARM), %d events published",
					rep.Readers, rep.Alarms, rep.InAlarm, rep.Published)
			}
			return err
		},
	})
	loopkit.Register(loopkit.Loop{
		Name: "aws-cost",
		Description: "Reads each AWS account's spend: one full Cost Explorer read a day and a cheap recheck every few hours, cached so a restart spends nothing. " +
			"Tells KARMAX urgently when a day spikes past the rule, a budget is crossed or Cost Anomaly Detection finds something, and summarises yesterday quietly for memory.",
		Schedule: loopkit.Every("1h"),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			rep, err := src.PollCost(ctx)
			health.record(src.Health(), nil, false)
			if rep.BilledCalls > 0 || rep.Published > 0 || err != nil {
				k.Logf("aws-cost: %d accounts, %d billed Cost Explorer calls, %d events published",
					rep.Readers, rep.BilledCalls, rep.Published)
			}
			return err
		},
	})
	return true
}

// awsHealth writes the source's health where connector health lives, and
// tells the operator once when a login is newly needed.
type awsHealth struct {
	s       *store.Store
	agentID string
	log     *zap.Logger
}

const (
	awsSourceKV = "aws-source"
	awsToldKey  = "logins-told"
)

// record saves every profile's health and the source's. Only the alarm poll
// passes tell: it sees every profile every few minutes, so it alone decides
// which logins are needed.
func (h awsHealth) record(hs []awssource.Health, fixes []string, tell bool) {
	for _, x := range hs {
		if err := h.s.SaveConnectorHealth(store.ConnectorHealth{Connector: "aws:" + x.Profile, Status: x.Status, Detail: x.Detail}); err != nil {
			h.log.Warn("could not save AWS profile health", zap.Error(err))
			return
		}
	}
	status, detail := awssource.Summary(hs)
	if err := h.s.SaveConnectorHealth(store.ConnectorHealth{Connector: "aws", Status: status, Detail: detail}); err != nil {
		h.log.Warn("could not save AWS source health", zap.Error(err))
	}
	if !tell {
		return
	}
	prev, _, _ := h.s.KVGet(awsSourceKV, awsToldKey)
	var told []string
	if prev != "" {
		told = strings.Split(prev, "\n")
	}
	if fresh := awssource.NewFixes(told, fixes); len(fresh) > 0 {
		// The app only: an expired login is not urgent, and WhatsApp is for
		// approvals and urgent things. The title names the new logins, since
		// alerts repeat-suppress by title and a different login is news.
		builtin.PushAppNotificationQuiet(h.s, h.agentID, "alert", "AWS needs a login: "+strings.Join(fresh, " ; "),
			"KARMAX cannot read some AWS accounts (alarms and spend) until this is run on its host: "+strings.Join(fixes, " ; ")+
				". The other accounts are still watched.")
	}
	if err := h.s.KVSet(awsSourceKV, awsToldKey, strings.Join(fixes, "\n"), 0); err != nil {
		h.log.Warn("could not remember which AWS logins were told", zap.Error(err))
	}
}
