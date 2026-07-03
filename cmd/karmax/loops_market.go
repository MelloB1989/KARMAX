package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/spf13/cobra"
)

// The loops MARKETPLACE: a public GitHub registry repo holds one directory per
// loop (loops/<name>/loop.json + optionally the loop's Go code), and a static
// site (GitHub Pages, /docs in the same repo) renders it. These commands are
// the author/consumer toolkit: scaffold a loop, browse/install from the
// registry, and publish (direct commit with write access, else fork + PR via
// the gh CLI). The registry location is env-overridable — never hardcoded
// beyond a default.

// defaultLoopsRegistry is the default public registry repo (owner/repo).
const defaultLoopsRegistry = "MelloB1989/karmax-loops"

// loopsRegistry returns the registry repo as "owner/repo"
// ($KARMAX_LOOPS_REGISTRY overrides).
func loopsRegistry() string {
	if v := strings.TrimSpace(os.Getenv("KARMAX_LOOPS_REGISTRY")); v != "" {
		return v
	}
	return defaultLoopsRegistry
}

// loopManifest is loops/<name>/loop.json in the registry.
type loopManifest struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Version     string           `json:"version"`
	Author      string           `json:"author"`
	Module      string           `json:"module,omitempty"`  // Go module holding the code; empty = the registry repo itself
	Package     string           `json:"package,omitempty"` // import path; defaults to module, or <registry>/loops/<name> when hosted in the registry
	Repo        string           `json:"repo,omitempty"`    // human URL to the code
	Tags        []string         `json:"tags,omitempty"`
	Schedule    string           `json:"schedule,omitempty"` // informational: cron/interval the loop registers
	Config      []loopConfigItem `json:"config,omitempty"`   // install-time env keys (KARMAX_LOOP_<NAME>_<KEY>)
}

type loopConfigItem struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

// resolveInstall computes the go-get module and blank-import package for a
// manifest (registry-hosted loops live under the registry module).
func (m loopManifest) resolveInstall() (module, pkg string) {
	module = strings.TrimSpace(m.Module)
	pkg = strings.TrimSpace(m.Package)
	if module == "" {
		module = "github.com/" + loopsRegistry()
		if pkg == "" {
			pkg = module + "/loops/" + m.Name
		}
	}
	if pkg == "" {
		pkg = module
	}
	return module, pkg
}

var loopNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func (m loopManifest) validate() error {
	switch {
	case !loopNameRe.MatchString(m.Name):
		return fmt.Errorf("name must be kebab-case (got %q)", m.Name)
	case strings.TrimSpace(m.Description) == "":
		return fmt.Errorf("description is required")
	case strings.TrimSpace(m.Version) == "":
		return fmt.Errorf("version is required (e.g. 0.1.0)")
	case strings.TrimSpace(m.Author) == "":
		return fmt.Errorf("author is required")
	}
	return nil
}

// ---- registry fetching (public, unauthenticated) ---------------------------

func registryHTTP(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return body, nil
}

