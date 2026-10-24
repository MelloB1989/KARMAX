package server

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/filexfer"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

func (f *fake) Brief(_ context.Context, task, from, repo, brief string, _ time.Time) error {
	f.calls = append(f.calls, fmt.Sprintf("brief %s from=%q repo=%q %s", task, from, repo, brief))
	return nil
}

func (f *fake) PutFiles(_ context.Context, agent, dir string, body io.Reader) (int64, error) {
	b, _ := io.ReadAll(body)
	f.calls = append(f.calls, fmt.Sprintf("put %s %s %s", agent, dir, b))
	if agent == "agent-big" {
		return int64(len(b)), fmt.Errorf("%w (2 GiB)", filexfer.ErrTooLarge)
	}
	return int64(len(b)), nil
}

func (f *fake) GetFiles(_ context.Context, agent, path string, w io.Writer) error {
	f.calls = append(f.calls, "get "+agent+" "+path)
	switch path {
	case "/etc":
		return fmt.Errorf("%w: files go under /work or /tmp only", daemon.ErrBadInput)
	case "/work/half":
		_, _ = io.WriteString(w, "PART")
		return fmt.Errorf("the container went away")
	}
	_, err := io.WriteString(w, "TARDATA")
	return err
}

func (f *fake) Rsh(_ context.Context, agent string, argv []string, stdin io.Reader, stdout io.Writer) (target.Result, error) {
	f.calls = append(f.calls, fmt.Sprintf("rsh %s %q", agent, argv))
	_, err := io.Copy(stdout, stdin)
	return target.Result{Exit: 3, Stderr: "oops"}, err
}
