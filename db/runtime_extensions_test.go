package db_test

import (
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/ssge/workflow/db"
	"github.com/ssge/workflow/db/store"
)

func testUserTaskRecords(t *testing.T, h *runtimeHarness) {
	seedInstance(t, h)
	task := store.NewUserTask{ID: "task", ActivationID: "first", FormID: "form",
		Components: `[{"label":"saved view"}]`, CreatedAt: 4}
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error { return tx.InsertUserTask(task) }))
	h.reopen(t)
	wantError(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		duplicate := task
		duplicate.ID = "duplicate"
		return tx.InsertUserTask(duplicate)
	}), store.ErrConflict)
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		got, err := tx.GetUserTaskByActivation("first")
		must(t, err)
		if got.NewUserTask != task || got.Status != store.TaskOpen || got.ClosedAt != nil {
			t.Fatalf("open task = %+v", got)
		}
		return tx.CloseUserTask(task.ID, store.TaskClosure{Status: store.TaskCompleted, ClosedAt: 6})
	}))
	h.reopen(t)
	reason := store.TaskCancelledByTimer
	wantError(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		return tx.CloseUserTask(task.ID, store.TaskClosure{Status: store.TaskCancelled, ClosedAt: 7, Reason: &reason})
	}), store.ErrConflict)
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		got, err := tx.GetUserTask(task.ID)
		must(t, err)
		if got.NewUserTask != task || got.Status != store.TaskCompleted ||
			got.ClosedAt == nil || *got.ClosedAt != 6 || got.Reason != nil {
			t.Fatalf("terminal task was not retained unchanged: %+v", got)
		}
		return nil
	}))
}

func testBoundaryTimerWork(t *testing.T, h *runtimeHarness) {
	seedInstance(t, h)
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		must(t, tx.InsertActivation(store.Activation{
			ID: "second", InstanceID: "instance", TokenID: "second-token", ElementID: "form", State: store.ActivationWaiting,
		}))
		for i, activation := range []string{"first", "second"} {
			must(t, tx.InsertUserTask(store.NewUserTask{ID: activation, ActivationID: activation, FormID: "form", Components: `[]`}))
			must(t, tx.InsertBoundaryTimer(store.NewBoundaryTimer{
				ID: "timer-" + activation, ActivationID: activation, ElementID: "alarm", DueAt: 50 + int64(i)*10,
			}))
			must(t, tx.EnqueueTimerWork("work-"+activation, activation))
		}
		return nil
	}))
	h.reopen(t)
	h.now.Store(49)
	noClaims(t, h, store.WorkTimer)
	h.now.Store(50)
	firing := claimOne(t, h, "runner", store.WorkTimer)
	if firing.Work.ReadyAt != 50 || firing.Work.ActivationID != "first" || firing.Work.InstanceID != "instance" {
		t.Fatalf("timer work did not retain its activation or deadline: %+v", firing.Work)
	}
	must(t, h.store.Apply(h.ctx, firing.Lease, func(tx store.Tx, work store.WorkItem) (*store.Outcome, error) {
		reason := store.TaskCancelledByTimer
		must(t, tx.CloseUserTask("first", store.TaskClosure{Status: store.TaskCancelled, Reason: &reason, ClosedAt: 50}))
		return nil, tx.TransitionBoundaryTimer(work.ActivationID, store.TimerFired)
	}))
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		must(t, tx.CloseUserTask("second", store.TaskClosure{Status: store.TaskCompleted, ClosedAt: 51}))
		return tx.TransitionBoundaryTimer("second", store.TimerCancelled)
	}))
	h.reopen(t)
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		timer, err := tx.GetBoundaryTimer("first")
		must(t, err)
		task, err := tx.GetUserTask("first")
		must(t, err)
		if timer.State != store.TimerFired || task.Status != store.TaskCancelled ||
			task.Reason == nil || *task.Reason != store.TaskCancelledByTimer {
			t.Fatalf("firing and task closure were not stored together: %+v / %+v", timer, task)
		}
		return nil
	}))
	h.now.Store(59)
	noClaims(t, h, store.WorkTimer)
	h.now.Store(60)
	stale := claimOne(t, h, "runner", store.WorkTimer)
	if stale.Work.ActivationID != "second" {
		t.Fatalf("claimed %+v, want the retained cancelled wake-up", stale.Work)
	}
	wantError(t, h.store.Apply(h.ctx, stale.Lease, func(tx store.Tx, work store.WorkItem) (*store.Outcome, error) {
		return nil, tx.TransitionBoundaryTimer(work.ActivationID, store.TimerFired)
	}), store.ErrConflict)
	must(t, h.store.Apply(h.ctx, stale.Lease, func(tx store.Tx, work store.WorkItem) (*store.Outcome, error) {
		timer, err := tx.GetBoundaryTimer(work.ActivationID)
		must(t, err)
		if timer.State != store.TimerCancelled || timer.DueAt != 60 {
			t.Fatalf("stale wake-up lost the original cancellation or deadline: %+v", timer)
		}
		return nil, nil // Consume the wake-up with no effects.
	}))
	h.now.Store(100)
	noClaims(t, h, store.WorkTimer)
}

