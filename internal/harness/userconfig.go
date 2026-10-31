package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
)

// ownSettingSources are the Claude Code setting sources a session loads
// unless its kind inherits the operator's user config: the project's and the
// local settings of its workdir, which is also where CLAUDE.md is read from
// (the workdir and every parent: KARMAX's own brief at the data root among
// them). Never the user source, ~/.claude: the operator's plugins, skills,
// hooks and settings, built for their own interactive sessions.
//
// Measured on this host's CLI (2.1.314) with one first turn: the operator's
// set loaded 103 skills, 8 plugins and 4 start-up hooks into a 31.6k-token
// first context; without it, 20 skills (Claude Code's own) and no hooks in
// 18.3k, ready 0.2s sooner warm and 1.5s sooner cold. Auth is unaffected: it
// is not a setting.
const ownSettingSources = "project,local"

// customise fills in what a session loads of Claude Code's customisation:
// the allowlisted plugins, skills and settings — the kind's own lists where
// it has them, else the supervisor's.
func (s *Supervisor) customise(sess *Session, pol Policy) {
	sess.InheritUserConfig = pol.InheritUserConfig
	plugins, skills := s.cfg.Plugins, s.cfg.Skills
	if pol.Plugins != nil {
		plugins = pol.Plugins
	}
	if pol.Skills != nil {
		skills = pol.Skills
	}

	settings := maps.Clone(s.cfg.Settings)
	enabled := map[string]any{}
	if set, ok := settings["enabledPlugins"].(map[string]any); ok {
		maps.Copy(enabled, set)
	}
	for _, p := range plugins {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
		case strings.ContainsRune(p, '/') || strings.ContainsRune(p, filepath.Separator):
			// A directory: loaded for this session only, installed or not.
			sess.PluginDirs = append(sess.PluginDirs, p)
		default:
			// An installed plugin's id. The flag's settings outrank every
			// file's, so this enables it whatever the operator's own says.
			enabled[p] = true
		}
	}
	if len(enabled) > 0 {
		if settings == nil {
			settings = map[string]any{}
		}
		settings["enabledPlugins"] = enabled
	}
	if len(settings) > 0 {
		if b, err := json.Marshal(settings); err == nil {
			sess.Settings = string(b)
		} else {
			s.log.Warn("harness: ignoring settings that are not JSON", "err", err.Error())
		}
	}

	if len(skills) == 0 {
		return
	}
	dir, missing, err := gatherSkills(s.skillsRoot(), skills)
	if err != nil {
		s.log.Warn("harness: could not gather the allowlisted skills", "err", err.Error())
		return
	}
	if len(missing) > 0 {
		s.log.Warn("harness: allowlisted skills that do not exist", "skills", missing)
	}
	sess.SkillsDir = dir
}

func (s *Supervisor) skillsRoot() string {
	if s.cfg.SkillsRoot != "" {
		return s.cfg.SkillsRoot
	}
	return filepath.Join(s.cfg.WorkdirRoot, ".skills")
}

// gatherSkills links each skill directory into one directory the CLI reads
// skills from — --add-dir <dir> loads <dir>/.claude/skills/* — and returns
// it, with the skills that do not exist (not linked). Each skill keeps its
// own name, where a plugin would put it under the plugin's namespace.
//
// The directory is named for the set, so kinds with different sets never
// share one, and the same set always lands in the same place. Each link is
// written beside its final name and renamed into place, so two sessions
// gathering the same set at once never see a half-made one. If two skills
// share a name, the first is kept.
func gatherSkills(root string, dirs []string) (dir string, missing []string, err error) {
	sum := sha256.Sum256([]byte(strings.Join(dirs, "\x00")))
	dir = filepath.Join(root, hex.EncodeToString(sum[:6]))
	into := filepath.Join(dir, ".claude", "skills")
	if err := os.MkdirAll(into, 0o755); err != nil {
		return "", nil, err
	}
	seen := map[string]bool{}
	for _, src := range dirs {
		name := filepath.Base(src)
		if seen[name] {
			continue
		}
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
			missing = append(missing, src)
			continue
		}
		seen[name] = true
		link := filepath.Join(into, name)
		if cur, err := os.Readlink(link); err == nil && cur == src {
			continue
		}
		tmp := fmt.Sprintf("%s.%x.tmp", link, rand.Uint64())
		if err := os.Symlink(src, tmp); err != nil {
			return "", nil, err
		}
		if err := os.Rename(tmp, link); err != nil {
			_ = os.Remove(tmp)
			return "", nil, err
		}
	}
	return dir, missing, nil
}
