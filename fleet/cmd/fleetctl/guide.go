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

// skillCmd: skill install [--dir D] [--print] [--force]. It writes the fleet
// skill, and the fleet-memory skill beside it for those fleetd gives the
// operator's memory (scope full: the operator, on a token or the tailnet;
// relay: an agent). Both are built into this binary; only who you are is
// asked of fleetd. A member gets the fleet skill alone, and a fleet-memory
// skill fleetctl wrote earlier is removed. When fleetd cannot say, the fleet
// skill is written and fleet-memory left as it is.
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
	skills := []struct{ name, text string }{{"fleet", guide.Skill()}}
	scope, whoErr := whoScope()
	memory := whoErr == nil && mayUse(scope, "memory_recall")
	if memory {
		skills = append(skills, struct{ name, text string }{"fleet-memory", guide.MemorySkill()})
	}
	if print {
		for i, sk := range skills {
			if i > 0 {
				fmt.Println()
			}
			fmt.Print(sk.text)
		}
		return nil
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".claude", "skills")
	}
	for _, sk := range skills {
		p := filepath.Join(dir, sk.name, "SKILL.md")
		if old, err := os.ReadFile(p); err == nil && !force && !strings.HasPrefix(string(old), "---\n") {
			return fmt.Errorf("%s exists and is not a skill; pass --force to replace it", p)
		}
	}
	for _, sk := range skills {
		p := filepath.Join(dir, sk.name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(sk.text), 0o644); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o644); err != nil { // WriteFile keeps an existing file's mode
			return err
		}
		fmt.Println(p)
	}
	switch {
	case whoErr != nil:
		fmt.Fprintf(os.Stderr, "fleetctl: fleetd cannot say who you are (%v): the fleet-memory skill is left as it is\n", whoErr)
	case !memory:
		p := filepath.Join(dir, "fleet-memory", "SKILL.md")
		if old, err := os.ReadFile(p); err == nil && guide.IsMemorySkill(string(old)) {
			if err := os.Remove(p); err != nil {
				return err
			}
			_ = os.Remove(filepath.Dir(p)) // only if nothing else is in it
			fmt.Printf("removed %s: the operator's memory is not for scope %s\n", p, scope)
		}
	}
	return nil
}
