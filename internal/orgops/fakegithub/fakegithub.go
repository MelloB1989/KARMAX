// Package fakegithub is an in-memory GitHub: the slice of the REST API the org
// office uses, plus control endpoints that open PRs, set CI results and fire
// the matching signed webhooks at KARMAX. It exists so the whole loop —
// webhook, review, merge, incident fix — can be exercised on a laptop with no
// GitHub org, and so tests can assert on exactly what the office did.
package fakegithub

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// File is a changed file on a fake PR.
type File struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// PR is a fake pull request.
type PR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Author string `json:"author"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	SHA    string `json:"sha"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Merged bool   `json:"merged"`
	Files  []File `json:"files"`
}

// Review is a review the office posted.
type Review struct {
	Repo     string           `json:"repo"`
	Number   int              `json:"number"`
	Event    string           `json:"event"`
	Body     string           `json:"body"`
	Comments []map[string]any `json:"comments"`
}

type repo struct {
	Name, FullName, Description, DefaultBranch string
	Private                                    bool
	Files                                      map[string]string
	Protection                                 map[string]any
}

// Server is the fake. Its exported fields are safe to read after a Lock.
type Server struct {
	mu        sync.Mutex
	repos     map[string]*repo
	prs       map[string]*PR
	next      map[string]int
	ci        map[string]string // sha -> success|failure|pending
	Reviews   []Review
	Comments  []map[string]any
	Merges    []string
	Created   []string
	Webhook   string // where to deliver webhooks (KARMAX /orgops/github)
	Secret    string
	Delivered []string
	base      string
	client    *http.Client
}

// New returns an empty fake. base is the URL it is served at, used in html_url.
func New(base string) *Server {
	return &Server{
		repos: map[string]*repo{}, prs: map[string]*PR{}, next: map[string]int{}, ci: map[string]string{},
		base: strings.TrimRight(base, "/"), client: &http.Client{Timeout: 10 * time.Second},
	}
}

// SetBase sets the URL used in html_url (when it is only known after listening).
func (s *Server) SetBase(b string) { s.mu.Lock(); s.base = strings.TrimRight(b, "/"); s.mu.Unlock() }

// AddRepo seeds a repository with files on its default branch.
func (s *Server) AddRepo(full, description string, files map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if files == nil {
		files = map[string]string{}
	}
	s.repos[full] = &repo{Name: full[strings.LastIndex(full, "/")+1:], FullName: full, Description: description,
		DefaultBranch: "main", Files: files}
}

// OpenPR records a PR and, when a webhook is configured, delivers pull_request.opened.
func (s *Server) OpenPR(p PR) PR {
	s.mu.Lock()
	s.next[p.Repo]++
	p.Number = s.next[p.Repo]
	if p.State == "" {
		p.State = "open"
	}
	if p.Base == "" {
		p.Base = "main"
	}
	if p.Head == "" {
		p.Head = fmt.Sprintf("feature-%d", p.Number)
	}
	if p.SHA == "" {
		p.SHA = fmt.Sprintf("%040x", time.Now().UnixNano()+int64(p.Number))
	}
	for i := range p.Files {
		if p.Files[i].Status == "" {
			p.Files[i].Status = "modified"
		}
		if p.Files[i].Additions == 0 {
			for _, l := range strings.Split(p.Files[i].Patch, "\n") {
				if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
					p.Files[i].Additions++
				}
			}
		}
	}
	cp := p
	s.prs[key(p.Repo, p.Number)] = &cp
	s.mu.Unlock()
	s.deliver("pull_request", s.prEvent("opened", &cp))
	return cp
}

// SetCI sets the CI result on a PR's head and delivers check_suite.completed.
func (s *Server) SetCI(repoName string, number int, state string) {
	s.mu.Lock()
	pr, ok := s.prs[key(repoName, number)]
	if !ok {
		s.mu.Unlock()
		return
	}
	s.ci[pr.SHA] = state
	body := map[string]any{
		"action":     "completed",
		"repository": map[string]any{"full_name": repoName},
		"sender":     map[string]any{"login": "ci-bot"},
		"check_suite": map[string]any{
			"head_branch": pr.Head, "head_sha": pr.SHA, "conclusion": state,
			"app":           map[string]any{"name": "fake-ci"},
			"pull_requests": []any{map[string]any{"number": number}},
		},
	}
	s.mu.Unlock()
	if state != "pending" {
		s.deliver("check_suite", body)
	}
}

