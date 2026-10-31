package aws

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"strings"
)

// Profile is one profile from the shared AWS config, as far as this source
// needs it: where it reads, which account it reads, and how it logs in.
// Nothing secret is held; the credentials file is read for its section names
// only.
type Profile struct {
	Name          string
	Region        string // the profile's own region, else the default profile's
	AccountID     string // from sso_account_id or role_arn; empty when only asking can tell
	SSOSession    string // sso_session: logged in with `aws sso login --sso-session`
	SSOStartURL   string // legacy SSO configured on the profile itself
	LoginSession  bool   // `aws login` console credentials
	Static        bool   // has keys in the credentials file
	RoleARN       string
	SourceProfile string
}

// LoginFix is the exact command that brings this profile's credentials back.
func (p Profile) LoginFix() string {
	switch {
	case p.SSOSession != "":
		return "aws sso login --sso-session " + p.SSOSession
	case p.SSOStartURL != "":
		return "aws sso login --profile " + p.Name
	case p.LoginSession && p.Name == "default":
		return "aws login"
	case p.LoginSession:
		return "aws login --profile " + p.Name
	case p.RoleARN != "" && p.SourceProfile != "":
		return "aws configure --profile " + p.SourceProfile + " (the source profile of " + p.Name + ")"
	default:
		return "aws configure --profile " + p.Name
	}
}

// LoadProfiles reads every profile from the shared config and credentials
// files, in the order the config file lists them; profiles found only in the
// credentials file follow. Missing files mean no profiles.
func LoadProfiles(configPath, credentialsPath string) ([]Profile, error) {
	cfg, err := readSections(configPath, true)
	if err != nil {
		return nil, err
	}
	creds, err := readSections(credentialsPath, false)
	if err != nil {
		return nil, err
	}

	defaultRegion := ""
	for _, s := range cfg {
		if s.kind == "profile" && s.name == "default" {
			defaultRegion = s.keys["region"]
		}
	}

	var out []Profile
	index := map[string]int{}
	for _, s := range cfg {
		if s.kind != "profile" {
			continue
		}
		p := Profile{
			Name:          s.name,
			Region:        s.keys["region"],
			AccountID:     s.keys["sso_account_id"],
			SSOSession:    s.keys["sso_session"],
			RoleARN:       s.keys["role_arn"],
			SourceProfile: s.keys["source_profile"],
			LoginSession:  s.keys["login_session"] != "",
		}
		if p.SSOSession == "" {
			p.SSOStartURL = s.keys["sso_start_url"]
		}
		if p.AccountID == "" && p.RoleARN != "" {
			p.AccountID = accountFromARN(p.RoleARN)
		}
		index[p.Name] = len(out)
		out = append(out, p)
	}
	for _, s := range creds {
		if i, ok := index[s.name]; ok {
			out[i].Static = true
			continue
		}
		index[s.name] = len(out)
		out = append(out, Profile{Name: s.name, Static: true})
	}
	for i := range out {
		if out[i].Region == "" {
			out[i].Region = defaultRegion
		}
	}
	return out, nil
}

// Allowed keeps the named profiles, in the order named; no names keeps all.
func Allowed(ps []Profile, names []string) []Profile {
	if len(names) == 0 {
		return ps
	}
	byName := make(map[string]Profile, len(ps))
	for _, p := range ps {
		byName[p.Name] = p
	}
	var out []Profile
	for _, n := range names {
		if p, ok := byName[strings.TrimSpace(n)]; ok {
			out = append(out, p)
			delete(byName, p.Name)
		}
	}
	return out
}

// accountFromARN reads the account id out of arn:aws:iam::<account>:role/x.
func accountFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 6 || parts[0] != "arn" {
		return ""
	}
	return parts[4]
}

type section struct {
	kind string // "profile", "sso-session" or another prefix
	name string
	keys map[string]string
}

// configKeys are the only keys ever read. Whatever else a section holds,
// secrets included, is skipped without being kept.
var configKeys = map[string]bool{
	"region": true, "sso_session": true, "sso_account_id": true, "sso_start_url": true,
	"login_session": true, "role_arn": true, "source_profile": true,
}

// readSections reads an INI file's sections. In the config file a profile is
// "[profile name]" (the default is "[default]"); in the credentials file every
// section is a profile and no key is read at all.
func readSections(path string, isConfig bool) ([]section, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) || path == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []section
	var cur *section
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			header := strings.TrimSpace(line[1 : len(line)-1])
			s := section{kind: "profile", name: header, keys: map[string]string{}}
			if isConfig && header != "default" {
				kind, name, ok := strings.Cut(header, " ")
				if !ok {
					// "[name]" in a config file is not a profile the CLI reads.
					s.kind, s.name = "other", header
				} else {
					s.kind, s.name = kind, strings.TrimSpace(name)
				}
			}
			out = append(out, s)
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil || !isConfig {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if configKeys[k] {
			cur.keys[k] = strings.TrimSpace(v)
		}
	}
	return out, sc.Err()
}
