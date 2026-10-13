package orgops

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// RepoRequest is a request for a new repository.
type RepoRequest struct {
	Name        string   `json:"name"`
	Template    string   `json:"template"`
	Description string   `json:"description"`
	Owners      []string `json:"owners"`
	Requester   string   `json:"requester"`
	Channel     string   `json:"channel"`
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// RequestRepo applies the repo_create policy to a request.
func (s *Service) RequestRepo(ctx context.Context, k loopkit.Kit, r RepoRequest) error {
	name := shortName(strings.TrimSpace(r.Name))
	if !repoName.MatchString(name) {
		return fmt.Errorf("%q is not a valid repository name", r.Name)
	}
	r.Name = name
	full := s.cfg.FullName(name)
	if r.Channel == "" {
		r.Channel = s.cfg.ChannelFor("approval", full)
	}
	switch s.cfg.PolicyFor(full).Decide(ActRepoCreate, 0) {
	case ModeOff:
		return fmt.Errorf("repository creation is turned off for %s", full)
	case ModeNotify:
		s.say(ctx, k, "", r.Channel, fmt.Sprintf("ℹ️ %s asked for a new repository %s (policy: notify only).", first(r.Requester, "someone"), full))
		return nil
	case ModeApprove:
		_, err := s.ask(ctx, k, Pending{Action: ActRepoCreate, Title: "create repository " + full, Repo: full, Channel: r.Channel,
			Args: map[string]any{"name": r.Name, "template": r.Template, "description": r.Description,
				"owners": strings.Join(r.Owners, ","), "requester": r.Requester}},
			fmt.Sprintf("Template: %s. It will be created with the org's standard files and branch protection.", first(s.templateFor(r.Template), "none")))
		return err
	}
	_, err := s.CreateRepo(ctx, k, r)
	return err
}

func (s *Service) templateFor(name string) string {
	if name == "" {
		name = s.cfg.Factory.DefaultTemplate
	}
	if t, ok := s.cfg.Factory.Templates[name]; ok {
		return t
	}
	return name
}

// CreateRepo makes the repository, writes the org's standard files, protects
// the default branch and indexes it — a new repo starts out compliant instead
// of being chased into compliance later.
func (s *Service) CreateRepo(ctx context.Context, k loopkit.Kit, r RepoRequest) (Repo, error) {
	full := s.cfg.FullName(r.Name)
	tpl := s.templateFor(r.Template)
	if tpl != "" {
		tpl = s.cfg.FullName(tpl)
	}
	repo, err := s.gh.CreateRepo(ctx, s.cfg.Org, r.Name, r.Description, tpl, !s.cfg.Factory.Public)
	if err != nil {
		s.say(ctx, k, "", r.Channel, fmt.Sprintf("❌ Could not create %s: %v", full, err))
		return repo, err
	}
	if repo.FullName == "" {
		repo.FullName = full
	}
	branch := first(repo.DefaultBranch, "main")
	// A repo generated from a template is populated asynchronously; give it a
	// moment so the first write does not race GitHub's own.
	if tpl != "" {
		time.Sleep(2 * time.Second)
	}

	files := s.bootstrapFiles(r, repo.FullName)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var wrote []string
	for _, p := range paths {
		if err := s.gh.PutFile(ctx, repo.FullName, p, files[p], "chore: add org standard "+p, branch); err != nil {
			k.Logf("orgops: bootstrap %s/%s: %v", repo.FullName, p, err)
			continue
		}
		wrote = append(wrote, p)
	}
	protected := false
	if s.cfg.Factory.ProtectDefaultBranch {
		if err := s.gh.ProtectBranch(ctx, repo.FullName, branch, s.cfg.Factory.RequiredChecks); err != nil {
			k.Logf("orgops: protect %s: %v", repo.FullName, err)
		} else {
			protected = true
		}
	}
	_ = k.Audit("github.repo.create", repo.FullName, "created", first(tpl, "blank"))
	msg := fmt.Sprintf("📦 Created *%s*%s\n%s", repo.FullName, map[bool]string{true: " from " + tpl, false: ""}[tpl != ""], repo.HTMLURL)
	if len(wrote) > 0 {
		msg += "\nStandard files: " + strings.Join(wrote, ", ")
	}
	if protected {
		msg += "\nBranch protection on " + branch + ": 1 review" + map[bool]string{true: " + checks " + strings.Join(s.cfg.Factory.RequiredChecks, ", "), false: ""}[len(s.cfg.Factory.RequiredChecks) > 0]
	}
	if m := mention(r.Owners); m != "" {
		msg += "\nOwners: " + m
	}
	s.say(ctx, k, "", r.Channel, msg)
	if s.cfg.PolicyFor(repo.FullName).ModeFor(ActKnowledge) != ModeOff {
		_ = s.publish(EventIndex, map[string]any{"repo": repo.FullName})
	}
	return repo, nil
}

func (s *Service) bootstrapFiles(r RepoRequest, full string) map[string]string {
	out := map[string]string{}
	owners := r.Owners
	if len(owners) == 0 {
		owners = s.cfg.OwnersFor(full)
	}
	repl := strings.NewReplacer("{{repo}}", full, "{{name}}", r.Name, "{{org}}", s.cfg.Org,
		"{{owners}}", strings.Join(withAt(owners), " "), "{{description}}", r.Description)
	for p, content := range s.cfg.Factory.Bootstrap {
		out[p] = repl.Replace(content)
	}
	if _, ok := out["CODEOWNERS"]; !ok && len(owners) > 0 {
		if _, ok := out[".github/CODEOWNERS"]; !ok {
			out[".github/CODEOWNERS"] = "* " + strings.Join(withAt(owners), " ") + "\n"
		}
	}
	// The org's own rules travel with every repository, so a coding agent
	// opening it reads them without being told where they are.
	if rules := s.OrgRules(); rules != "" {
		if _, ok := out["CLAUDE.md"]; !ok {
			out["CLAUDE.md"] = "# " + full + "\n\n" + first(r.Description, "") + "\n\n" + rules + "\n"
		}
	}
	return out
}

func withAt(owners []string) []string {
	out := make([]string, 0, len(owners))
	for _, o := range owners {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if !strings.HasPrefix(o, "@") {
			o = "@" + o
		}
		out = append(out, o)
	}
	return out
}

// AuditRepos checks every repository against the required files and reports
// the ones that fall short — the standardisation sweep.
func (s *Service) AuditRepos(ctx context.Context) (map[string][]Finding, error) {
	repos, err := s.gh.ListRepos(ctx, s.cfg.Org)
	if err != nil {
		return nil, err
	}
	out := map[string][]Finding{}
	for _, r := range repos {
		if r.Archived {
			continue
		}
		fs := MissingFiles(s.cfg.Standards, func(p string) bool {
			_, err := s.gh.GetFile(ctx, r.FullName, p, "")
			return err == nil
		})
		if len(fs) > 0 {
			out[r.FullName] = fs
		}
	}
	return out, nil
}
