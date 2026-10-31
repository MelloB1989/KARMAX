package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/MelloB1989/karmax/fleet/internal/desk"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
	"github.com/MelloB1989/karmax/fleet/internal/term"
)

// DeskOps is the desk (internal/desk): the operator's tmux sessions on
// fleetd's own machine. Every route to it is the full scope's alone.
type DeskOps interface {
	List(ctx context.Context, lines int) (fleetapi.DeskList, error)
	Create(ctx context.Context, c fleetapi.DeskCreate) (fleetapi.DeskSession, error)
	End(ctx context.Context, name string) error
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
	if w, ok := ops.(withDesk); ok {
		return w.desk
	}
	return nil
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

// deskRoutes registers the desk: GET /v1/desk?lines=N (the sessions, with
// their screens), POST /v1/desk (a new one), DELETE /v1/desk/{name} (end it)
// and GET /v1/desk/dirs?prefix=P (the launch form's directories).
func deskRoutes(route func(string, func(http.ResponseWriter, *http.Request) (any, error), ...scope), ops Ops) {
	d := deskOf(ops)
	handle := func(pattern string, h func(r *http.Request) (any, error)) {
		route(pattern, func(w http.ResponseWriter, r *http.Request) (any, error) {
			if d == nil {
				return nil, notFound{errors.New("the desk is off (fleet.yaml desk.enabled)")}
			}
			v, err := h(r)
			return v, deskErr(err)
		}, fullOnly...)
	}
	handle("GET "+fleetapi.PathDesk, func(r *http.Request) (any, error) {
		lines, err := intParam(r, "lines")
		if err != nil {
			return nil, err
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
}

// deskErr is the desk's error as the API answers it.
func deskErr(err error) error {
	switch {
	case errors.Is(err, desk.ErrNoSession):
		return notFound{err}
	case errors.Is(err, desk.ErrInvalid):
		return badRequest{err}
	}
	return err
}
