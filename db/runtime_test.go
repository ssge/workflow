package db_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ssge/workflow/db"
	"github.com/ssge/workflow/db/store"
	"github.com/ssge/workflow/internal/testdb"
	"gorm.io/gorm"
)

func TestRuntimeStore(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := os.Getenv("WORKFLOW_TEST_POSTGRES_DSN")
			if driver == "postgres" && dsn == "" {
				t.Skip("set WORKFLOW_TEST_POSTGRES_DSN to run PostgreSQL contracts")
			}
			for _, tc := range []struct {
				name string
				run  func(*testing.T, *runtimeHarness)
			}{
				{"S1_unit_of_work", testUnitOfWork},
				{"S1_inbox_replay", testInboxReplay},
				{"S1_publication", testPublication},
				{"S2_claim_and_fence", testClaimAndFence},
				{"S2_future_ready_time", testFutureReadyTime},
				{"catalog_reads", testCatalogReads},
				{"user_task_records", testUserTaskRecords},
				{"boundary_timer_work", testBoundaryTimerWork},
				{"fork_join_records", testForkJoinRecords},
				{"incident_block", testIncidentBlock},
			} {
				t.Run(tc.name, func(t *testing.T) {
					tc.run(t, newRuntimeHarness(t, driver, dsn))
				})
			}
		})
	}
}

type runtimeHarness struct {
	ctx                context.Context
	driver, connection string
	now                atomic.Int64
	database           *gorm.DB
	store              store.Store
}

func newRuntimeHarness(t *testing.T, driver, dsn string) *runtimeHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	h := &runtimeHarness{ctx: ctx, driver: driver}
	if driver == "sqlite" {
		h.connection = filepath.Join(t.TempDir(), "runtime.db")
	} else {
		h.connection = testdb.PostgresSchema(t, dsn)
	}
	h.reopen(t)
	t.Cleanup(func() {
		pool, err := h.database.DB()
		must(t, err)
		must(t, pool.Close())
	})
	must(t, db.Migrate(h.ctx, h.database))
	return h
}

func (h *runtimeHarness) reopen(t *testing.T) {
	t.Helper()
	if h.database != nil {
		pool, err := h.database.DB()
		must(t, err)
		must(t, pool.Close())
	}
	database, err := db.Open(h.driver, h.connection)
	must(t, err)
	h.database, h.store = database, db.NewRuntimeStore(database, h.now.Load)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

func publish(key string) store.Publication {
	return store.Publication{ProcessKey: key, ProfileRevision: "r1", Sources: `[]`, Plan: `{"end":"done"}`}
}

func seedInstance(t *testing.T, h *runtimeHarness) {
	t.Helper()
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		if _, err := tx.Publish(publish("process")); err != nil {
			return err
		}
		if _, err := tx.CreateInstanceAtHead(store.InstanceStart{
			ID: "instance", ProcessKey: "process", Variables: `{"n":0}`,
		}); err != nil {
			return err
		}
		return insertStep(tx, "first", "start")
	}))
}

func insertStep(tx store.Tx, visit, element string) error {
	if err := tx.InsertActivation(store.Activation{
		ID: visit, InstanceID: "instance", TokenID: "token", ElementID: element, State: store.ActivationReady,
	}); err != nil {
		return err
	}
	return tx.EnqueueStep(store.StepWork{ID: visit, InstanceID: "instance", ActivationID: visit})
}

func advance(tx store.Tx) error {
	if err := tx.ChangeInstance(store.InstanceChange{
		ID: "instance", ExpectedRevision: 1, Status: store.InstanceRunning, Variables: `{"n":1}`,
		Entry: store.JournalEntry{Kind: store.TransitionStep, AtMS: 5},
	}); err != nil {
		return err
	}
	if err := tx.SetActivationState("first", store.ActivationReady, store.ActivationConsumed); err != nil {
		return err
	}
	return insertStep(tx, "next", "done")
}

