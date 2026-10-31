package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const awsBase = "home: /h\nhosts: {kali: {exec: [docker]}}\norchestrator: {host: kali, token_env: O}\nagents: {agent-01: {host: kali, token_env: T}}\n"

// No aws: section is no AWS tab.
func TestNoAWSSectionIsNil(t *testing.T) {
	c, err := Parse([]byte(awsBase))
	if err != nil || c.AWS != nil {
		t.Fatalf("aws = %+v, %v", c.AWS, err)
	}
}

func TestTheAWSSectionFillsDefaults(t *testing.T) {
	c, err := Parse([]byte(awsBase + "aws: {keys_csv: ~/secrets/aws/reader.csv}\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := c.AWS
	if a == nil || a.KeysCSV != "/h/secrets/aws/reader.csv" || !slices.Equal(a.Regions, []string{"ap-south-1", "us-east-1"}) ||
		a.Every != 5*time.Minute || a.CostEvery != 6*time.Hour || a.CLI != "" {
		t.Fatalf("aws = %+v", a)
	}
	c, err = Parse([]byte(awsBase + "aws: {keys_csv: /k.csv, regions: [all], every: 10m, cost_every: 12h, cli: ~/.local/bin/aws}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a := c.AWS; !slices.Equal(a.Regions, []string{"all"}) || a.Every != 10*time.Minute || a.CostEvery != 12*time.Hour || a.CLI != "/h/.local/bin/aws" {
		t.Fatalf("aws = %+v", a)
	}
}

func TestTheAWSSectionRefusesWhatWouldGoWrong(t *testing.T) {
	for name, c := range map[string]struct{ body, says string }{
		"no keys":          {"aws: {regions: [us-east-1]}", "keys_csv is required"},
		"a bad region":     {"aws: {keys_csv: /k.csv, regions: [mumbai]}", `"mumbai" is not a region`},
		"all and more":     {"aws: {keys_csv: /k.csv, regions: [all, us-east-1]}", "all stands alone"},
		"too often":        {"aws: {keys_csv: /k.csv, every: 10s}", "every 10s: want at least 1m"},
		"cost too often":   {"aws: {keys_csv: /k.csv, cost_every: 5m}", "cost_every 5m0s: want at least 1h"},
		"a key, not a csv": {"aws: {keys_csv: AKIAEXAMPLEKEY000001}", "keys_csv must be the CSV file's full path"},
	} {
		_, err := Parse([]byte(awsBase + c.body + "\n"))
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want %q", name, err, c.says)
		}
		if err != nil && strings.Contains(err.Error(), "AKIAEXAMPLEKEY000001") {
			t.Errorf("%s: the error repeats what looks like a key: %v", name, err)
		}
	}
}
