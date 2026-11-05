package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
	"github.com/MelloB1989/karmax/fleet/internal/desk"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/term"
)

// DeskOps is the desk (internal/desk): the operator's tmux sessions on
// fleetd's own machine. Every route to it is the full scope's alone.
type DeskOps interface {
	List(ctx context.Context, lines int) (fleetapi.DeskList, error)
	// Changes is the list once it is no longer version since, or after wait.
	Changes(ctx context.Context, since string, wait time.Duration) (fleetapi.DeskList, error)
	Create(ctx context.Context, c fleetapi.DeskCreate) (fleetapi.DeskSession, error)
	Update(ctx context.Context, name string, u fleetapi.DeskUpdate) (fleetapi.DeskSession, error)
	End(ctx context.Context, name string) error
	CreateWorkspace(ctx context.Context, c fleetapi.DeskWorkspaceCreate) (fleetapi.DeskWorkspace, error)
	UpdateWorkspace(ctx context.Context, id string, u fleetapi.DeskWorkspaceUpdate) (fleetapi.DeskWorkspace, error)
	DeleteWorkspace(ctx context.Context, id string, end bool) error
	// Paste writes a pasted file (or, tarred, a folder) for a session.
	Paste(ctx context.Context, session, file string, tarred bool, body io.Reader) (fleetapi.DeskPasted, error)
	// File opens a regular file on fleetd's machine, for a download.
	File(path string) (*os.File, os.FileInfo, error)
	Dirs(prefix string) []string
	// Terminal is where the terminal route reaches a session.
	Terminal(name string) (term.Target, bool)
}

// WithDesk is ops with the desk: the API serves it, and the terminal route
// reaches its sessions as desk:<name>.
func WithDesk(ops Ops, d DeskOps) Ops { return withDesk{Ops: ops, desk: d} }

type withDesk struct {
	Ops
	desk DeskOps
}

// deskOf is ops's desk; nil when fleetd serves none.
func deskOf(ops Ops) DeskOps {
	for {
		switch w := ops.(type) {
		case withDesk:
			return w.desk
		case withFiles:
			ops = w.Ops
		default:
			return nil
		}
	}
}

// terminal is where a worker's terminal is: a desk session's on this
// machine (desk:<name>), or an agent's.
func terminal(ops Ops, worker string) (term.Target, bool) {
	if name, ok := fleetapi.DeskName(worker); ok {
		if d := deskOf(ops); d != nil {
			return d.Terminal(name)
		}
		return term.Target{}, false
	}
	return ops.Terminal(worker)
}

// deskRoutes registers the desk (fleetapi/desk.go): the sessions and their
// workspaces, a long poll for their changes, pastes and downloads.
func deskRoutes(route func(string, func(http.ResponseWriter, *http.Request) (any, error), ...scope),
	long func(func(http.ResponseWriter, *http.Request) (any, error)) func(http.ResponseWriter, *http.Request) (any, error), ops Ops) {
	d := deskOf(ops)
	handleW := func(pattern string, h func(w http.ResponseWriter, r *http.Request) (any, error)) {
		route(pattern, long(func(w http.ResponseWriter, r *http.Request) (any, error) {
			if d == nil {
				return nil, notFound{errors.New("the desk is off (fleet.yaml desk.enabled)")}
			}
			v, err := h(w, r)
			return v, deskErr(err)
		}), fullOnly...)
	}
	handle := func(pattern string, h func(r *http.Request) (any, error)) {
		handleW(pattern, func(_ http.ResponseWriter, r *http.Request) (any, error) { return h(r) })
	}
	handle("GET "+fleetapi.PathDesk, func(r *http.Request) (any, error) {
		lines, err := intParam(r, "lines")
		if err != nil {
			return nil, err
		}
		wait, err := intParam(r, "wait")
		if err != nil {
			return nil, err
		}
		if lines == 0 { // a window's list: shared, and waited on
			return d.Changes(r.Context(), r.URL.Query().Get("since"), time.Duration(min(wait, fleetapi.DeskMaxWait))*time.Second)
		}
		return d.List(r.Context(), int(min(lines, maxLines)))
	})
	handle("POST "+fleetapi.PathDesk, func(r *http.Request) (any, error) {
		var in fleetapi.DeskCreate
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return d.Create(r.Context(), in)
	})
	handle("PATCH "+fleetapi.PathDesk+"/{name}", func(r *http.Request) (any, error) {
		var in fleetapi.DeskUpdate
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return d.Update(r.Context(), r.PathValue("name"), in)
	})
	handle("DELETE "+fleetapi.PathDesk+"/{name}", func(r *http.Request) (any, error) {
		if err := d.End(r.Context(), r.PathValue("name")); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	})
	handle("GET "+fleetapi.PathDeskDirs, func(r *http.Request) (any, error) {
		dirs := d.Dirs(r.URL.Query().Get("prefix"))
		if dirs == nil {
			dirs = []string{}
		}
		return map[string][]string{"dirs": dirs}, nil
	})
	handle("POST "+fleetapi.PathDeskWorkspaces, func(r *http.Request) (any, error) {
		var in fleetapi.DeskWorkspaceCreate
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return d.CreateWorkspace(r.Context(), in)
	})
	handle("PATCH "+fleetapi.PathDeskWorkspaces+"/{id}", func(r *http.Request) (any, error) {
		var in fleetapi.DeskWorkspaceUpdate
		if err := body(r, &in); err != nil {
			return nil, err
		}
		return d.UpdateWorkspace(r.Context(), r.PathValue("id"), in)
	})
	handle("DELETE "+fleetapi.PathDeskWorkspaces+"/{id}", func(r *http.Request) (any, error) {
		if err := d.DeleteWorkspace(r.Context(), r.PathValue("id"), r.URL.Query().Get("end") == "1"); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	})
	handleW("POST "+fleetapi.PathDeskPaste, func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		session, file := q.Get("session"), q.Get("file")
		if session == "" || file == "" {
			return nil, badRequest{errors.New("paste: session and file are required")}
		}
		var tarred bool
		switch q.Get("kind") {
		case "", "file":
		case "tar":
			tarred = true
		default:
			return nil, badRequest{fmt.Errorf("paste: kind %q: want file or tar", q.Get("kind"))}
		}
		if r.ContentLength > fleetapi.DeskMaxPaste {
			return nil, tooLarge{fmt.Errorf("paste: %d MB is more than %d MB", r.ContentLength>>20, fleetapi.DeskMaxPaste>>20)}
		}
		return d.Paste(r.Context(), session, file, tarred, r.Body)
	})
	handleW("GET "+fleetapi.PathDeskFile, func(w http.ResponseWriter, r *http.Request) (any, error) {
		f, fi, err := d.File(r.URL.Query().Get("path"))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(f.Name())}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "", fi.ModTime(), f) // octet-stream unless its bytes say otherwise
		return streamed{}, nil
	})
}

// conflict is an error the API answers 409.
type conflict struct{ error }

func (conflict) Is(t error) bool { return t == daemon.ErrConflict }

// tooLarge is an error the API answers 413.
type tooLarge struct{ error }

// deskErr is the desk's error as the API answers it.
func deskErr(err error) error {
	switch {
	case errors.Is(err, desk.ErrNoSession):
		return notFound{err}
	case errors.Is(err, desk.ErrInvalid):
		return badRequest{err}
	case errors.Is(err, desk.ErrNotEmpty):
		return conflict{err}
	case errors.Is(err, desk.ErrTooLarge):
		return tooLarge{err}
	}
	return err
}
