package aws

import (
	"reflect"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestTheSourceIsOnWithSaneDefaults(t *testing.T) {
	c, on := ConfigFromEnv(envOf(nil), "/home/example")
	if !on {
		t.Fatal("the source is off without being turned off")
	}
	if c.ConfigFile != "/home/example/.aws/config" || c.CredentialsFile != "/home/example/.aws/credentials" {
		t.Errorf("files = %q %q", c.ConfigFile, c.CredentialsFile)
	}
	if c.SpikeFactor != 1.5 || c.SpikeFloor != 5 || c.TrailingDays != 7 || c.CostRecheck != 6*time.Hour || c.AlarmLookback != 24*time.Hour {
		t.Errorf("defaults = %+v", c)
	}
	if len(c.Profiles) != 0 {
		t.Errorf("profiles = %v, want every usable one", c.Profiles)
	}
}

func TestTheSourceCanBeTurnedOff(t *testing.T) {
	if _, on := ConfigFromEnv(envOf(map[string]string{"KARMAX_SOURCE_AWS": "OFF"}), "/home/example"); on {
		t.Fatal("KARMAX_SOURCE_AWS=off left the source on")
	}
}

func TestEverySettingComesFromTheEnvironment(t *testing.T) {
	c, _ := ConfigFromEnv(envOf(map[string]string{
		"AWS_CONFIG_FILE":               "/etc/example/aws-config",
		"KARMAX_AWS_PROFILES":           " ops-readonly , staging ",
		"KARMAX_AWS_ALIASES":            "111122223333=example-prod, 444455556666 = example-staging",
		"KARMAX_AWS_ALARM_LOOKBACK":     "2h",
		"KARMAX_AWS_COST_SPIKE_FACTOR":  "2",
		"KARMAX_AWS_COST_SPIKE_FLOOR":   "0",
		"KARMAX_AWS_COST_TRAILING_DAYS": "14",
		"KARMAX_AWS_COST_RECHECK":       "off",
	}), "/home/example")
	if c.ConfigFile != "/etc/example/aws-config" {
		t.Errorf("config file = %q", c.ConfigFile)
	}
	if !reflect.DeepEqual(c.Profiles, []string{"ops-readonly", "staging"}) {
		t.Errorf("profiles = %v", c.Profiles)
	}
	if c.Aliases["111122223333"] != "example-prod" || c.Aliases["444455556666"] != "example-staging" {
		t.Errorf("aliases = %v", c.Aliases)
	}
	if c.AlarmLookback != 2*time.Hour || c.SpikeFactor != 2 || c.SpikeFloor != 0 || c.TrailingDays != 14 || c.CostRecheck != 0 {
		t.Errorf("config = %+v", c)
	}
}