// fetchRegistryManifests lists loops/ in the registry and loads each manifest.
func fetchRegistryManifests() ([]loopManifest, error) {
	reg := loopsRegistry()
	body, err := registryHTTP("https://api.github.com/repos/" + reg + "/contents/loops")
	if err != nil {
		return nil, fmt.Errorf("list registry %s: %w", reg, err)
	}
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("parse registry listing: %w", err)
	}
	var out []loopManifest
	for _, e := range entries {
		if e.Type != "dir" {
			continue
		}
		m, err := fetchRegistryManifest(e.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", e.Name, err)
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func fetchRegistryManifest(name string) (loopManifest, error) {
	var m loopManifest
	body, err := registryHTTP("https://raw.githubusercontent.com/" + loopsRegistry() + "/main/loops/" + name + "/loop.json")
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("parse loop.json: %w", err)
	}
	return m, nil
}

// ---- commands ---------------------------------------------------------------

func newLoopsBrowseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "browse [query]",
		Short: "Browse the public loops marketplace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manifests, err := fetchRegistryManifests()
			if err != nil {
				return err
			}
			query := ""
			if len(args) == 1 {
				query = strings.ToLower(args[0])
			}
			shown := 0
			for _, m := range manifests {
				if query != "" && !strings.Contains(strings.ToLower(m.Name+" "+m.Description+" "+strings.Join(m.Tags, " ")), query) {
					continue
				}
				shown++
				tags := ""
				if len(m.Tags) > 0 {
					tags = "  [" + strings.Join(m.Tags, ", ") + "]"
				}
				fmt.Printf("• %-18s v%-8s by %-14s%s\n    %s\n", m.Name, m.Version, m.Author, tags, oneLine(m.Description, 120))
			}
			if shown == 0 {
				fmt.Println("no loops matched.")
				return nil
			}
			owner, _, _ := strings.Cut(loopsRegistry(), "/")
			repo := loopsRegistry()[len(owner)+1:]
			fmt.Printf("\n%d loop(s). Install with: karmax loops install <name>   |   marketplace: https://%s.github.io/%s/\n", shown, owner, repo)
			return nil
		},
	}
}

func newLoopsInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>",
		Short: "Show a marketplace loop's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			m, err := fetchRegistryManifest(args[0])
			if err != nil {
				return err
			}
			module, pkg := m.resolveInstall()
			fmt.Printf("%s v%s — by %s\n\n%s\n\n", m.Name, m.Version, m.Author, m.Description)
			if m.Schedule != "" {
				fmt.Printf("schedule: %s\n", m.Schedule)
			}
			if len(m.Tags) > 0 {
				fmt.Printf("tags:     %s\n", strings.Join(m.Tags, ", "))
			}
			fmt.Printf("module:   %s\npackage:  %s\n", module, pkg)
			if m.Repo != "" {
				fmt.Printf("code:     %s\n", m.Repo)
			}
			for _, c := range m.Config {
				fmt.Printf("config:   KARMAX_LOOP_%s_%s — %s\n", envSanitizeCLI(m.Name), envSanitizeCLI(c.Key), c.Description)
			}
			fmt.Printf("\ninstall:  karmax loops install %s\n", m.Name)
			return nil
		},
	}
}

func newLoopsInstallCmd() *cobra.Command {
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "install <name|module>",
		Short: "Install a loop from the marketplace (by name) or any Go module path",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ref := strings.TrimSpace(args[0])
			module, pkg := ref, ref
			if !strings.Contains(ref, "/") {
				m, err := fetchRegistryManifest(ref)
				if err != nil {
					return fmt.Errorf("loop %q not found in registry %s: %w", ref, loopsRegistry(), err)
				}
				module, pkg = m.resolveInstall()
				fmt.Printf("installing %s v%s (%s)\n", m.Name, m.Version, pkg)
			}
			root, err := loopinstall.RepoRoot()
			if err != nil {
				return err
			}
			log, err := loopinstall.InstallWithPackage(root, module, pkg)
			if err != nil {
				fmt.Print(log)
				return err
			}
			fmt.Println("installed and rebuilt.")
			if noRestart {
				fmt.Println("restart karmax to activate: systemctl --user restart karmax.service")
				return nil
			}
			if out, err := loopinstall.Restart(); err != nil {
				fmt.Printf("restart failed (%v): %s\nrestart manually to activate.\n", err, out)
				return nil
			}
			fmt.Println("karmax restarted — the loop is live (see `karmax loops list`).")
			return nil
		},
	}
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "skip restarting the karmax service")
	return cmd
}

