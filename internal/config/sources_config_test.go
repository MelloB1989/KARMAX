package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The alarm gate's keys load as the example documents them; absent, they are
// zero, which the runtime reads as "debounce at its default, the rest off".
func TestAWSAlarmGateKeysLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "karmax.yaml")
	body := `
sources:
  aws:
    alarms:
      debounce_polls: 1
      cooldown: 6h
      ignore: ["TargetTracking-*-AlarmHigh-*", "TargetTracking-*-AlarmLow-*"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Sources.AWS.Alarms
	if a.DebouncePolls == nil || *a.DebouncePolls != 1 || a.Cooldown != 6*time.Hour || len(a.Ignore) != 2 {
		t.Fatalf("alarms = %+v", a)
	}

	example, err := Load("../../karmax.yaml.example")
	if err != nil {
		t.Fatalf("the example config does not load: %v", err)
	}
	if a := example.Sources.AWS.Alarms; a.DebouncePolls != nil || a.Cooldown != 0 || len(a.Ignore) != 0 {
		t.Errorf("the example sets the alarm gate (%+v); its block is commented, so the defaults apply", a)
	}
}