func testForkJoinRecords(t *testing.T, h *runtimeHarness) {
	seedInstance(t, h)
	fork := store.NewFork{ID: "fork", InstanceID: "instance", SplitActivationID: "first", JoinElementID: "join"}
	left := store.JoinArrival{ForkID: fork.ID, ActivationID: "left", BranchFlowID: "left", IncomingFlowID: "from-left"}
	right := store.JoinArrival{ForkID: fork.ID, ActivationID: "right", BranchFlowID: "right", IncomingFlowID: "from-right"}
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		must(t, tx.CreateFork(fork))
		for _, branch := range []string{"left", "right", "late"} {
			must(t, tx.InsertActivation(store.Activation{
				ID: branch, InstanceID: fork.InstanceID, TokenID: branch, ElementID: "join", State: store.ActivationWaiting,
			}))
			must(t, tx.InsertBranchActivation(store.BranchActivation{
				ActivationID: branch, InstanceID: fork.InstanceID, ForkID: fork.ID, BranchFlowID: branch,
			}))
		}
		return tx.InsertJoinArrival(left)
	}))
	h.reopen(t)
	dupIncoming := right
	dupIncoming.IncomingFlowID = left.IncomingFlowID
	dupBranch := left
	dupBranch.IncomingFlowID = right.IncomingFlowID
	for _, invalid := range []store.JoinArrival{dupIncoming, dupBranch} {
		wantError(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
			return tx.InsertJoinArrival(invalid)
		}), store.ErrConflict)
	}
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		branch, err := tx.GetBranchActivation(right.ActivationID)
		must(t, err)
		if branch.ForkID != fork.ID || branch.BranchFlowID != right.BranchFlowID {
			t.Fatalf("branch record = %+v", branch)
		}
		must(t, tx.InsertJoinArrival(right))
		arrivals, err := tx.ListJoinArrivals(fork.ID)
		must(t, err)
		if !slices.Equal(arrivals, []store.JoinArrival{left, right}) {
			t.Fatalf("arrivals in closing transaction = %+v", arrivals)
		}
		return tx.CloseFork(fork.ID)
	}))
	h.reopen(t)
	wantError(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		return tx.InsertJoinArrival(store.JoinArrival{
			ForkID: fork.ID, ActivationID: "late", BranchFlowID: "late", IncomingFlowID: "from-late",
		})
	}), store.ErrConflict)
	wantError(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error { return tx.CloseFork(fork.ID) }), store.ErrConflict)
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		got, err := tx.GetFork(fork.ID)
		must(t, err)
		arrivals, err := tx.ListJoinArrivals(fork.ID)
		must(t, err)
		if got.NewFork != fork || got.State != store.ForkClosed || !slices.Equal(arrivals, []store.JoinArrival{left, right}) {
			t.Fatalf("reopened fork = %+v, arrivals = %+v", got, arrivals)
		}
		return nil
	}))
}

func testIncidentBlock(t *testing.T, h *runtimeHarness) {
	seedInstance(t, h)
	first := claimOne(t, h, "first-runner", store.WorkStep)
	stepError := errors.New("injected step failure")
	wantError(t, h.store.Apply(h.ctx, first.Lease, func(tx store.Tx, _ store.WorkItem) (*store.Outcome, error) {
		must(t, advance(tx))
		return nil, stepError
	}), stepError)
	incident := store.IncidentRaise{
		ExpectedRevision: 1, ElementID: "start", Code: "EVALUATION_ERROR", Message: "bad input",
		RetryInput: `{"value":"input"}`, AtMS: 20,
	}
	staleRevision := incident
	staleRevision.ExpectedRevision++
	wantError(t, h.store.RaiseIncident(h.ctx, first.Lease, staleRevision), store.ErrConflict)

	// Advance time only at the final fence check, after incident writes.
	var samples atomic.Int64
	expiring := db.NewRuntimeStore(h.database, func() int64 {
		if samples.Add(1) == 1 {
			return first.Lease.ExpiresAt - 1
		}
		return first.Lease.ExpiresAt
	})
	wantError(t, expiring.RaiseIncident(h.ctx, first.Lease, incident), store.ErrLeaseLost)
	h.reopen(t)
	checkInstance(t, h, 1, `{"n":0}`)
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		_, err := tx.GetIncident("instance", 2)
		wantError(t, err, store.ErrNotFound)
		return nil
	}))
	h.now.Store(first.Lease.ExpiresAt)
	current := claimOne(t, h, "replacement", store.WorkStep)
	wantError(t, h.store.RaiseIncident(h.ctx, first.Lease, incident), store.ErrLeaseLost)
	must(t, h.store.RaiseIncident(h.ctx, current.Lease, incident))
	h.reopen(t)
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		i, err := tx.GetInstance("instance")
		must(t, err)
		entry, err := tx.GetTransition("instance", 2)
		must(t, err)
		got, err := tx.GetIncident("instance", 2)
		must(t, err)
		if i.Status != store.InstanceIncident || i.Revision != 2 || i.Variables != `{"n":0}` ||
			entry.Kind != store.TransitionIncidentRaised || entry.AtMS != incident.AtMS ||
			got.WorkID != current.Work.ID || got.Code != incident.Code || got.ElementID != incident.ElementID ||
			got.Message != incident.Message || got.RetryInput != incident.RetryInput {
			t.Fatalf("incident commit = %+v / %+v / %+v", i, entry, got)
		}
		return nil
	}))
	var work db.WorkItem
	must(t, h.database.Take(&work, "work_id = ?", current.Work.ID).Error)
	if work.State != "BLOCKED" || work.Owner != nil || work.LeaseUntil != nil || work.Fence != current.Lease.Fence {
		t.Fatalf("failed work was not durably blocked with its fence retained: %+v", work)
	}
	h.now.Store(100)
	noClaims(t, h, store.WorkStep)
}
