package orgops

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The commented org_ops block in karmax.yaml.example is what operators copy;
// it must decode and validate.
func TestExampleConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../karmax.yaml.example")
	if err != nil {
		t.Skip(err)
	}
	s := string(raw)
	start := strings.Index(s, "# org_ops:")
	end := strings.Index(s[start:], "\n\n")
	if start < 0 || end < 0 {
		t.Fatal("no org_ops block in the example")
	}
	var lines []string
	for _, l := range strings.Split(s[start:start+end], "\n") {
		lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(l, "#"), " "))
	}
	var doc struct {
		OrgOps Config `yaml:"org_ops"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &doc); err != nil {
		t.Fatal(err)
	}
	c := doc.OrgOps.WithDefaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Org != "acme" || c.PolicyFor("acme/internal-tools").Merge != ModeAuto || len(c.Probes) != 1 {
		t.Fatalf("decoded wrong: %+v", c)
	}
	if CheckPR(c.Standards, PR{Title: "feat(api): x", Body: "b"}, nil) != nil {
		t.Fatal("example title pattern rejects a conventional title")
	}
}