func checkInstance(t *testing.T, h *runtimeHarness, revision int64, variables store.Object) {
	t.Helper()
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		instance, err := tx.GetInstance("instance")
		must(t, err)
		if instance.Revision != revision || instance.Variables != variables ||
			instance.Ref != (store.DefinitionRef{ProcessKey: "process", Version: 1}) {
			t.Fatalf("instance = %+v, want revision %d with %s and original pin", instance, revision, variables)
		}
		initial, err := tx.GetTransition("instance", 1)
		must(t, err)
		if initial.Kind != store.TransitionStarted {
			t.Fatalf("initial transition = %+v", initial)
		}
		step, err := tx.GetTransition("instance", 2)
		if revision == 1 {
			wantError(t, err, store.ErrNotFound)
			_, err = tx.GetActivation("next")
			wantError(t, err, store.ErrNotFound)
		} else {
			must(t, err)
			if step.Kind != store.TransitionStep || step.AtMS != 5 {
				t.Fatalf("step transition = %+v", step)
			}
			next, err := tx.GetActivation("next")
			must(t, err)
			if next.State != store.ActivationReady {
				t.Fatalf("next activation = %+v", next)
			}
		}
		return nil
	}))
}

func testUnitOfWork(t *testing.T, h *runtimeHarness) {
	seedInstance(t, h)
	claim := claimOne(t, h, "runner", store.WorkStep)
	// Force the journal insert to fail after updating the instance row.
	// Even a callback that inadvertently drops the error must not commit.
	if err := h.store.Apply(h.ctx, claim.Lease, func(tx store.Tx, _ store.WorkItem) (*store.Outcome, error) {
		_ = tx.ChangeInstance(store.InstanceChange{
			ID: "instance", ExpectedRevision: 1, Status: store.InstanceRunning, Variables: `{"partial":true}`,
			Entry: store.JournalEntry{Kind: store.TransitionStarted},
		})
		return nil, nil
	}); err == nil {
		t.Fatal("committed an instance update without a journal entry")
	}
	rollback := errors.New("injected crash after writes")
	wantError(t, h.store.Apply(h.ctx, claim.Lease, func(tx store.Tx, _ store.WorkItem) (*store.Outcome, error) {
		if err := advance(tx); err != nil {
			return nil, err
		}
		return nil, rollback
	}), rollback)
	h.reopen(t)
	checkInstance(t, h, 1, `{"n":0}`)
	noClaims(t, h, store.WorkStep)

	must(t, h.store.Apply(h.ctx, claim.Lease, func(tx store.Tx, _ store.WorkItem) (*store.Outcome, error) {
		return nil, advance(tx)
	}))
	h.reopen(t)
	checkInstance(t, h, 2, `{"n":1}`)
	h.now.Store(claim.Lease.ExpiresAt)
	if got := claimOne(t, h, "another-runner", store.WorkStep); got.Work.ActivationID != "next" {
		t.Fatalf("claimed %+v, want committed successor", got)
	}
	noClaims(t, h, store.WorkStep)
	wantError(t, h.store.WithTransaction(h.ctx, advance), store.ErrConflict)
}

func admission(id, body string) store.Admission {
	return store.Admission{
		OperationID: id, WorkID: "command-" + id, Key: "shared-key",
		Command: store.Command{Kind: store.Start, Target: `{}`, Payload: store.Object(body)},
	}
}

