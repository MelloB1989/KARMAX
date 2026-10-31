package aws

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is how the AWS source reads. Nothing in it names a real account:
// profiles come from the shared AWS config, thresholds from the environment.
type Config struct {
	ConfigFile      string            // ~/.aws/config, or AWS_CONFIG_FILE
	CredentialsFile string            // ~/.aws/credentials, or AWS_SHARED_CREDENTIALS_FILE; section names only
	Profiles        []string          // allow-list, in order of preference; empty means every usable profile
	Aliases         map[string]string // account id -> the name the operator calls it

	// AlarmLookback is how far back the first read of an account looks for
	// transitions; an alarm firing since before then is told once as context.
	AlarmLookback time.Duration

	// A day is a spike when it costs more than SpikeFactor times the mean of
	// the TrailingDays before it AND more than SpikeFloor (in the bill's
	// currency). The floor also decides whether a Cost Anomaly Detection
	// finding is urgent.
	SpikeFactor  float64
	SpikeFloor   float64
	TrailingDays int
	// CostRecheck is how often a day's cost is looked at again after the one
	// full read a day; each look is one billed Cost Explorer call. 0 turns
	// rechecks off.
	CostRecheck time.Duration
	// IncludeCredits counts credits and refunds against spend. Off by
	// default: an account running on credits would otherwise read as costing
	// nothing, and a spike burning through credits would go unseen.
	IncludeCredits bool
}

// Defaults.
const (
	DefaultAlarmLookback = 24 * time.Hour
	DefaultSpikeFactor   = 1.5
	DefaultSpikeFloor    = 5.0
	DefaultTrailingDays  = 7
	DefaultCostRecheck   = 6 * time.Hour
)

func (c Config) withDefaults() Config {
	if c.AlarmLookback <= 0 {
		c.AlarmLookback = DefaultAlarmLookback
	}
	if c.SpikeFactor <= 0 {
		c.SpikeFactor = DefaultSpikeFactor
	}
	if c.SpikeFloor < 0 {
		c.SpikeFloor = DefaultSpikeFloor
	}
	if c.TrailingDays <= 0 {
		c.TrailingDays = DefaultTrailingDays
	}
	if c.CostRecheck < 0 {
		c.CostRecheck = 0
	}
	return c
}

// ConfigFromEnv reads the source's settings, and reports false when
// KARMAX_SOURCE_AWS=off.
//
//	KARMAX_AWS_PROFILES            profiles to read, comma-separated, in order of preference (default: all)
//	KARMAX_AWS_ALIASES             names for accounts: 111122223333=prod,444455556666=staging
//	KARMAX_AWS_ALARM_LOOKBACK      how far the first read looks back (default 24h)
//	KARMAX_AWS_COST_SPIKE_FACTOR   a day above this times the trailing mean is a spike (default 1.5)
//	KARMAX_AWS_COST_SPIKE_FLOOR    ...and above this amount (default 5)
//	KARMAX_AWS_COST_TRAILING_DAYS  days in the trailing mean (default 7)
//	KARMAX_AWS_COST_RECHECK        how often to look at today's cost again (default 6h; 0 or off for never)
//	KARMAX_AWS_COST_INCLUDE_CREDITS=1  count credits and refunds against spend (default: spend before credits)
func ConfigFromEnv(getenv func(string) string, home string) (Config, bool) {
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }
	if strings.EqualFold(env("KARMAX_SOURCE_AWS"), "off") {
		return Config{}, false
	}
	c := Config{
		ConfigFile:      env("AWS_CONFIG_FILE"),
		CredentialsFile: env("AWS_SHARED_CREDENTIALS_FILE"),
		SpikeFloor:      DefaultSpikeFloor,
		CostRecheck:     DefaultCostRecheck,
	}
	if c.ConfigFile == "" {
		c.ConfigFile = filepath.Join(home, ".aws", "config")
	}
	if c.CredentialsFile == "" {
		c.CredentialsFile = filepath.Join(home, ".aws", "credentials")
	}
	for _, p := range strings.Split(env("KARMAX_AWS_PROFILES"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.Profiles = append(c.Profiles, p)
		}
	}
	for _, pair := range strings.Split(env("KARMAX_AWS_ALIASES"), ",") {
		id, name, ok := strings.Cut(pair, "=")
		if id, name = strings.TrimSpace(id), strings.TrimSpace(name); ok && id != "" && name != "" {
			if c.Aliases == nil {
				c.Aliases = map[string]string{}
			}
			c.Aliases[id] = name
		}
	}
	if d, err := time.ParseDuration(env("KARMAX_AWS_ALARM_LOOKBACK")); err == nil && d > 0 {
		c.AlarmLookback = d
	}
	if f, err := strconv.ParseFloat(env("KARMAX_AWS_COST_SPIKE_FACTOR"), 64); err == nil && f > 0 {
		c.SpikeFactor = f
	}
	if f, err := strconv.ParseFloat(env("KARMAX_AWS_COST_SPIKE_FLOOR"), 64); err == nil && f >= 0 {
		c.SpikeFloor = f
	}
	if n, err := strconv.Atoi(env("KARMAX_AWS_COST_TRAILING_DAYS")); err == nil && n > 0 {
		c.TrailingDays = n
	}
	c.IncludeCredits, _ = strconv.ParseBool(env("KARMAX_AWS_COST_INCLUDE_CREDITS"))
	switch v := env("KARMAX_AWS_COST_RECHECK"); {
	case strings.EqualFold(v, "off") || v == "0":
		c.CostRecheck = 0
	default:
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.CostRecheck = d
		}
	}
	return c.withDefaults(), true
}
