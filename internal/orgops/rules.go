package orgops

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// OrgRules is the org-wide rule text, read once from rules_files. The files
// are the org's own words — the office quotes them to every model it asks, so
// "follow our standards" means the standards the org wrote down, not a model's
// idea of good practice.
func (s *Service) OrgRules() string {
	s.rulesOnce.Do(func() {
		var b strings.Builder
		for _, p := range s.cfg.RulesFiles {
			raw, err := os.ReadFile(expandHome(p))
			if err != nil {
				continue
			}
			fmt.Fprintf(&b, "## %s\n%s\n\n", p, strings.TrimSpace(string(raw)))
		}
		s.orgRules = strings.TrimSpace(b.String())
	})
	return s.orgRules
}

// RepoRules reads a repository's own rule files from ref (default branch when empty).
func (s *Service) RepoRules(ctx context.Context, repo, ref string) string {
	var b strings.Builder
	for _, p := range s.cfg.RepoRulesFiles {
		text, err := s.gh.GetFile(ctx, repo, p, ref)
		if err != nil || strings.TrimSpace(text) == "" {
			continue
		}
		fmt.Fprintf(&b, "## %s/%s\n%s\n\n", repo, p, truncate(strings.TrimSpace(text), 12000))
	}
	return strings.TrimSpace(b.String())
}

// rulesFor is everything a model working in repo must follow.
func (s *Service) rulesFor(ctx context.Context, repo, ref string) string {
	parts := []string{}
	if r := s.OrgRules(); r != "" {
		parts = append(parts, "# Organisation rules\n"+r)
	}
	if r := s.RepoRules(ctx, repo, ref); r != "" {
		parts = append(parts, "# Repository rules\n"+r)
	}
	if len(parts) == 0 {
		return "(no written rules — apply sound engineering judgement)"
	}
	return strings.Join(parts, "\n\n")
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}
