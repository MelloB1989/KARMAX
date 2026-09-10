package fsscope

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Turning a policy into rules Claude Code will actually enforce.
//
// The semantics here were established by trying them, not by reading about
// them, and three of them are surprising enough to be worth writing down:
//
//   - `--dangerously-skip-permissions` ignores deny rules completely. A policy
//     and that flag cannot both be in the same command line, which is why
//     Enforced switches the harness to --permission-mode dontAsk.
//   - An absolute path in a rule needs a SECOND leading slash —
//     `Read(//Users/x/.ssh/**)`. Written with one it silently matches nothing,
//     which looks exactly like a rule that is working.
//   - The write-side rule is `Edit(...)`, not `Write(...)`. Claude Code says so
//     out loud if you get it wrong, but it says it to stderr while carrying on.
//
// The one that matters most: a denied path is denied to the Bash tool too, so
// `cat ~/.ssh/id_rsa` is refused rather than routed around. Without that this
// would be decoration.

// harnessTools is everything the harness is allowed to reach with the policy
// on.
//
// dontAsk denies anything not named here, so this list is what stops a policy
// about FILES from quietly removing the agent's ability to search the web. It
// has to track Claude Code's built-in tool names; a name that disappears costs
// nothing, a tool that appears and is not added is simply unavailable.
var harnessTools = []string{
	"Bash", "BashOutput", "KillShell",
	"Read", "Edit", "Write", "NotebookEdit",
	"Glob", "Grep",
	"WebFetch", "WebSearch",
	"TodoWrite", "Task", "SlashCommand", "ExitPlanMode",
	// The browser, when it is attached. Named by server, which covers every
	// tool that server offers.
	"mcp__playwright",
}

// StandardDenies are the paths never handed over, whatever was granted.
//
// Not stored in anybody's policy file: it is computed every time, so a machine
// that has been running for a year gets the same protection as a fresh one
// without somebody editing JSON. The operator can add to it and cannot subtract
// from it, which is the right way round — "read everything under my home" is
// something people say, and it is never what they mean about their private key.
func StandardDenies(dataDir string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	var out []string
	add := func(parts ...string) {
		if home == "" {
			return
		}
		out = append(out, filepath.Join(append([]string{home}, parts...)...))
	}

	// Credentials for other systems. The agent has its own way into everything
	// it is supposed to reach; these are the keys to the things it is not.
	add(".ssh")
	add(".gnupg")
	add(".aws")
	add(".azure")
	add(".config", "gcloud")
	add(".kube")
	add(".docker", "config.json")
	add(".netrc")
	add(".npmrc")
	add(".pypirc")

	// The harness's own credentials. An agent that can read the token it runs
	// on can hand that token to somebody else.
	add(".claude", ".credentials.json")
	add(".claude.json")
	add(".codex", "auth.json")

	// KARMAX's own state, and the sessions it holds on the operator's behalf.
	//
	// The browser profile is the important one. "The browser being open is the
	// grant" is only true while the agent cannot read the cookie jar directly —
	// otherwise one Read turns a window the person can close into every account
	// they have ever signed into, permanently.
	data := strings.TrimSpace(dataDir)
	if data == "" && home != "" {
		data = filepath.Join(home, ".karmax")
	}
	if data != "" {
		out = append(out,
			filepath.Join(data, "browser"),
			filepath.Join(data, "db"),
			filepath.Join(data, "access.json"),
		)
	}
	add(".config", "gogcli")
	add(".wacli")
	add(".local", "share", "keyrings")

	switch runtime.GOOS {
	case "darwin":
		add("Library", "Keychains")
		add("Library", "Application Support", "Google", "Chrome")
		add("Library", "Application Support", "Firefox")
		add("Library", "Cookies")
	case "linux":
		add(".mozilla")
		add(".config", "google-chrome")
		add(".config", "chromium")
	case "windows":
		add("AppData", "Local", "Google", "Chrome", "User Data")
		add("AppData", "Roaming", "Mozilla")
	}
	return out
}

// Settings is the JSON handed to Claude Code as --settings.
type Settings struct {
	Permissions Permissions `json:"permissions"`
}

// Permissions is Claude Code's own shape. Deny wins over allow.
type Permissions struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// SettingsJSON renders the policy as a --settings value.
//
// Returns "" when nothing is enforced, which is the caller's signal to keep
// doing what it did before this package existed.
func (p Policy) SettingsJSON(dataDir string) string {
	if !p.Enforced {
		return ""
	}
	perms := Permissions{Allow: append([]string(nil), harnessTools...)}

	// What the denials actually do, stated plainly because the alternative is a
	// feature that reads as stronger than it is.
	//
	// Claude Code has no way to say "only these folders" while keeping the
	// shell — the flag that does that (--restricted) takes Bash away, and a
	// harness that cannot run a command is not the harness KARMAX delegates to.
	// So the grants are where it WORKS (--add-dir), and the denials are the
	// hard boundary. Restricting a directory is a real block; granting one is
	// an instruction, not a fence. Anything shown to a person has to say it
	// that way round.
	for _, path := range p.denials(dataDir) {
		for _, rule := range rulePaths(path) {
			perms.Deny = append(perms.Deny,
				"Read("+rule+")",
				// Edit, not Write: Write(path) matches nothing, and Claude Code
				// warns about it on stderr while carrying on regardless.
				"Edit("+rule+")",
			)
		}
	}
	for _, g := range p.Grants {
		if g.Write {
			continue
		}
		// A read-only grant is a grant with the writing taken back out, and
		// this half IS enforced.
		for _, rule := range rulePaths(g.Path) {
			perms.Deny = append(perms.Deny, "Edit("+rule+")")
		}
	}

	b, err := json.Marshal(Settings{Permissions: perms})
	if err != nil {
		return ""
	}
	return string(b)
}

// denials is the operator's list plus the standard one, deduplicated.
func (p Policy) denials(dataDir string) []string {
	seen := map[string]bool{}
	var out []string
	for _, path := range append(append([]string{}, StandardDenies(dataDir)...), p.Deny...) {
		key := path
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			key = strings.ToLower(path)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, path)
	}
	return out
}

// rulePaths writes an absolute path the way Claude Code's matcher wants it.
//
// Two rules, because a denial has to cover both a directory and a single file
// and the shapes differ. `~/.ssh` needs the `/**` form to cover what is inside
// it; `~/.claude.json` is a file, and the `/**` form alone would match nothing
// at all while looking exactly like a rule that works.
//
// The second leading slash is not a typo and not cosmetic: `Read(/x/**)` is
// read as a path relative to the project and matches nothing, while
// `Read(//x/**)` is the absolute one. That was found by testing, and a rule
// that silently matches nothing is the worst way for a permission system to be
// wrong.
func rulePaths(abs string) []string {
	abs = filepath.ToSlash(strings.TrimSuffix(filepath.ToSlash(abs), "/"))
	if !strings.HasPrefix(abs, "/") {
		// Windows: C:/Users/... becomes //C:/Users/...
		abs = "/" + abs
	}
	return []string{"/" + abs, "/" + abs + "/**"}
}
