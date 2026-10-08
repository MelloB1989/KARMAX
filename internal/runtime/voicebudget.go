package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/MelloB1989/karmax/internal/cost"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"go.uber.org/zap"
)

// A hard lifetime spend cap on the calls-only Bedrock key.

const voiceBudgetLine = "I have to end this call, my call budget is used up. I will let you know once it is raised."

// unknownRate prices a model the table does not know, high on purpose.
var unknownRate = cost.Rate{In: 3, Out: 15}

type voiceLedger struct {
	store     *store.Store
	fp        string
	capUSD    float64
	worstTurn float64
	alert     func(string) error
	log       *zap.Logger
}

func keyFingerprint(key string) string {
	if key == "" {
		return "default-credentials"
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// worstTurnUSD bounds one turn: every model in the chain, retried, with full tool passes.
func worstTurnUSD(models []string) float64 {
	const passes, inTok, outTok = 4, 12000, 300
	total := 0.0
	for _, m := range models {
		r, ok := cost.RateFor(m)
		if !ok {
			r = unknownRate
		}
		total += 2 * passes * (inTok*r.In + outTok*r.Out) / 1e6
	}
	return total
}

func newVoiceLedger(s *store.Store, key string, capUSD float64, models []string,
	alert func(string) error, log *zap.Logger) *voiceLedger {
	if s == nil || capUSD <= 0 {
		return nil
	}
	return &voiceLedger{store: s, fp: keyFingerprint(key), capUSD: capUSD,
		worstTurn: worstTurnUSD(models), alert: alert, log: log}
}

// record bills one model call and returns its cost; only Bedrock spend counts against the key.
func (l *voiceLedger) record(u karmahelper.Usage) float64 {
	if l == nil || u.Provider != "bedrock" {
		return 0
	}
	usd, ok := cost.Estimate(cost.Usage{Model: u.Model, InputTokens: int64(u.InputTokens),
		OutputTokens: int64(u.OutputTokens), CacheRead: int64(u.CacheRead), CacheWrite: int64(u.CacheWrite)})
	if !ok {
		usd = (float64(u.InputTokens)*unknownRate.In + float64(u.OutputTokens)*unknownRate.Out) / 1e6
	}
	if _, err := l.store.AddVoiceSpend(l.fp, usd); err != nil {
		l.log.Warn("voice: could not record spend", zap.Error(err))
	}
	return usd
}

// blocked reports whether another turn could cross the cap, alerting once per crossing.
func (l *voiceLedger) blocked() bool {
	if l == nil {
		return false
	}
	spent, err := l.store.VoiceSpent(l.fp)
	if err != nil {
		l.log.Warn("voice: could not read spend", zap.Error(err))
		return false
	}
	if spent+l.worstTurn < l.capUSD {
		_ = l.store.ClearVoiceBudgetAlert(l.fp)
		return false
	}
	if first, err := l.store.ClaimVoiceBudgetAlert(l.fp); err == nil && first && l.alert != nil {
		if aerr := l.alert(fmt.Sprintf("Voice call budget reached: $%.2f spent of the $%.2f cap. "+
			"Calls will hang up until the cap is raised.", spent, l.capUSD)); aerr != nil {
			l.log.Warn("voice: could not send the budget alert", zap.Error(aerr))
		}
	}
	return true
}
