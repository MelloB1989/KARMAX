package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/filexfer"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// inboxName is a task id that is safe as a directory name.
var inboxName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// Inbox is the directory a task's attached files arrive in.
func Inbox(task string) string { return filexfer.DefaultDir + "/" + task + "/" }

// maxBytes is the cap on one transfer.
func (f *Fleet) maxBytes() int64 { return f.cfg.Files.MaxBytes }

// fileErr turns what a transfer ran into into the error classes the server
// maps: a refused archive or path is the caller's (ErrBadInput), a stream
// over the cap stays ErrTooLarge.
func fileErr(err error) error {
	switch {
	case err == nil, errors.Is(err, filexfer.ErrTooLarge), errors.Is(err, ErrBadInput):
		return err
	case errors.Is(err, agent.ErrPath):
		return fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	return err
}

// counter counts the bytes read through it.
type counter struct {
	r io.Reader
	n atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// PutFiles unpacks a tar stream into dir in an agent's container, and
// returns how many bytes of it came. The stream is checked entry by entry on
// its way (filexfer.Sanitize) and never held: what a refusal or the size cap
// stops has already been unpacked up to that point.
func (f *Fleet) PutFiles(ctx context.Context, name, dir string, body io.Reader) (int64, error) {
	if err := f.known(name); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	if dir == "" {
		dir = filexfer.DefaultDir
	}
	dir, err := filexfer.CheckPath(dir)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	in := &counter{r: body}
	pr, pw := io.Pipe()
	checked := make(chan error, 1)
	go func() {
		err := filexfer.Sanitize(pw, filexfer.Limit(in, f.maxBytes()))
		pw.CloseWithError(err)
		checked <- err
	}()
	err = f.box(name).PutFiles(ctx, dir, pr)
	pr.Close() // a command that ended early must not leave the checker blocked
	if cerr := <-checked; cerr != nil && !errors.Is(cerr, io.ErrClosedPipe) {
		if errors.Is(cerr, filexfer.ErrTooLarge) {
			return in.n.Load(), cerr
		}
		return in.n.Load(), fmt.Errorf("%w: %v", ErrBadInput, cerr)
	}
	return in.n.Load(), fileErr(err)
}

// GetFiles writes a tar of a path in an agent's container to w, checked
// entry by entry on its way out, and cut at the size cap. Nothing is written
// to w before the command has produced its first entry, so that a path the
// container refuses can still be told apart from a transfer that broke.
func (f *Fleet) GetFiles(ctx context.Context, name, path string, w io.Writer) error {
	if err := f.known(name); err != nil {
		return fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	path, err := filexfer.CheckPath(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	ran := make(chan error, 1)
	go func() {
		err := f.box(name).GetFiles(ctx, path, pw)
		pw.CloseWithError(err)
		ran <- err
	}()
	serr := filexfer.Sanitize(w, filexfer.Limit(pr, f.maxBytes()))
	if serr != nil {
		cancel() // stop the command: nobody reads what it writes
	}
	pr.Close()
	rerr := <-ran
	switch {
	case serr != nil && errors.Is(serr, filexfer.ErrTooLarge):
		return serr
	case rerr != nil && !errors.Is(rerr, io.ErrClosedPipe) && !errors.Is(rerr, context.Canceled):
		return fileErr(rerr) // the command's own account first: it is why the stream broke
	case serr != nil:
		return fmt.Errorf("%w: %v", ErrBadInput, serr)
	}
	return nil
}

// Rsh runs a command in an agent's container with stdin and stdout streamed
// (fleetctl rsh, for rsync). Its exit status and the tail of its stderr
// come back; a non-zero exit is not an error here.
func (f *Fleet) Rsh(ctx context.Context, name string, argv []string, stdin io.Reader, stdout io.Writer) (target.Result, error) {
	if err := f.known(name); err != nil {
		return target.Result{}, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	if len(argv) == 0 || argv[0] == "" {
		return target.Result{}, fmt.Errorf("%w: rsh needs a command", ErrBadInput)
	}
	return f.box(name).Rsh(ctx, argv, stdin, stdout)
}

// Brief delivers the brief of a task placed with Attach, once its files are
// in: like run's own delivery, naming the inbox they are in. A brief that
// cannot be delivered closes the task as undelivered.
func (f *Fleet) Brief(ctx context.Context, task, from, repo, brief string, now time.Time) error {
	if strings.TrimSpace(brief) == "" {
		return fmt.Errorf("%w: a brief needs text", ErrBadInput)
	}
	t, err := f.openTask(task)
	if err != nil {
		return err
	}
	if from == "" {
		from = t.Assigner
	}
	_, err = f.deliver(ctx, RunRequest{Brief: brief, Repo: repo, From: from, Attach: true}, RunReply{Task: t.ID, Agent: t.Agent}, now)
	return err
}
