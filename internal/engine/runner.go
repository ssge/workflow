package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ssge/workflow/db/store"
)

// Runner applies at most one leased cause at a time per polling loop. Its
// only local scheduling state is which kind to check first.
type Runner struct {
	engine  *Engine
	owner   string
	leaseMS int64
	turn    atomic.Uint32
}

// NewRunner uses an owner ID unique to the runner for its leases.
func (e *Engine) NewRunner(owner string, lease time.Duration) (*Runner, error) {
	if owner == "" || lease.Milliseconds() <= 0 {
		return nil, errors.New("runner requires an owner and a lease of at least one millisecond")
	}
	return &Runner{engine: e, owner: owner, leaseMS: lease.Milliseconds()}, nil
}

// RunOnce tries each work class in rotating order until it claims one
// item. It reports true only for a committed application of that item.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	kinds := [...]store.WorkKind{store.WorkCommand, store.WorkStep, store.WorkTimer}
	first := int((r.turn.Add(1) - 1) % uint32(len(kinds)))
	for i := range kinds {
		kind := kinds[(first+i)%len(kinds)]
		claims, err := r.engine.store.Claim(ctx, store.ClaimRequest{
			Owner: r.owner, Kind: kind, Limit: 1, LeaseMS: r.leaseMS,
		})
		if err != nil {
			return false, fmt.Errorf("claim %s: %w", kind, err)
		}
		if len(claims) == 0 {
			continue
		}
		claim := claims[0]
		if err := r.engine.store.Apply(ctx, claim.Lease, r.engine.apply); err != nil {
			var executionErr executionFailure
			if errors.As(err, &executionErr) && claim.Work.InstanceID != "" {
				if executionErr.revision == 0 {
					return false, fmt.Errorf("model failure has no expected revision: %w", err)
				}
				retryInput, encodeErr := encodeObject(map[string]string{"activationId": claim.Work.ActivationID})
				if encodeErr != nil {
					return false, encodeErr
				}
				err := r.engine.store.RaiseIncident(ctx, claim.Lease, store.IncidentRaise{
					ExpectedRevision: executionErr.revision, AtMS: r.engine.clock.Now().UnixMilli(),
					ElementID: executionErr.elementID, Code: executionErr.code, Message: executionErr.message,
					RetryInput: retryInput,
				})
				if err != nil {
					return false, fmt.Errorf("raise incident for work %q: %w", claim.Work.ID, err)
				}
				return true, nil
			}
			return false, fmt.Errorf("apply work %q: %w", claim.Work.ID, err)
		}
		return true, nil
	}
	return false, nil
}

// Run polls immediately and after each commit. It waits pollInterval when
// idle or after a conflict or lost lease, then makes a fresh claim. Other
// errors stop the loop for the caller to handle; any unfinished lease expires
// normally. Run returns ctx.Err() on cancellation.
func (r *Runner) Run(ctx context.Context, pollInterval time.Duration) error {
	if pollInterval <= 0 {
		return errors.New("poll interval must be positive")
	}
	for {
		worked, err := r.RunOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrLeaseLost) {
			return err
		}
		if worked {
			continue
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