func testInboxReplay(t *testing.T, h *runtimeHarness) {
	a := admission("a", `{"variables":{"n":1.234567890123456789012345678901234e+6144,"list":[-0,1]},"processKey":"process"}`)
	b := admission("b", `{"processKey":"process","variables":{"list":[0.0,1.0],"n":12.34567890123456789012345678901234e+6143}}`)
	// A successor insert that fails must not leave a pending operation.
	seed := "seed"
	must(t, h.database.Create(&db.Operation{OperationID: seed, IdempotencyKey: db.CommandKey(seed), Kind: "Start", Target: `{}`, Payload: `{}`}).Error)
	must(t, h.database.Create(&db.WorkItem{WorkID: "already-used", Kind: "COMMAND", OperationID: &seed, State: "DONE"}).Error)
	bad := a
	bad.WorkID = "already-used"
	if _, err := h.store.Admit(h.ctx, bad); err == nil {
		t.Fatal("admitted an operation without recoverable command work")
	}
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		_, err := tx.GetOperation(a.OperationID)
		wantError(t, err, store.ErrNotFound)
		return nil
	}))

	one, two := raceCalls(
		func() (store.Operation, error) { return h.store.Admit(h.ctx, a) },
		func() (store.Operation, error) { return h.store.Admit(h.ctx, b) },
	)
	must(t, one.err)
	must(t, two.err)
	if one.value.ID != two.value.ID || one.value.Status != store.OperationPending || two.value.Status != store.OperationPending {
		t.Fatalf("different receipts: %+v, %+v", one.value, two.value)
	}
	bad = b
	bad.Command.Payload = `{"processKey":"process","variables":{"list":[0,1],"n":12.34567890123456789012345678901235e+6143}}`
	_, err := h.store.Admit(h.ctx, bad)
	wantError(t, err, store.ErrIdempotencyConflict)
	bad.Command.Payload = `{"processKey":"process","variables":{"list":[1,0],"n":12.34567890123456789012345678901234e+6143}}`
	_, err = h.store.Admit(h.ctx, bad)
	wantError(t, err, store.ErrIdempotencyConflict)
	bad = b
	bad.Command.Kind = store.Deploy
	_, err = h.store.Admit(h.ctx, bad)
	wantError(t, err, store.ErrIdempotencyConflict)
	bad = b
	bad.Command.Target = `{"pathId":"different"}`
	_, err = h.store.Admit(h.ctx, bad)
	wantError(t, err, store.ErrIdempotencyConflict)

	h.reopen(t)
	claim := claimOne(t, h, "runner", store.WorkCommand)
	if claim.Work.OperationID != one.value.ID {
		t.Fatalf("claimed wrong command: %+v", claim)
	}
	noClaims(t, h, store.WorkCommand)
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		_, err := tx.Publish(publish("process"))
		return err
	}))
	result := store.Object(`{"instanceId":"created"}`)
	finish := func(tx store.Tx, work store.WorkItem) (*store.Outcome, error) {
		op, err := tx.GetOperation(work.OperationID)
		must(t, err)
		if op.Status != store.OperationPending {
			t.Fatalf("operation before commit = %+v", op)
		}
		_, err = tx.CreateInstanceAtHead(store.InstanceStart{ID: "created", ProcessKey: "process", Variables: `{}`})
		if err != nil {
			return nil, err
		}
		return &store.Outcome{Result: &result}, nil
	}
	wantError(t, h.store.Apply(h.ctx, claim.Lease, func(tx store.Tx, work store.WorkItem) (*store.Outcome, error) {
		outcome, err := finish(tx, work)
		h.now.Store(claim.Lease.ExpiresAt)
		return outcome, err
	}), store.ErrLeaseLost)
	h.reopen(t)
	pending, err := h.store.Admit(h.ctx, a)
	must(t, err)
	if pending.ID != one.value.ID || pending.Status != store.OperationPending || pending.Result != nil {
		t.Fatalf("expired apply changed the receipt: %+v", pending)
	}
	reclaim := claimOne(t, h, "runner", store.WorkCommand)
	must(t, h.store.Apply(h.ctx, reclaim.Lease, finish))
	h.reopen(t)
	replay, err := h.store.Admit(h.ctx, a)
	must(t, err)
	if replay.ID != one.value.ID || replay.Status != store.OperationSucceeded || replay.Result == nil || *replay.Result != result {
		t.Fatalf("terminal replay = %+v", replay)
	}
	noClaims(t, h, store.WorkCommand)
}

