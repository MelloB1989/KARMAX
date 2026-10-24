package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/filexfer"
)

// remoteSpec is <agent>:<path>. An agent's name is two characters or more, so
// that C:\dir is a path on this machine.
var remoteSpec = regexp.MustCompile(`^([a-z0-9][a-z0-9-]+):(.*)$`)

// open sends a request that streams: no overall timeout (a transfer takes as
// long as it takes), and the body is sent as it is read. A failing status is
// a *statusError, like c.do.
func (c *client) open(ctx context.Context, method, path string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ExpectContinueTimeout: 10 * time.Second}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fleetd unreachable at %s: %w", c.base, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, &statusError{Code: resp.StatusCode, msg: e.Error, Body: b}
		}
		return nil, &statusError{Code: resp.StatusCode, msg: "fleetd: " + resp.Status, Body: b}
	}
	return resp, nil
}

// upload packs local paths and streams them into dir in an agent's
// container; it returns the bytes fleetd took.
func upload(ctx context.Context, c *client, agent, dir string, paths []string) (int64, error) {
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			return 0, err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := filexfer.Pack(pw, paths)
		pw.CloseWithError(err)
		packed <- err
	}()
	q := url.Values{"path": {dir}}.Encode()
	resp, err := c.open(ctx, "PUT", "/v1/agents/"+url.PathEscape(agent)+"/files?"+q, pr,
		map[string]string{"Content-Type": "application/x-tar", "Expect": "100-continue"})
	pr.Close()
	cancel()
	if perr := <-packed; perr != nil && !errors.Is(perr, io.ErrClosedPipe) {
		return 0, perr // what could not be packed is the reason, not the broken stream
	}
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct{ Bytes int64 }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Bytes, nil
}

// download streams a path of an agent's container into the local directory.
func download(ctx context.Context, c *client, agent, path, dir string) error {
	resp, err := c.open(ctx, "GET", "/v1/agents/"+url.PathEscape(agent)+"/files?"+url.Values{"path": {path}}.Encode(), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	xerr := filexfer.Extract(dir, resp.Body)
	_, _ = io.Copy(io.Discard, resp.Body) // the trailers come after the body
	if msg := resp.Trailer.Get("X-Fleet-Error"); msg != "" {
		return fmt.Errorf("%s (what arrived before it is in %s)", msg, dir)
	}
	return xerr
}

// cpCmd is `cp <local>... <agent>[:<dir>]` and `cp <agent>:<path> [<localdir>]`.
func cpCmd(args []string) error {
	const usage = `cp <local>... <agent>[:<dir>]   |   cp <agent>:<path> [<localdir>]`
	_, pos, err := flags("cp", args, func(fs *flag.FlagSet) {})
	if err != nil {
		return err
	}
	if len(pos) < 2 && !(len(pos) == 1 && remoteSpec.MatchString(pos[0])) {
		return fmt.Errorf("usage: fleetctl %s", usage)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if m := remoteSpec.FindStringSubmatch(pos[0]); m != nil {
		if len(pos) > 2 || m[2] == "" {
			return fmt.Errorf("usage: fleetctl %s", usage)
		}
		dir := "."
		if len(pos) == 2 {
			dir = pos[1]
		}
		path, err := filexfer.CheckPath(m[2])
		if err != nil {
			return err
		}
		if err := download(ctx, c, m[1], path, dir); err != nil {
			return err
		}
		fmt.Printf("%s:%s → %s\n", m[1], path, dir)
		return nil
	}
	agent, dir, _ := strings.Cut(pos[len(pos)-1], ":")
	if dir == "" {
		dir = filexfer.DefaultDir
	}
	if dir, err = filexfer.CheckPath(dir); err != nil {
		return err
	}
	local := pos[:len(pos)-1]
	n, err := upload(ctx, c, agent, dir, local)
	if err != nil {
		return err
	}
	fmt.Printf("%d path(s), %s → %s:%s\n", len(local), filexfer.Size(n), agent, dir)
	return nil
}

// rshCmd is `rsh <agent> <cmd…>`: the command runs in the agent's container
// with this process's stdin and stdout joined to it, which is what rsync's
// -e wants. It exits with the command's status.
func rshCmd(args []string) error {
	// rsync may say who it is: -l user. There is no user.
	for len(args) >= 2 && args[0] == "-l" {
		args = args[2:]
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: fleetctl rsh <agent> <cmd…>")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	q := url.Values{"arg": args[1:]}.Encode()
	resp, err := c.open(context.Background(), "POST", "/v1/agents/"+url.PathEscape(args[0])+"/rsh?"+q, os.Stdin, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return err
	}
	if msg, _ := url.QueryUnescape(resp.Trailer.Get("X-Fleet-Stderr")); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	if msg := resp.Trailer.Get("X-Fleet-Error"); msg != "" {
		return errors.New(msg)
	}
	code, err := strconv.Atoi(resp.Trailer.Get("X-Fleet-Exit"))
	if err != nil {
		return fmt.Errorf("fleetd ended the stream without an exit status")
	}
	if code != 0 {
		return exitCode{code}
	}
	return nil
}

// syncCmd is `sync <localdir> <agent>:<dir>` and the reverse, by rsync
// through `fleetctl rsh`.
func syncCmd(args []string) error {
	var del, dry bool
	var exclude repeated
	_, pos, err := flags("sync", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&del, "delete", false, "")
		fs.BoolVar(&dry, "dry-run", false, "")
		fs.Var(&exclude, "exclude", "")
	})
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf("usage: fleetctl sync [--delete] [--dry-run] [--exclude P] <localdir> <agent>:<dir>   |   <agent>:<dir> <localdir>")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	argv, err := rsyncArgv(self, pos[0], pos[1], del, dry, exclude)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		return fmt.Errorf("rsync is not installed on this machine")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// rsyncArgv is the rsync command for a sync. Exactly one side is remote, and
// its directory is under /work or /tmp. A trailing slash on the source means
// what it does in rsync: its contents, not the directory itself.
func rsyncArgv(self, src, dst string, del, dry bool, exclude []string) ([]string, error) {
	remote := func(s string) (string, error) {
		m := remoteSpec.FindStringSubmatch(s)
		if m == nil {
			return s, nil
		}
		p, err := filexfer.CheckPath(m[2])
		if err != nil {
			return "", err
		}
		return m[1] + ":" + p, nil
	}
	var err error
	if src, err = remote(src); err != nil {
		return nil, err
	}
	if dst, err = remote(dst); err != nil {
		return nil, err
	}
	if remoteSpec.MatchString(src) == remoteSpec.MatchString(dst) {
		return nil, fmt.Errorf("one side of a sync is an agent (<agent>:<dir>), the other a local directory")
	}
	rsh := self + " rsh"
	if strings.ContainsAny(self, " \t") {
		rsh = `"` + self + `" rsh`
	}
	argv := []string{"rsync", "-a", "-e", rsh}
	if del {
		argv = append(argv, "--delete")
	}
	if dry {
		argv = append(argv, "--dry-run", "-v")
	}
	for _, e := range exclude {
		argv = append(argv, "--exclude", e)
	}
	return append(argv, src, dst), nil
}
