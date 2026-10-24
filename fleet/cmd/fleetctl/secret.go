package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// secretCmd is `fleetctl secret`. put, ls, grant, revoke and rm are the
// full scope's; exec and get are an agent's, for a secret it is granted.
// Nothing here prints a value unless get is told --print.
func secretCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleetctl secret put|ls|grant|revoke|rm|exec|get … (fleetctl help)")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "put":
		var file string
		_, pos, err := flags("secret put", args, func(fs *flag.FlagSet) { fs.StringVar(&file, "file", "", "") })
		if err != nil {
			return err
		}
		if err := need(pos, 1, "secret put <name> [--file <path>]   (the value from stdin, or the file)"); err != nil {
			return err
		}
		value, err := secretValue(file, os.Stdin)
		if err != nil {
			return err
		}
		return post("/v1/secrets", map[string]any{"name": pos[0], "value": value})

	case "ls":
		var asJSON bool
		if _, _, err := flags("secret ls", args, func(fs *flag.FlagSet) { fs.BoolVar(&asJSON, "json", false, "") }); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		var out []ledger.SecretInfo
		if err := c.do("GET", "/v1/secrets", nil, &out); err != nil {
			return err
		}
		if asJSON {
			return printJSON(out)
		}
		return secretTable(os.Stdout, out, time.Now())

	case "grant", "revoke":
		var task, agent, ttl string
		_, pos, err := flags("secret "+sub, args, func(fs *flag.FlagSet) {
			fs.StringVar(&task, "task", "", "")
			fs.StringVar(&agent, "agent", "", "")
			if sub == "grant" {
				fs.StringVar(&ttl, "ttl", "", "")
			}
		})
		if err != nil {
			return err
		}
		use := "secret revoke <name> --task T | --agent A"
		if sub == "grant" {
			use = "secret grant <name> --task T | --agent A [--ttl 2h]"
		}
		if err := need(pos, 1, use); err != nil {
			return err
		}
		if (task == "") == (agent == "") {
			return fmt.Errorf("usage: fleetctl %s", use)
		}
		body := map[string]string{"task": task, "agent": agent}
		if ttl != "" {
			body["ttl"] = ttl
		}
		return post("/v1/secrets/"+url.PathEscape(pos[0])+"/"+sub, body)

	case "rm":
		_, pos, err := flags("secret rm", args, func(*flag.FlagSet) {})
		if err != nil {
			return err
		}
		if err := need(pos, 1, "secret rm <name>"); err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		if err := c.do("DELETE", "/v1/secrets/"+url.PathEscape(pos[0]), nil, nil); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil

	case "exec":
		pairs, argv, err := execArgs(args)
		if err != nil {
			return err
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		env := os.Environ()
		for _, p := range pairs {
			v, err := readSecret(c, p.secret)
			if err != nil {
				return err
			}
			if bytes.IndexByte(v, 0) >= 0 {
				return fmt.Errorf("secret %s holds a NUL byte, which no environment variable can: use fleetctl secret get --to", p.secret)
			}
			env = append(env, p.env+"="+string(v))
		}
		return runWithEnv(argv, env)

	case "get":
		var to string
		var print bool
		_, pos, err := flags("secret get", args, func(fs *flag.FlagSet) {
			fs.StringVar(&to, "to", "", "")
			fs.BoolVar(&print, "print", false, "")
		})
		if err != nil {
			return err
		}
		if err := need(pos, 1, "secret get <name> --to <file under "+secretsDir()+"> | --print"); err != nil {
			return err
		}
		if (to == "") == !print {
			return fmt.Errorf("usage: fleetctl secret get <name> --to <file> | --print (one: --print puts the value in your output, and so in the model's context)")
		}
		var path string
		if to != "" {
			if path, err = secretPath(secretsDir(), to); err != nil {
				return err
			}
		}
		c, err := newClient()
		if err != nil {
			return err
		}
		v, err := readSecret(c, pos[0])
		if err != nil {
			return err
		}
		if print {
			_, err := os.Stdout.Write(v)
			return err
		}
		if err := writeSecretFile(secretsDir(), path, v); err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	}
	return fmt.Errorf("fleetctl secret %s: no such command (put, ls, grant, revoke, rm, exec, get)", sub)
}

