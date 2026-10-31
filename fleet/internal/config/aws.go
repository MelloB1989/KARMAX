package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// AWS is the AWS account fleetctl's AWS tab shows (internal/awsdash), read
// with the AWS CLI v2 and an IAM access key; read-only keys are enough.
type AWS struct {
	// KeysCSV is the access-key CSV the IAM console downloads. fleetd
	// reads it at each read and hands the keys to the CLI alone.
	KeysCSV string `yaml:"keys_csv"`
	// Regions are read; default [ap-south-1, us-east-1]. [all] is every
	// region the account has enabled.
	Regions []string `yaml:"regions"`
	// Every is how old the resources may get before they are read again;
	// default 5m, at least 1m.
	Every time.Duration `yaml:"every"`
	// CostEvery is the same for Cost Explorer, which charges $0.01 a call
	// (a read is three); default 6h, at least 1h.
	CostEvery time.Duration `yaml:"cost_every"`
	// CLI is the aws binary; empty is the PATH's, else ~/.local/bin/aws.
	CLI string `yaml:"cli"`
}

// awsRegion is a region's name: ap-south-1, us-gov-west-1.
var awsRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

func (a *AWS) defaults(home string) {
	if len(a.Regions) == 0 {
		a.Regions = []string{"ap-south-1", "us-east-1"}
	}
	if a.Every <= 0 {
		a.Every = 5 * time.Minute
	}
	if a.CostEvery <= 0 {
		a.CostEvery = 6 * time.Hour
	}
	a.KeysCSV = expand(a.KeysCSV, home)
	a.CLI = expand(a.CLI, home)
}

func (a *AWS) validate() error {
	switch {
	case a.KeysCSV == "":
		return fmt.Errorf("aws: keys_csv is required (the access-key CSV the IAM console downloads)")
	case !filepath.IsAbs(a.KeysCSV):
		// Without echoing it: a key pasted where its file belongs.
		return fmt.Errorf("aws: keys_csv must be the CSV file's full path (or ~/…), not a key")
	case a.Every < time.Minute:
		return fmt.Errorf("aws: every %s: want at least 1m", a.Every)
	case a.CostEvery < time.Hour:
		return fmt.Errorf("aws: cost_every %s: want at least 1h: each Cost Explorer call is charged", a.CostEvery)
	}
	if slices.Contains(a.Regions, "all") {
		if len(a.Regions) > 1 {
			return fmt.Errorf("aws: regions: all stands alone: [all] or a list of regions")
		}
		return nil
	}
	for _, r := range a.Regions {
		if !awsRegion.MatchString(r) {
			return fmt.Errorf("aws: regions: %q is not a region (want e.g. ap-south-1)", strings.TrimSpace(r))
		}
	}
	return nil
}