func newLoopsRemoveCmd() *cobra.Command {
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "remove <name|module>",
		Short: "Remove an installed marketplace loop (drops the import and rebuilds)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ref := strings.TrimSpace(args[0])
			pkg := ref
			if !strings.Contains(ref, "/") {
				if m, err := fetchRegistryManifest(ref); err == nil {
					_, pkg = m.resolveInstall()
				}
			}
			root, err := loopinstall.RepoRoot()
			if err != nil {
				return err
			}
			installed, _ := loopinstall.InstalledModules(root)
			found := ""
			for _, mod := range installed {
				if mod == pkg || mod == ref || strings.HasSuffix(mod, "/"+ref) {
					found = mod
					break
				}
			}
			if found == "" {
				return fmt.Errorf("%q is not installed (see `karmax loops list`)", ref)
			}
			log, err := loopinstall.Remove(root, found)
			if err != nil {
				fmt.Print(log)
				return err
			}
			fmt.Printf("removed %s and rebuilt.\n", found)
			if noRestart {
				fmt.Println("restart karmax to apply: systemctl --user restart karmax.service")
				return nil
			}
			if out, err := loopinstall.Restart(); err != nil {
				fmt.Printf("restart failed (%v): %s\n", err, out)
				return nil
			}
			fmt.Println("karmax restarted.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "skip restarting the karmax service")
	return cmd
}

func newLoopsNewCmd() *cobra.Command {
	var module, author string
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Scaffold a new loop (boilerplate + manifest), ready to publish",
		Long: "Scaffold a new loop in ./<name>/.\n" +
			"By default the loop is REGISTRY-HOSTED: its code is published into the marketplace repo itself (no repo of your own needed).\n" +
			"Pass --module github.com/you/karmax-<name> to make it a STANDALONE module living in your own repo (adds a go.mod); then only the manifest goes to the registry.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if !loopNameRe.MatchString(name) {
				return fmt.Errorf("loop name must be kebab-case, e.g. my-loop")
			}
			if author == "" {
				if out, err := exec.Command("gh", "api", "user", "-q", ".login").Output(); err == nil {
					author = strings.TrimSpace(string(out))
				}
			}
			if author == "" {
				author = os.Getenv("USER")
			}
			dir := name
			if err := os.MkdirAll(dir, 0755); err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(dir, "loop.go")); err == nil {
				return fmt.Errorf("%s/loop.go already exists", dir)
			}

			pkgName := strings.ReplaceAll(name, "-", "")
			m := loopManifest{
				Name: name, Description: "TODO: one line shown in the marketplace.",
				Version: "0.1.0", Author: author, Module: module,
			}
			if module != "" {
				m.Package = module
				if strings.HasPrefix(module, "github.com/") {
					m.Repo = "https://" + module
				}
			}
			manifest, _ := json.MarshalIndent(m, "", "  ")
			files := map[string]string{
				"loop.go":   loopBoilerplate(pkgName, name),
				"loop.json": string(manifest) + "\n",
				"README.md": "# " + name + "\n\nA [KARMAX](https://github.com/MelloB1989/KARMAX) loop.\n\nTODO: describe what it does and any `KARMAX_LOOP_" + envSanitizeCLI(name) + "_*` config keys.\n",
			}
			if module != "" {
				files["go.mod"] = "module " + module + "\n\ngo 1.24\n"
				files[".gitignore"] = "*.test\n"
			}
			for f, content := range files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0644); err != nil {
					return err
				}
			}

			fmt.Printf("scaffolded ./%s/\n\nnext steps:\n", dir)
			fmt.Println("  1. implement Run in loop.go; fill in description (loop.json + Description in loop.go)")
			if module != "" {
				fmt.Printf("  2. cd %s && go get github.com/MelloB1989/karmax@main && go build ./...\n", dir)
				fmt.Printf("  3. push the module to %s\n", module)
				fmt.Printf("  4. karmax loops publish %s   (submits the manifest to the marketplace)\n", dir)
			} else {
				fmt.Printf("  2. karmax loops publish %s   (validates, then submits code + manifest to the marketplace)\n", dir)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&module, "module", "", "standalone Go module path (your own repo); omit to host the code in the registry")
	cmd.Flags().StringVar(&author, "author", "", "author name (default: your gh login)")
	return cmd
}