// PR returns a copy of a PR.
func (s *Server) PR(repoName string, n int) (PR, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prs[key(repoName, n)]
	if !ok {
		return PR{}, false
	}
	return *p, true
}

// File returns a file's content on a repo's default branch.
func (s *Server) File(repoName, path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoName]
	if !ok {
		return "", false
	}
	c, ok := r.Files[path]
	return c, ok
}

// Snapshot is everything the office did, for assertions and the /_fake/state endpoint.
func (s *Server) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	prs := make([]PR, 0, len(s.prs))
	for _, p := range s.prs {
		cp := *p
		cp.Files = nil
		prs = append(prs, cp)
	}
	sort.Slice(prs, func(i, j int) bool {
		return prs[i].Repo+strconv.Itoa(prs[i].Number) < prs[j].Repo+strconv.Itoa(prs[j].Number)
	})
	repos := make([]string, 0, len(s.repos))
	for k := range s.repos {
		repos = append(repos, k)
	}
	sort.Strings(repos)
	return map[string]any{"repos": repos, "prs": prs, "reviews": s.Reviews, "comments": s.Comments,
		"merges": s.Merges, "created": s.Created, "delivered": s.Delivered}
}

func key(r string, n int) string { return fmt.Sprintf("%s#%d", r, n) }

func (s *Server) prJSON(p *PR) map[string]any {
	add, del := 0, 0
	for _, f := range p.Files {
		add += f.Additions
		del += f.Deletions
	}
	return map[string]any{
		"number": p.Number, "title": p.Title, "body": p.Body, "state": p.State, "draft": p.Draft,
		"merged": p.Merged, "html_url": fmt.Sprintf("%s/%s/pull/%d", s.base, p.Repo, p.Number),
		"additions": add, "deletions": del, "changed_files": len(p.Files),
		"user": map[string]any{"login": p.Author},
		"head": map[string]any{"ref": p.Head, "sha": p.SHA},
		"base": map[string]any{"ref": p.Base},
	}
}

func (s *Server) prEvent(action string, p *PR) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"action": action, "repository": map[string]any{"full_name": p.Repo},
		"sender": map[string]any{"login": p.Author}, "pull_request": s.prJSON(p),
	}
}

func (s *Server) deliver(event string, body map[string]any) {
	s.mu.Lock()
	url, secret := s.Webhook, s.Secret
	s.mu.Unlock()
	if url == "" {
		return
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := s.client.Do(req)
	status := "error: "
	if err == nil {
		resp.Body.Close()
		status = resp.Status
	} else {
		status += err.Error()
	}
	s.mu.Lock()
	s.Delivered = append(s.Delivered, event+" -> "+status)
	s.mu.Unlock()
}

// ServeHTTP implements the API.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	seg := strings.Split(p, "/")
	if strings.HasPrefix(p, "_fake/") {
		s.control(w, r, seg[1:])
		return
	}
	switch {
	case len(seg) == 3 && seg[0] == "orgs" && seg[2] == "repos" && r.Method == "GET",
		len(seg) == 3 && seg[0] == "users" && seg[2] == "repos" && r.Method == "GET":
		s.listRepos(w, seg[1])
	case len(seg) == 3 && seg[0] == "orgs" && seg[2] == "repos" && r.Method == "POST":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.createRepo(w, seg[1], in, "")
	case len(seg) >= 3 && seg[0] == "repos":
		s.repoAPI(w, r, seg[1]+"/"+seg[2], seg[3:])
	default:
		writeErr(w, 404)
	}
}

