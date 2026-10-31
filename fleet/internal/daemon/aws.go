package daemon

import (
	"context"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// AWSReader is the operator's AWS account as fleetctl's AWS tab shows it
// (*awsdash.Dash).
type AWSReader interface {
	Report(ctx context.Context, refresh bool) (*fleetapi.AWSReport, error)
}

// AWS is the account as last read, read again where that is old (at once
// on refresh); nil and no error when fleet.yaml has no aws:.
func (f *Fleet) AWS(ctx context.Context, refresh bool) (*fleetapi.AWSReport, error) {
	if f.AWSDash == nil {
		return nil, nil
	}
	return f.AWSDash.Report(ctx, refresh)
}