func testPublication(t *testing.T, h *runtimeHarness) {
	ctx := h.ctx
	doPublish := func(input store.Publication) (ref store.DefinitionRef, err error) {
		err = h.store.WithTransaction(ctx, func(tx store.Tx) error {
			ref, err = tx.Publish(input)
			return err
		})
		return ref, err
	}
	first, second := raceCalls(
		func() (store.DefinitionRef, error) { return doPublish(publish("process")) },
		func() (store.DefinitionRef, error) {
			p := publish("process")
			p.Plan = `{"end":"other"}`
			return doPublish(p)
		},
	)
	must(t, first.err)
	must(t, second.err)
	if a, b := first.value.Version, second.value.Version; !(a == 1 && b == 2 || a == 2 && b == 1) {
		t.Fatalf("concurrent versions = %d and %d, want 1 and 2", a, b)
	}
	other, err := doPublish(publish("other"))
	must(t, err)
	if other.Version != 1 {
		t.Fatalf("version is not scoped by key: %+v", other)
	}
	must(t, h.store.WithTransaction(ctx, func(tx store.Tx) error {
		pinned, err := tx.CreateInstanceAtHead(store.InstanceStart{ID: "instance", ProcessKey: "process", Variables: `{}`})
		must(t, err)
		if pinned.Ref.Version != 2 {
			t.Fatalf("new instance has wrong pin: %+v", pinned)
		}
		return nil
	}))
	rollback := errors.New("publication aborted")
	wantError(t, h.store.WithTransaction(ctx, func(tx store.Tx) error {
		_, err := tx.Publish(publish("process"))
		must(t, err)
		return rollback
	}), rollback)
	h.reopen(t)
	must(t, h.store.WithTransaction(ctx, func(tx store.Tx) error {
		_, err := tx.GetDefinition(store.DefinitionRef{ProcessKey: "process", Version: 3})
		wantError(t, err, store.ErrNotFound)
		i, err := tx.CreateInstanceAtHead(store.InstanceStart{ID: "before", ProcessKey: "process", Variables: `{}`})
		must(t, err)
		if i.Ref.Version != 2 {
			t.Fatalf("aborted publication moved head: %+v", i)
		}
		return nil
	}))
	third, err := doPublish(publish("process"))
	must(t, err)
	if third.Version != 3 {
		t.Fatalf("rollback left a version gap: %+v", third)
	}
	must(t, h.store.WithTransaction(ctx, func(tx store.Tx) error {
		old, err := tx.GetDefinition(second.value)
		must(t, err)
		if old.Plan != `{"end":"other"}` {
			t.Fatalf("old definition changed: %+v", old)
		}
		i, err := tx.GetInstance("instance")
		must(t, err)
		j, err := tx.CreateInstanceAtHead(store.InstanceStart{ID: "after", ProcessKey: "process", Variables: `{}`})
		must(t, err)
		if i.Ref.Version != 2 || j.Ref != third {
			t.Fatalf("incorrect old/new pins: %+v / %+v", i.Ref, j.Ref)
		}
		return nil
	}))
}

