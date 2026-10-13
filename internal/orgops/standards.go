package orgops

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Severity orders findings. A blocker stops a merge whatever the policy says.
type Severity string

const (
	SevInfo    Severity = "info"
	SevWarning Severity = "warning"
	SevBlocker Severity = "blocker"
)

func (s Severity) rank() int {
	switch s {
	case SevBlocker:
		return 2
	case SevWarning:
		return 1
	}
	return 0
}

// Finding is one thing a check found.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Path     string   `json:"path,omitempty"`
	Line     int      `json:"line,omitempty"`
}

type compiledStandards struct {
	Standards
	title *regexp.Regexp
}

func compileStandards(s Standards) (compiledStandards, error) {
	c := compiledStandards{Standards: s}
	if s.TitlePattern != "" {
		re, err := regexp.Compile(s.TitlePattern)
		if err != nil {
			return c, fmt.Errorf("org_ops.standards.title_pattern: %w", err)
		}
		c.title = re
	}
	return c, nil
}

// CheckPR runs the org's deterministic standards over a PR. These are the
// checks a model should never be asked to do: they have one right answer and
// cost nothing, so a model's attention is left for what needs judgement.
func CheckPR(s Standards, pr PR, files []PRFile) []Finding {
	c, err := compileStandards(s)
	if err != nil {
		return []Finding{{Rule: "config", Severity: SevWarning, Message: err.Error()}}
	}
	var out []Finding
	if c.title != nil && !c.title.MatchString(pr.Title) {
		out = append(out, Finding{Rule: "title", Severity: SevWarning,
			Message: fmt.Sprintf("PR title %q does not follow the org convention (%s)", pr.Title, s.TitlePattern)})
	}
	if s.RequireDescription && strings.TrimSpace(pr.Body) == "" {
		out = append(out, Finding{Rule: "description", Severity: SevWarning,
			Message: "PR has no description — say what changes and why"})
	}
	changed := 0
	for _, f := range files {
		changed += f.Additions + f.Deletions
	}
	if s.MaxChangedLines > 0 && changed > s.MaxChangedLines {
		out = append(out, Finding{Rule: "size", Severity: SevWarning,
			Message: fmt.Sprintf("PR changes %d lines (limit %d) — consider splitting it", changed, s.MaxChangedLines)})
	}
	for _, f := range files {
		for _, g := range s.ProtectedPaths {
			if pathMatch(g, f.Filename) {
				out = append(out, Finding{Rule: "protected-path", Severity: SevBlocker, Path: f.Filename,
					Message: fmt.Sprintf("%s is a protected path (%s) — a human must approve this change", f.Filename, g)})
				break
			}
		}
	}
	if s.RequireTests {
		code, tests := false, false
		for _, f := range files {
			if isTestPath(s.TestPaths, f.Filename) {
				tests = true
			} else if isSourceFile(f.Filename) {
				code = true
			}
		}
		if code && !tests {
			out = append(out, Finding{Rule: "tests", Severity: SevWarning,
				Message: "source code changed but no tests were added or updated"})
		}
	}
	return out
}

// MissingFiles lists the org's required files a repository does not have.
func MissingFiles(s Standards, has func(string) bool) []Finding {
	var out []Finding
	for _, f := range s.RequiredFiles {
		if !has(f) {
			out = append(out, Finding{Rule: "required-file", Severity: SevWarning, Path: f,
				Message: fmt.Sprintf("repository is missing %s", f)})
		}
	}
	return out
}

// pathMatch matches a glob against a path, where "dir/**" covers everything
// below dir and a pattern with no slash matches the base name anywhere.
func pathMatch(glob, name string) bool {
	if strings.HasSuffix(glob, "/**") {
		return strings.HasPrefix(name, strings.TrimSuffix(glob, "**"))
	}
	if !strings.Contains(glob, "/") {
		ok, _ := path.Match(glob, path.Base(name))
		return ok
	}
	ok, _ := path.Match(glob, name)
	return ok
}

func isTestPath(globs []string, name string) bool {
	for _, g := range globs {
		if pathMatch(g, name) {
			return true
		}
	}
	base := path.Base(name)
	return strings.HasSuffix(base, "_test.go") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.HasPrefix(base, "test_") ||
		strings.Contains(name, "/tests/") || strings.HasPrefix(name, "tests/") ||
		strings.Contains(name, "__tests__/")
}

var sourceExt = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
	".java": true, ".kt": true, ".rb": true, ".rs": true, ".cs": true, ".php": true,
	".swift": true, ".c": true, ".cc": true, ".cpp": true, ".scala": true,
}

func isSourceFile(name string) bool { return sourceExt[path.Ext(name)] }

// worst returns the highest severity among findings.
func worst(fs []Finding) Severity {
	w := SevInfo
	for _, f := range fs {
		if f.Severity.rank() > w.rank() {
			w = f.Severity
		}
	}
	return w
}

// sortFindings puts blockers first, then by path.
func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Severity.rank() != fs[j].Severity.rank() {
			return fs[i].Severity.rank() > fs[j].Severity.rank()
		}
		return fs[i].Path < fs[j].Path
	})
}

// Risk is a 0-1 estimate of what merging this change unattended could cost.
// It is a policy input, not a verdict: the number decides whether "auto" may
// act or must ask.
func Risk(pr PR, files []PRFile, findings []Finding) float64 {
	r := 0.05
	changed := 0
	for _, f := range files {
		changed += f.Additions + f.Deletions
		lower := strings.ToLower(f.Filename)
		for _, hot := range []string{"migration", "auth", "payment", "billing", "secret", "infra/", "terraform", ".github/workflows", "deploy", "k8s", "helm"} {
			if strings.Contains(lower, hot) {
				r += 0.15
				break
			}
		}
	}
	switch {
	case changed > 1000:
		r += 0.4
	case changed > 300:
		r += 0.25
	case changed > 100:
		r += 0.1
	}
	for _, f := range findings {
		switch f.Severity {
		case SevBlocker:
			r += 0.5
		case SevWarning:
			r += 0.05
		}
	}
	if r > 1 {
		r = 1
	}
	return r
}
