package orgops

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// RepoCard is the knowledge store's entry for one repository.
type RepoCard struct {
	Repo      string    `json:"repo"`
	Card      string    `json:"card"`
	IndexedAt time.Time `json:"indexed_at"`
}

// IndexRepo reads a repository's own documentation and keeps a card of it in
// long-term memory (GitLoom when configured), so "how does billing talk to
// auth?" is answered from the code's own words across every repository.
func (s *Service) IndexRepo(ctx context.Context, k loopkit.Kit, repo string) (RepoCard, error) {
	repo = s.cfg.FullName(repo)
	meta, err := s.gh.GetRepo(ctx, repo)
	if err != nil {
		return RepoCard{}, fmt.Errorf("read %s: %w", repo, err)
	}
	var docs strings.Builder
	for _, p := range s.cfg.Knowledge.Files {
		text, err := s.gh.GetFile(ctx, repo, p, "")
		if err != nil || strings.TrimSpace(text) == "" {
			continue
		}
		if len(text) > s.cfg.Knowledge.MaxBytesPerFile {
			text = text[:s.cfg.Knowledge.MaxBytesPerFile] + "\n[truncated]"
		}
		fmt.Fprintf(&docs, "=== %s ===\n%s\n\n", p, text)
	}
	if docs.Len() == 0 {
		docs.WriteString("(the repository has no documentation files)")
	}
	prompt := fmt.Sprintf(`Write a concise knowledge card for the repository %s (%s) of the %s organisation, from its own documentation below.
Cover, as short bullet points: purpose; tech stack; how to build/run/test; main components and APIs it exposes;
what it depends on (other services, databases, queues); conventions or rules a contributor must follow; owners if stated.
Only state what the documents support. Plain text, under 250 words.

%s`, repo, first(meta.Description, "no description"), s.cfg.Org, docs.String())
	card, err := k.Summarize(ctx, prompt)
	if err != nil || strings.TrimSpace(card) == "" {
		card = "Description: " + first(meta.Description, "none") + "\n" + truncate(docs.String(), 1500)
	}
	rc := RepoCard{Repo: repo, Card: strings.TrimSpace(card), IndexedAt: s.now()}
	_ = s.putJSON(groupKnowledge, repo, rc, 0)
	if err := k.Remember(fmt.Sprintf("[orgops knowledge] Repository %s:\n%s", repo, rc.Card)); err != nil {
		k.Logf("orgops: remember %s: %v", repo, err)
	}
	return rc, nil
}

// IndexAll indexes every live repository in the org.
func (s *Service) IndexAll(ctx context.Context, k loopkit.Kit) (int, error) {
	repos, err := s.gh.ListRepos(ctx, s.cfg.Org)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range repos {
		if r.Archived || s.cfg.PolicyFor(r.FullName).ModeFor(ActKnowledge) == ModeOff {
			continue
		}
		if _, err := s.IndexRepo(ctx, k, r.FullName); err != nil {
			k.Logf("orgops: index %s: %v", r.FullName, err)
			continue
		}
		n++
	}
	return n, nil
}

// Cards returns the knowledge cards, optionally only those mentioning query.
func (s *Service) Cards(query string) []RepoCard {
	all, _ := listJSON[RepoCard](s, groupKnowledge)
	if query == "" {
		return all
	}
	var terms []string
	for _, t := range strings.Fields(strings.ToLower(query)) {
		if len(t) >= 3 {
			terms = append(terms, t)
		}
	}
	var out []RepoCard
	for _, c := range all {
		text := strings.ToLower(c.Repo + " " + c.Card)
		for _, t := range terms {
			if strings.Contains(text, t) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