func (s *Server) repoAPI(w http.ResponseWriter, r *http.Request, full string, rest []string) {
	s.mu.Lock()
	rp, ok := s.repos[full]
	s.mu.Unlock()
	if len(rest) == 0 {
		if !ok {
			writeErr(w, 404)
			return
		}
		writeJSON(w, 200, s.repoJSON(rp))
		return
	}
	switch {
	case rest[0] == "generate" && r.Method == "POST":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		owner, _ := in["owner"].(string)
		s.createRepo(w, owner, in, full)
	case rest[0] == "pulls" && len(rest) == 1 && r.Method == "GET":
		head := r.URL.Query().Get("head")
		if i := strings.Index(head, ":"); i >= 0 {
			head = head[i+1:]
		}
		s.mu.Lock()
		out := []map[string]any{}
		for _, p := range s.prs {
			if p.Repo == full && p.State == "open" && (head == "" || p.Head == head) {
				out = append(out, s.prJSON(p))
			}
		}
		s.mu.Unlock()
		writeJSON(w, 200, out)
	case rest[0] == "pulls" && len(rest) == 1 && r.Method == "POST":
		var in struct{ Title, Body, Head, Base string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		pr := s.OpenPR(PR{Repo: full, Title: in.Title, Body: in.Body, Head: in.Head, Base: in.Base, Author: "karmax-bot"})
		s.mu.Lock()
		out := s.prJSON(s.prs[key(full, pr.Number)])
		s.mu.Unlock()
		writeJSON(w, 201, out)
	case rest[0] == "pulls" && len(rest) >= 2:
		n, _ := strconv.Atoi(rest[1])
		s.mu.Lock()
		pr, found := s.prs[key(full, n)]
		s.mu.Unlock()
		if !found {
			writeErr(w, 404)
			return
		}
		s.pullAPI(w, r, pr, rest[2:])
	case rest[0] == "issues" && len(rest) == 3 && rest[2] == "comments" && r.Method == "POST":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		in["repo"], in["number"] = full, rest[1]
		s.mu.Lock()
		s.Comments = append(s.Comments, in)
		s.mu.Unlock()
		writeJSON(w, 201, map[string]any{"id": len(s.Comments)})
	case rest[0] == "commits" && len(rest) == 3:
		s.mu.Lock()
		st := s.ci[rest[1]]
		s.mu.Unlock()
		if rest[2] == "status" {
			statuses := []any{}
			if st != "" {
				statuses = append(statuses, map[string]any{"state": st, "context": "fake-ci"})
			}
			writeJSON(w, 200, map[string]any{"state": map[bool]string{true: st, false: "pending"}[st != ""], "statuses": statuses})
			return
		}
		writeJSON(w, 200, map[string]any{"check_runs": []any{}})
	case rest[0] == "contents" && len(rest) >= 2:
		if !ok {
			writeErr(w, 404)
			return
		}
		path := strings.Join(rest[1:], "/")
		if r.Method == "PUT" {
			var in struct{ Content, Message string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			b, _ := base64.StdEncoding.DecodeString(in.Content)
			s.mu.Lock()
			rp.Files[path] = string(b)
			s.mu.Unlock()
			writeJSON(w, 201, map[string]any{"content": map[string]any{"path": path}})
			return
		}
		s.mu.Lock()
		c, has := rp.Files[path]
		s.mu.Unlock()
		if !has {
			writeErr(w, 404)
			return
		}
		writeJSON(w, 200, map[string]any{"type": "file", "encoding": "base64", "sha": fmt.Sprintf("%x", sha256.Sum256([]byte(c)))[:40],
			"content": base64.StdEncoding.EncodeToString([]byte(c))})
	case rest[0] == "branches" && len(rest) == 3 && rest[2] == "protection" && r.Method == "PUT":
		if !ok {
			writeErr(w, 404)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		rp.Protection = in
		s.mu.Unlock()
		writeJSON(w, 200, in)
	default:
		writeErr(w, 404)
	}
}

func (s *Server) pullAPI(w http.ResponseWriter, r *http.Request, pr *PR, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == "GET":
		s.mu.Lock()
		out := s.prJSON(pr)
		s.mu.Unlock()
		writeJSON(w, 200, out)
	case len(rest) == 1 && rest[0] == "files":
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page > 1 {
			writeJSON(w, 200, []File{})
			return
		}
		s.mu.Lock()
		files := append([]File{}, pr.Files...)
		s.mu.Unlock()
		writeJSON(w, 200, files)
	case len(rest) == 1 && rest[0] == "reviews" && r.Method == "POST":
		var in struct {
			Event    string           `json:"event"`
			Body     string           `json:"body"`
			Comments []map[string]any `json:"comments"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		s.Reviews = append(s.Reviews, Review{Repo: pr.Repo, Number: pr.Number, Event: in.Event, Body: in.Body, Comments: in.Comments})
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"id": len(s.Reviews), "state": in.Event})
	case len(rest) == 1 && rest[0] == "merge" && r.Method == "PUT":
		s.mu.Lock()
		if pr.Merged || pr.State != "open" {
			s.mu.Unlock()
			writeJSON(w, 405, map[string]any{"message": "Pull Request is not mergeable"})
			return
		}
		pr.Merged, pr.State = true, "closed"
		s.Merges = append(s.Merges, key(pr.Repo, pr.Number))
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"merged": true, "sha": pr.SHA})
		ev := s.prEvent("closed", pr)
		go s.deliver("pull_request", ev)
	default:
		writeErr(w, 404)
	}
}

func (s *Server) listRepos(w http.ResponseWriter, owner string) {
	s.mu.Lock()
	out := []map[string]any{}
	names := make([]string, 0, len(s.repos))
	for k := range s.repos {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if strings.HasPrefix(k, owner+"/") {
			out = append(out, s.repoJSON(s.repos[k]))
		}
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *Server) createRepo(w http.ResponseWriter, owner string, in map[string]any, template string) {
	name, _ := in["name"].(string)
	desc, _ := in["description"].(string)
	priv, _ := in["private"].(bool)
	full := owner + "/" + name
	s.mu.Lock()
	if _, exists := s.repos[full]; exists {
		s.mu.Unlock()
		writeJSON(w, 422, map[string]any{"message": "name already exists on this account"})
		return
	}
	files := map[string]string{"README.md": "# " + name + "\n\n" + desc + "\n"}
	if t, ok := s.repos[template]; ok {
		files = map[string]string{}
		for k, v := range t.Files {
			files[k] = v
		}
	}
	rp := &repo{Name: name, FullName: full, Description: desc, DefaultBranch: "main", Private: priv, Files: files}
	s.repos[full] = rp
	s.Created = append(s.Created, full)
	out := s.repoJSON(rp)
	s.mu.Unlock()
	writeJSON(w, 201, out)
}

func (s *Server) repoJSON(r *repo) map[string]any {
	return map[string]any{"name": r.Name, "full_name": r.FullName, "description": r.Description,
		"default_branch": r.DefaultBranch, "private": r.Private, "html_url": s.base + "/" + r.FullName}
}

// control is the test/demo surface: /_fake/repos, /_fake/pulls, /_fake/ci, /_fake/state.
func (s *Server) control(w http.ResponseWriter, r *http.Request, seg []string) {
	if len(seg) == 0 {
		writeErr(w, 404)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	switch seg[0] {
	case "state":
		writeJSON(w, 200, s.Snapshot())
	case "repos":
		var in struct {
			Repo, Description string
			Files             map[string]string
		}
		if json.Unmarshal(body, &in) != nil || in.Repo == "" {
			writeJSON(w, 400, map[string]any{"message": `want {"repo":"org/name","files":{...}}`})
			return
		}
		s.AddRepo(in.Repo, in.Description, in.Files)
		writeJSON(w, 201, map[string]any{"repo": in.Repo})
	case "pulls":
		var in PR
		if json.Unmarshal(body, &in) != nil || in.Repo == "" {
			writeJSON(w, 400, map[string]any{"message": `want {"repo","title","body","author","files":[{"filename","patch"}]}`})
			return
		}
		writeJSON(w, 201, s.OpenPR(in))
	case "ci":
		var in struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
			State  string `json:"state"`
		}
		if json.Unmarshal(body, &in) != nil || in.Repo == "" {
			writeJSON(w, 400, map[string]any{"message": `want {"repo","number","state":"success|failure|pending"}`})
			return
		}
		s.SetCI(in.Repo, in.Number, in.State)
		writeJSON(w, 200, map[string]any{"ok": true})
	case "webhook":
		var in struct{ URL, Secret string }
		_ = json.Unmarshal(body, &in)
		s.mu.Lock()
		s.Webhook, s.Secret = in.URL, in.Secret
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeErr(w, 404)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int) {
	writeJSON(w, code, map[string]any{"message": http.StatusText(code)})
}