func loopBoilerplate(pkgName, name string) string {
	return `// Package ` + pkgName + ` is a KARMAX loop.
package ` + pkgName + `

import (
	"context"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func init() {
	loopkit.Register(loopkit.Loop{
		Name:        "` + name + `",
		Description: "TODO: one line shown in the marketplace and installer.",
		Schedule:    loopkit.Every("6h"), // or loopkit.Cron("0 0 9 * * *"); remove for webhook/manual-only
		// Webhook: "/hooks/` + name + `", // optional: also fire when this route is hit
		Run: run,
	})
}

func run(ctx context.Context, k loopkit.Kit) error {
	// Kit is your capability surface: k.Ask (main agent), k.Harness (Claude Code),
	// k.Remember/k.Recall (memory), k.Notify (app push), k.Propose (approvals inbox),
	// k.Remind (phone reminder), k.SendWhatsApp/k.ReadWhatsApp, k.HTTP,
	// k.Config (install-time env), k.RunLoop, k.Trigger, k.Logf.
	k.Logf("` + name + `: running (trigger: %s)", k.Trigger().Kind)

	// TODO: implement. Example:
	// body, _, err := k.HTTP(ctx, "GET", "https://example.com/api", nil, "")
	// if err != nil {
	// 	return err
	// }
	// return k.Notify("` + name + `", body)
	return nil
}
`
}

func newLoopsPublishCmd() *cobra.Command {
	var skipVerify bool
	cmd := &cobra.Command{
		Use:   "publish [dir]",
		Short: "Publish a loop to the public marketplace (direct commit or fork+PR via gh)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			raw, err := os.ReadFile(filepath.Join(dir, "loop.json"))
			if err != nil {
				return fmt.Errorf("no loop.json in %s (scaffold one with `karmax loops new`): %w", dir, err)
			}
			var m loopManifest
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("parse loop.json: %w", err)
			}
			if err := m.validate(); err != nil {
				return err
			}
			if strings.Contains(m.Description, "TODO") {
				return fmt.Errorf("loop.json description is still the TODO placeholder")
			}
			registryHosted := strings.TrimSpace(m.Module) == ""

			// Files that go to the registry: the manifest, plus the code for
			// registry-hosted loops.
			payload := map[string][]byte{"loops/" + m.Name + "/loop.json": raw}
			if registryHosted {
				entries, err := os.ReadDir(dir)
				if err != nil {
					return err
				}
				for _, e := range entries {
					n := e.Name()
					if e.IsDir() || n == "loop.json" || (!strings.HasSuffix(n, ".go") && n != "README.md") {
						continue
					}
					if strings.HasSuffix(n, "_test.go") {
						continue
					}
					b, err := os.ReadFile(filepath.Join(dir, n))
					if err != nil {
						return err
					}
					payload["loops/"+m.Name+"/"+n] = b
				}
				if !skipVerify {
					if err := verifyLoopCompiles(dir, m.Name); err != nil {
						return fmt.Errorf("loop does not compile (fix it or pass --no-verify): %w", err)
					}
					fmt.Println("✓ compiles against the local KARMAX source")
				}
			}

			if _, err := exec.LookPath("gh"); err != nil {
				return fmt.Errorf("publishing needs the GitHub CLI (gh) installed and authenticated")
			}
			url, err := publishToRegistry(m, payload)
			if err != nil {
				return err
			}
			fmt.Printf("published: %s\n", url)
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipVerify, "no-verify", false, "skip the local compile check")
	return cmd
}

