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

// workerSuffix is a slot's number after an agent's name: agent-03:2:/dir is
// the second session of agent-03, in the same container and /work.
var workerSuffix = regexp.MustCompile(`^([0-9]+)(?::(.*))?$`)

// parseRemote splits <worker>:<path>, where the worker is an agent or
// agent:N. A path made of digits alone must be written ./2 or /work/2.
func parseRemote(s string) (worker, path string, ok bool) {
	m := remoteSpec.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	if w := workerSuffix.FindStringSubmatch(m[2]); w != nil {
		return m[1] + ":" + w[1], w[2], true
	}
	return m[1], m[2], true
}

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
		ExpectContinueTimeout: 3 * time.Second}
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
	_, _, firstRemote := parseRemote(pos[0])
	if len(pos) < 2 && !(len(pos) == 1 && firstRemote) {
		return fmt.Errorf("usage: fleetctl %s", usage)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if worker, rpath, ok := parseRemote(pos[0]); ok {
		if len(pos) > 2 || rpath == "" {
			return fmt.Errorf("usage: fleetctl %s", usage)
		}
		dir := "."
		if len(pos) == 2 {
			dir = pos[1]
		}
		path, err := filexfer.CheckPath(rpath)
		if err != nil {
			return err
		}
		if err := download(ctx, c, worker, path, dir); err != nil {
			return err
		}
		fmt.Printf("%s:%s → %s\n", worker, path, dir)
		return nil
	}
	last := pos[len(pos)-1]
	agent, dir, ok := parseRemote(last)
	if !ok {
		agent, dir = last, ""
	}
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
//
// What follows the agent is another program's argv (rsync's --server and its
// flags), so it is taken as it stands and not read by flags().
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
	resp, err := c.open(context.Background(), "POST", "/v1/agents/"+url.PathEscape(rshWorker(args[0]))+"/rsh?"+q, os.Stdin,
		map[string]string{"Expect": "100-continue"}) // a refusal comes back without waiting on stdin
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

// rshWorker turns the host name sync gave rsync (agent-03+2) back into the
// worker's (agent-03:2).
func rshWorker(host string) string {
	if i := strings.LastIndex(host, "+"); i > 0 && workerSuffix.MatchString(host[i+1:]) && !strings.Contains(host[i+1:], ":") {
		return host[:i] + ":" + host[i+1:]
	}
	return host
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
	// rsync splits host:path at the first colon, so a worker (agent-03:2) goes
	// as agent-03+2, which rsh turns back.
	remote := func(s string) (string, bool, error) {
		worker, rpath, ok := parseRemote(s)
		if !ok {
			return s, false, nil
		}
		p, err := filexfer.CheckPath(rpath)
		if err != nil {
			return "", true, err
		}
		if strings.HasSuffix(rpath, "/") && p != "/" {
			p += "/" // rsync: the directory's contents, not the directory
		}
		return strings.Replace(worker, ":", "+", 1) + ":" + p, true, nil
	}
	src, srcRemote, err := remote(src)
	if err != nil {
		return nil, err
	}
	dst, dstRemote, err := remote(dst)
	if err != nil {
		return nil, err
	}
	if srcRemote == dstRemote {
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
