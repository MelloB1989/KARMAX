package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MelloB1989/karmax/fleet/internal/guide"
)

// guideCmd prints the guide fleetd serves.
func guideCmd(args []string) error {
	return getAndPrint(args, "/v1/guide", func(raw json.RawMessage) error {
		var r struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		fmt.Print(r.Text)
		if !strings.HasSuffix(r.Text, "\n") {
			fmt.Println()
		}
		return nil
	})
}

// skillCmd: skill install [--dir D] [--print] [--force]. The skill is built
// into this binary, so it installs without fleetd.
func skillCmd(args []string) error {
	const use = "skill install [--dir D] [--print] [--force]"
	if len(args) == 0 || args[0] != "install" {
		return errors.New("usage: fleetctl " + use)
	}
	var dir string
	var print, force bool
	_, pos, err := flags("skill install", args[1:], func(fs *flag.FlagSet) {
		fs.StringVar(&dir, "dir", "", "")
		fs.BoolVar(&print, "print", false, "")
		fs.BoolVar(&force, "force", false, "")
	})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("usage: fleetctl " + use)
	}
	if print {
		fmt.Print(guide.Skill())
		return nil
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".claude", "skills")
	}
	p := filepath.Join(dir, "fleet", "SKILL.md")
	if old, err := os.ReadFile(p); err == nil && !force && !strings.HasPrefix(string(old), "---\n") {
		return fmt.Errorf("%s exists and is not a skill; pass --force to replace it", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(guide.Skill()), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(p, 0o644); err != nil { // WriteFile keeps an existing file's mode
		return err
	}
	fmt.Println(p)
	return nil
}