func testCatalogReads(t *testing.T, h *runtimeHarness) {
	v1 := store.DefinitionRef{ProcessKey: "process", Version: 1}
	v2 := store.DefinitionRef{ProcessKey: "process", Version: 2}
	v3 := store.DefinitionRef{ProcessKey: "process", Version: 3}
	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		publishAt := func(key string, at int64) {
			in := publish(key)
			in.CreatedAt = at
			_, err := tx.Publish(in)
			must(t, err)
		}
		start := func(key, id string) {
			_, err := tx.CreateInstanceAtHead(store.InstanceStart{ID: id, ProcessKey: key, Variables: `{}`})
			must(t, err)
		}
		publishAt("process", 20)
		start("process", "running")
		start("process", "incident")
		start("process", "completed")
		endedAt := int64(21)
		must(t, tx.ChangeInstance(store.InstanceChange{
			ID: "completed", ExpectedRevision: 1, Status: store.InstanceCompleted, Variables: `{}`, EndedAt: &endedAt,
			Entry: store.JournalEntry{Kind: store.TransitionCompleted, AtMS: endedAt},
		}))
		must(t, tx.ChangeInstance(store.InstanceChange{
			ID: "incident", ExpectedRevision: 1, Status: store.InstanceIncident, Variables: `{}`,
			Entry: store.JournalEntry{Kind: store.TransitionIncidentRaised, AtMS: 21},
		}))
		publishAt("process", 22)
		start("process", "new-version")
		publishAt("process", 22)
		publishAt("other", 22)
		start("other", "other-process")
		head, err := tx.GetHead("process")
		must(t, err)
		if head != v3 {
			t.Fatalf("head in publishing transaction = %+v, want %+v", head, v3)
		}
		count, err := tx.CountInstancePins(v1, store.InstanceIncident)
		must(t, err)
		if count != 1 {
			t.Fatalf("incident count in writing transaction = %d, want 1", count)
		}
		return nil
	}))
	h.reopen(t)

	must(t, h.store.WithTransaction(h.ctx, func(tx store.Tx) error {
		page, err := tx.ListDefinitions("process", 0, 2)
		must(t, err)
		if want := []store.DefinitionVersion{{Ref: v1, CreatedAt: 20}, {Ref: v2, CreatedAt: 22}}; !slices.Equal(page, want) {
			t.Fatalf("first page = %+v, want %+v", page, want)
		}
		page, err = tx.ListDefinitions("process", v2.Version, 2)
		must(t, err)
		if want := []store.DefinitionVersion{{Ref: v3, CreatedAt: 22}}; !slices.Equal(page, want) {
			t.Fatalf("next page = %+v, want %+v", page, want)
		}
		_, err = tx.GetHead("missing")
		wantError(t, err, store.ErrNotFound)
		page, err = tx.ListDefinitions("missing", 0, 2)
		must(t, err)
		if len(page) != 0 {
			t.Fatalf("unknown process returned versions: %+v", page)
		}
		head, err := tx.GetHead("process")
		must(t, err)
		if head != v3 {
			t.Fatalf("reopened head = %+v, want %+v", head, v3)
		}
		pins, err := tx.ListInstancePins(v1)
		must(t, err)
		wantPins := []store.InstancePin{
			{InstanceID: "completed", Status: store.InstanceCompleted},
			{InstanceID: "incident", Status: store.InstanceIncident},
			{InstanceID: "running", Status: store.InstanceRunning},
		}
		if !slices.Equal(pins, wantPins) {
			t.Fatalf("v1 pins = %+v, want %+v", pins, wantPins)
		}
		total, err := tx.CountInstancePins(v1)
		must(t, err)
		active, err := tx.CountInstancePins(v1, store.InstanceRunning, store.InstanceIncident)
		must(t, err)
		emptyPins, err := tx.ListInstancePins(v3)
		must(t, err)
		emptyCount, err := tx.CountInstancePins(v3, store.InstanceRunning, store.InstanceIncident)
		must(t, err)
		if total != 3 || active != 2 || len(emptyPins) != 0 || emptyCount != 0 {
			t.Fatalf("v1 total/active = %d/%d, v3 pins/active = %+v/%d, want 3/2 and empty/0", total, active, emptyPins, emptyCount)
		}
		return nil
	}))
}

func claimRequest(owner string, kind store.WorkKind) store.ClaimRequest {
	return store.ClaimRequest{Owner: owner, Kind: kind, Limit: 1, LeaseMS: 10}
}

func claimOne(t *testing.T, h *runtimeHarness, owner string, kind store.WorkKind) store.Claim {
	t.Helper()
	got, err := h.store.Claim(h.ctx, claimRequest(owner, kind))
	must(t, err)
	if len(got) != 1 {
		t.Fatalf("expected one %s claim, got %+v", kind, got)
	}
	return got[0]
}

func noClaims(t *testing.T, h *runtimeHarness, kind store.WorkKind) {
	t.Helper()
	got, err := h.store.Claim(h.ctx, claimRequest("probe", kind))
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("expected no %s claims, got %+v", kind, got)
	}
}