// secretValue is a put's value: the file's bytes as they are, or stdin's
// without the one line end that echo or a heredoc adds (keep an SSH key's
// in its file: --file). Typed at a terminal, it is not echoed.
func secretValue(file string, stdin *os.File) ([]byte, error) {
	if file != "" {
		return os.ReadFile(file)
	}
	var v []byte
	var err error
	if term.IsTerminal(int(stdin.Fd())) {
		fmt.Fprint(os.Stderr, "value (not echoed): ")
		v, err = term.ReadPassword(int(stdin.Fd()))
		fmt.Fprintln(os.Stderr)
	} else {
		v, err = io.ReadAll(io.LimitReader(stdin, 1<<20))
	}
	if err != nil {
		return nil, err
	}
	v = bytes.TrimSuffix(v, []byte("\n"))
	v = bytes.TrimSuffix(v, []byte("\r"))
	if len(v) == 0 {
		return nil, fmt.Errorf("no value on stdin (pipe it in, or use --file)")
	}
	return v, nil
}

// readSecret asks fleetd for a granted secret, as this agent.
func readSecret(c *client, name string) ([]byte, error) {
	var out struct{ Value []byte }
	if err := c.do("POST", "/v1/secrets/"+url.PathEscape(name)+"/read", nil, &out); err != nil {
		return nil, fmt.Errorf("secret %s: %w", name, err)
	}
	return out.Value, nil
}

type envSecret struct{ env, secret string }

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// execArgs splits `NAME=secret… -- cmd args…`.
func execArgs(args []string) ([]envSecret, []string, error) {
	const use = "usage: fleetctl secret exec NAME=<secret>… -- <cmd> [args…]"
	i := 0
	for i < len(args) && args[i] != "--" {
		i++
	}
	if i == 0 || i >= len(args)-1 {
		return nil, nil, errors.New(use)
	}
	var pairs []envSecret
	for _, a := range args[:i] {
		env, secret, ok := strings.Cut(a, "=")
		if !ok || !envName.MatchString(env) || secret == "" {
			return nil, nil, fmt.Errorf("%q: want NAME=<secret>; %s", a, use)
		}
		pairs = append(pairs, envSecret{env, secret})
	}
	return pairs, args[i+1:], nil
}

// runWithEnv runs a command with its own stdio and the given environment,
// and ends with its exit code.
func runWithEnv(argv, env []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return exitCode{ee.ExitCode()}
	}
	return err
}

// secretsDir is where get writes: $FLEET_WORK/.secrets, the agent's desk.
func secretsDir() string {
	w := os.Getenv("FLEET_WORK")
	if w == "" {
		w = "/work"
	}
	return filepath.Join(w, ".secrets")
}

// secretPath is where --to points, refused unless it is inside dir. A bare
// name is a file in dir.
func secretPath(dir, to string) (string, error) {
	p := to
	if !filepath.IsAbs(p) {
		if !strings.ContainsRune(p, filepath.Separator) {
			p = filepath.Join(dir, p)
		} else if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	p = filepath.Clean(p)
	if rel, err := filepath.Rel(dir, p); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--to %s: a secret is written only under %s", to, dir)
	}
	return p, nil
}

// writeSecretFile writes a value 0600, whole or not at all, in a 0700 dir
// that no symlink leads out of; a symlink at the path itself is replaced,
// not followed.
func writeSecretFile(dir, path string, v []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(realDir, realParent); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s leads out of %s", filepath.Dir(path), dir)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(v); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// secretTable is `secret ls`: names, grants and last reads.
func secretTable(out io.Writer, ss []ledger.SecretInfo, now time.Time) error {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tUPDATED\tGRANTS\tLAST READ")
	for _, s := range ss {
		var gs []string
		for _, g := range s.Grants {
			who := "agent " + g.Agent
			if g.Task != "" {
				who = "task " + g.Task
			}
			if !g.Expires.IsZero() {
				who += " (" + g.Expires.Sub(now).Round(time.Minute).String() + " left)"
			}
			gs = append(gs, who)
		}
		grants := strings.Join(gs, ", ")
		if grants == "" {
			grants = "-"
		}
		read := "-"
		if !s.ReadAt.IsZero() {
			read = clock(s.ReadAt) + " " + s.ReadAgent
			if s.ReadTask != "" {
				read += " (" + s.ReadTask + ")"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, clock(s.Updated), grants, read)
	}
	return w.Flush()
}
