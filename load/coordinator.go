package load

import (
	"context"

	"github.com/nashcom/eas-tools/herd"
)

// Coordinator is what the worker needs from the thing that hands out accounts
// and commands: the nshtestherd client (herd.Client) in coordinator mode, or
// Local in standalone mode.
type Coordinator interface {
	// Register returns the account and test_id of a new client.
	Register(ctx context.Context, requestKey string) (*herd.Reply, error)
	// Status passes on the report of a client and returns what it should do.
	Status(ctx context.Context, r herd.Report) (*herd.Reply, error)
}

var _ Coordinator = (*herd.Client)(nil)
