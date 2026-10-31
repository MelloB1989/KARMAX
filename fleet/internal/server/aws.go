package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// AWSOps is the AWS account fleetd shows the operator (internal/awsdash).
type AWSOps interface {
	// AWS is the account as last read, read again where that is old (at
	// once on refresh); nil and no error when fleet.yaml has no aws:.
	AWS(ctx context.Context, refresh bool) (*fleetapi.AWSReport, error)
}

// awsRead bounds a read of the account: every region, and Cost Explorer.
// It is the account's, not one caller's: a caller who leaves does not end
// it, and the others still get it.
const awsRead = 3 * time.Minute

// awsRoutes registers GET /v1/aws[?refresh=1], for the operator alone.
// Callers who ask while a read is on its way share it, a refresh too.
func awsRoutes(route func(string, func(http.ResponseWriter, *http.Request) (any, error), ...scope), ops AWSOps) {
	var f awsFlight
	route("GET "+fleetapi.PathAWS, func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query().Get("refresh")
		rep, err := f.read(r.Context(), q == "1" || q == "true", ops)
		if rep == nil && err == nil {
			return nil, notFound{errors.New("aws is not set up in fleet.yaml (aws: keys_csv: …)")}
		}
		return rep, err
	}, fullOnly...)
}

// awsFlight is the read in flight, if any.
type awsFlight struct {
	mu  sync.Mutex
	cur *awsCall
}

type awsCall struct {
	done chan struct{}
	rep  *fleetapi.AWSReport
	err  error
}

// read joins the read in flight, or starts one, and waits for it or for
// ctx to end.
func (f *awsFlight) read(ctx context.Context, refresh bool, acct AWSOps) (*fleetapi.AWSReport, error) {
	f.mu.Lock()
	c := f.cur
	if c == nil {
		c = &awsCall{done: make(chan struct{})}
		f.cur = c
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), awsRead)
			defer cancel()
			c.rep, c.err = acct.AWS(rctx, refresh)
			f.mu.Lock()
			f.cur = nil
			f.mu.Unlock()
			close(c.done)
		}()
	}
	f.mu.Unlock()
	select {
	case <-c.done:
		return c.rep, c.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}