// verifyLoopCompiles copies a registry-hosted loop into the local KARMAX
// checkout (as a throwaway internal package) and builds it, so broken code
// never reaches the marketplace.
func verifyLoopCompiles(dir, name string) error {
	root, err := loopinstall.RepoRoot()
	if err != nil {
		return fmt.Errorf("no local KARMAX source to verify against (%w)", err)
	}
	tmp := filepath.Join(root, "internal", ".loopverify", name)
	if err := os.MkdirAll(tmp, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Join(root, "internal", ".loopverify"))
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(tmp, e.Name()), b, 0644); err != nil {
			return err
		}
	}
	cmd := exec.Command(loopinstall.GoBin(), "build", "./internal/.loopverify/...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// publishToRegistry lands the payload files in the registry repo: a direct
// commit to main when the gh user has push access, otherwise fork + branch +
// pull request. Returns the commit/PR URL.
func publishToRegistry(m loopManifest, payload map[string][]byte) (string, error) {
	reg := loopsRegistry()
	login, err := ghQ("api", "user", "-q", ".login")
	if err != nil {
		return "", fmt.Errorf("gh not authenticated: %w", err)
	}

	canPush := false
	if v, err := ghQ("api", "repos/"+reg, "-q", ".permissions.push"); err == nil && v == "true" {
		canPush = true
	}

	target, branch := reg, "main"
	if !canPush {
		// Fork (idempotent), then stage the change on a branch of the fork.
		if _, err := ghQ("repo", "fork", reg, "--clone=false"); err != nil {
			return "", fmt.Errorf("fork %s: %w", reg, err)
		}
		_, repoName, _ := strings.Cut(reg, "/")
		target = login + "/" + repoName
		branch = "loop-" + m.Name + "-" + m.Version
		// Branch from the fork's current main (retry briefly — forking is async).
		var sha string
		for i := 0; i < 10; i++ {
			if sha, err = ghQ("api", "repos/"+target+"/git/ref/heads/main", "-q", ".object.sha"); err == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if sha == "" {
			return "", fmt.Errorf("fork %s not ready: %w", target, err)
		}
		if _, err := ghQ("api", "-X", "POST", "repos/"+target+"/git/refs", "-f", "ref=refs/heads/"+branch, "-f", "sha="+sha); err != nil && !strings.Contains(err.Error(), "already exists") {
			return "", fmt.Errorf("create branch: %w", err)
		}
	}

	message := "loop: " + m.Name + " v" + m.Version
	for path, content := range payload {
		if err := ghPutFile(target, branch, path, message, content); err != nil {
			return "", fmt.Errorf("write %s: %w", path, err)
		}
	}

	if canPush {
		return "https://github.com/" + reg + "/tree/main/loops/" + m.Name, nil
	}
	prURL, err := ghQ("pr", "create", "--repo", reg, "--head", login+":"+branch,
		"--title", "Add loop: "+m.Name+" v"+m.Version,
		"--body", m.Description+"\n\nSubmitted with `karmax loops publish` by @"+login+".")
	if err != nil {
		return "", fmt.Errorf("create PR (branch %s is pushed on your fork — you can open it manually): %w", branch, err)
	}
	return prURL, nil
}

// ghPutFile creates/updates one file via the GitHub contents API (fetching the
// existing blob sha when the file already exists, e.g. a version bump).
func ghPutFile(repo, branch, path, message string, content []byte) error {
	args := []string{"api", "-X", "PUT", "repos/" + repo + "/contents/" + path,
		"-f", "message=" + message,
		"-f", "content=" + base64.StdEncoding.EncodeToString(content),
		"-f", "branch=" + branch,
	}
	if sha, err := ghQ("api", "repos/"+repo+"/contents/"+path+"?ref="+branch, "-q", ".sha"); err == nil && sha != "" {
		args = append(args, "-f", "sha="+sha)
	}
	_, err := ghQ(args...)
	return err
}

// ghQ runs gh and returns trimmed stdout (stderr folded into the error).
func ghQ(args ...string) (string, error) {
	var stdout, stderr strings.Builder
	cmd := exec.Command("gh", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// envSanitizeCLI mirrors the runtime's loop-config env key sanitization
// (KARMAX_LOOP_<NAME>_<KEY>).
func envSanitizeCLI(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