func testClaimAndFence(t *testing.T, h *runtimeHarness) {
	seedInstance(t, h)
	a, b := raceCalls(
		func() ([]store.Claim, error) { return h.store.Claim(h.ctx, claimRequest("a", store.WorkStep)) },
		func() ([]store.Claim, error) { return h.store.Claim(h.ctx, claimRequest("b", store.WorkStep)) },
	)
	must(t, a.err)
	must(t, b.err)
	if len(a.value)+len(b.value) != 1 {
		t.Fatalf("two claimants got %+v and %+v, want one live lease", a.value, b.value)
	}
	initial := append(a.value, b.value...)[0].Lease
	if initial.Fence != 1 || initial.ExpiresAt != 10 {
		t.Fatalf("initial lease = %+v", initial)
	}
	h.now.Store(9)
	noClaims(t, h, store.WorkStep)

	// Check the lease again AFTER all effects, not only before entering Apply.
	wantError(t, h.store.Apply(h.ctx, initial, func(tx store.Tx, _ store.WorkItem) (*store.Outcome, error) {
		err := advance(tx)
		h.now.Store(10)
		return nil, err
	}), store.ErrLeaseLost)
	h.reopen(t)
	checkInstance(t, h, 1, `{"n":0}`)
	reclaimed := claimOne(t, h, initial.Owner, store.WorkStep).Lease
	if reclaimed.WorkID != initial.WorkID || reclaimed.Fence != initial.Fence+1 || reclaimed.ExpiresAt != 20 {
		t.Fatalf("reclaim = %+v, previous = %+v", reclaimed, initial)
	}

	notCalled := func(store.Tx, store.WorkItem) (*store.Outcome, error) {
		t.Fatal("stale claim entered transaction callback")
		return nil, nil
	}
	wantError(t, h.store.Apply(h.ctx, initial, notCalled), store.ErrLeaseLost)
	forged := reclaimed
	forged.Owner = "not-the-owner"
	wantError(t, h.store.Apply(h.ctx, forged, notCalled), store.ErrLeaseLost)
	must(t, h.store.Apply(h.ctx, reclaimed, func(tx store.Tx, _ store.WorkItem) (*store.Outcome, error) {
		return nil, advance(tx)
	}))
	wantError(t, h.store.Apply(h.ctx, reclaimed, notCalled), store.ErrLeaseLost)
	checkInstance(t, h, 2, `{"n":1}`)
}

func testFutureReadyTime(t *testing.T, h *runtimeHarness) {
	seedInstance(t, h)
	// Timer scheduling is not an A2 port. A fixture uses the existing schema.
	must(t, h.database.Create(&db.UserTask{TaskID: "task", ActivationID: "first", FormID: "form", Components: `[]`}).Error)
	must(t, h.database.Create(&db.BoundaryTimer{TimerID: "timer", ActivationID: "first", ElementID: "alarm", DueAt: 50}).Error)
	instance, activation := "instance", "first"
	must(t, h.database.Create(&db.WorkItem{WorkID: "alarm", Kind: "TIMER", InstanceID: &instance, ActivationID: &activation, ReadyAt: 50}).Error)
	h.now.Store(49)
	noClaims(t, h, store.WorkTimer)
	h.now.Store(50)
	if got := claimOne(t, h, "runner", store.WorkTimer); got.Work.ID != "alarm" {
		t.Fatalf("due work = %+v, want alarm", got)
	}
}

type result[T any] struct {
	value T
	err   error
}

func raceCalls[T any](a, b func() (T, error)) (result[T], result[T]) {
	start, one, two := make(chan struct{}), make(chan result[T], 1), make(chan result[T], 1)
	run := func(fn func() (T, error), ch chan<- result[T]) {
		<-start
		v, err := fn()
		ch <- result[T]{v, err}
	}
	go run(a, one)
	go run(b, two)
	close(start)
	return <-one, <-two
}
