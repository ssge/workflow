package engine_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ssge/workflow/db"
	"github.com/ssge/workflow/db/store"
	"github.com/ssge/workflow/internal/application"
	"github.com/ssge/workflow/internal/clock"
	"github.com/ssge/workflow/internal/engine"
	"github.com/ssge/workflow/internal/model"
	"github.com/ssge/workflow/internal/testdb"
)

func TestEngine(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := os.Getenv("WORKFLOW_TEST_POSTGRES_DSN")
			if driver == "postgres" && dsn == "" {
				t.Skip("set WORKFLOW_TEST_POSTGRES_DSN to run PostgreSQL contracts")
			}
			for _, tc := range []struct {
				name string
				test func(*testing.T, *harness)
			}{
				{"N1_durable_admission_and_replay", testAdmission},
				{"N2_leased_step_handoff", testHandoff},
				{"N2_fair_polling", testFairPolling},
				{"N5_publication_pinning_and_drain", testVersions},
				{"C2_pinned_form_and_decision_resources", testPinnedResources},
				{"B3_task_submit_and_query", testTaskSubmission},
				{"B4_boundary_timer", testBoundaryTimer},
				{"C1_parallel_join", testParallelJoin},
				{"N4_parallel_region_reentry", testParallelReentry},
				{"N4_last_arrival_before_commit", func(t *testing.T, h *harness) { testLastArrivalRecovery(t, h, true) }},
				{"N4_last_arrival_after_commit", func(t *testing.T, h *harness) { testLastArrivalRecovery(t, h, false) }},
				{"N2_R1_fixture_P1_immediate_exit", testFixtureImmediateExit},
				{"N2_R1_fixture_P2_first_match", testFixtureFirstMatch},
				{"N2_R1_fixture_P3_maximum_loop", testFixtureMaximumLoop},
				{"P4_R1_fixture_interrupting_timer", testFixtureTimer},
				{"N3_incident_freeze", testIncidentFreeze},
				{"N6_incident_isolation_after_restart", testIncidentIsolationAfterRestart},
				{"N3_timer_completion_race", testTimerCompletionRace},
			} {
				t.Run(tc.name, func(t *testing.T) {
					tc.test(t, newHarness(t, driver, dsn))
				})
			}
		})
	}
}

const lease = time.Second

type harness struct {
	ctx                context.Context
	clock              *clock.Fake
	driver, connection string
}

func newHarness(t *testing.T, driver, dsn string) *harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	h := &harness{ctx: ctx, clock: clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), driver: driver}
	if driver == "sqlite" {
		h.connection = filepath.Join(t.TempDir(), "engine.db")
	} else {
		h.connection = testdb.PostgresSchema(t, dsn)
	}
	database, err := db.Open(driver, h.connection)
	must(t, err)
	pool, err := database.DB()
	must(t, err)
	defer pool.Close()
	must(t, db.Migrate(ctx, database))
	return h
}

func (h *harness) connect(t *testing.T) (store.Store, *sql.DB) {
	t.Helper()
	database, err := db.Open(h.driver, h.connection)
	must(t, err)
	pool, err := database.DB()
	must(t, err)
	t.Cleanup(func() { must(t, pool.Close()) })
	return db.NewRuntimeStore(database, func() int64 { return h.clock.Now().UnixMilli() }), pool
}

func (h *harness) runner(t *testing.T, s store.Store, owner string) *engine.Runner {
	t.Helper()
	r, err := engine.New(s, h.clock).NewRunner(owner, lease)
	must(t, err)
	return r
}

func testAdmission(t *testing.T, h *harness) {
	s, conn := h.connect(t)
	e := engine.New(s, h.clock)
	cmd := deploy(t, "old_end")
	receipt := accept(t, h.ctx, e, "one", cmd)
	if receipt.Status != "PENDING" || receipt.OperationID == "" {
		t.Fatalf("receipt = %+v", receipt)
	}
	must(t, conn.Close())
	s, _ = h.connect(t)
	e = engine.New(s, h.clock)
	pending, err := e.GetOperationByKey(h.ctx, "one")
	must(t, err)
	if pending.OperationID != receipt.OperationID || pending.Status != "PENDING" || pending.Result != nil || pending.Error != nil {
		t.Fatalf("durable pending operation = %+v", pending)
	}
	page, err := e.ListDefinitionVersions(h.ctx, "p", 0, "")
	must(t, err)
	if page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("admission published before the runner: %+v", page)
	}
	if replay := accept(t, h.ctx, e, "one", cmd); replay != receipt {
		t.Fatalf("pending replay = %+v, want %+v", replay, receipt)
	}
	_, err = e.Accept(h.ctx, "one", deploy(t, "different_end"))
	wantError(t, err, 409, "IDEMPOTENCY_CONFLICT")
	_, err = engine.New(unavailableAdmission{s}, h.clock).Accept(h.ctx, "unconfirmed", cmd)
	wantError(t, err, 503, "UNAVAILABLE")
	_, err = e.GetOperationByKey(h.ctx, "unconfirmed")
	wantError(t, err, 404, "NOT_FOUND")
	_, err = e.Accept(h.ctx, "invalid-request", start(`{"processKey":"p","variables":{"n":1,"n":2}}`))
	wantError(t, err, 400, "INVALID_REQUEST")

	h.expireApply(t, s)
	page, err = e.ListDefinitionVersions(h.ctx, "p", 0, "")
	must(t, err)
	if len(page.Items) != 0 || accept(t, h.ctx, e, "one", cmd) != receipt {
		t.Fatal("publication or receipt success escaped the expired lease")
	}
	r := h.runner(t, s, "A")
	runOnce(t, h.ctx, r, true)
	published := success[publication](t, h.ctx, e, receipt)
	if published != (publication{"p", 1}) {
		t.Fatalf("publication = %+v", published)
	}
	if replay := accept(t, h.ctx, e, "one", cmd); replay.OperationID != receipt.OperationID || replay.Status != "SUCCEEDED" {
		t.Fatalf("terminal replay = %+v", replay)
	}
	runOnce(t, h.ctx, r, false)
}

func testHandoff(t *testing.T, h *harness) {
	s, _ := h.connect(t)
	e := engine.New(s, h.clock)
	rA := h.runner(t, s, "A")
	accept(t, h.ctx, e, "publish", deploy(t, "finish"))
	runOnce(t, h.ctx, rA, true)
	receipt := accept(t, h.ctx, e, "start", start(`{"processKey":"p","variables":{"exact":9007199254740993.1250,"val":[1e-2,null]}}`))
	h.expireApply(t, s)
	if replay := accept(t, h.ctx, e, "start", start(`{"variables":{"val":[0.010,null],"exact":9007199254740993.125},"processKey":"p"}`)); replay != receipt {
		t.Fatalf("exact numeric replay = %+v, want %+v", replay, receipt)
	}
	must(t, s.WithTransaction(h.ctx, func(tx store.Tx) error {
		count, err := tx.CountInstancePins(store.DefinitionRef{ProcessKey: "p", Version: 1})
		if err == nil && count != 0 {
			return fmt.Errorf("start effects escaped expired lease: %d pins", count)
		}
		return err
	}))
	runOnce(t, h.ctx, rA, true)
	created := success[started](t, h.ctx, e, receipt)
	checkInstance(t, h.ctx, e, created.ID, 1, "RUNNING")

	// Advance the lease clock after the engine has prepared the STEP effects,
	// but before Apply's final lease check. The entire attempt must roll back.
	h.expireApply(t, s)
	checkInstance(t, h.ctx, e, created.ID, 1, "RUNNING")
	s2, _ := h.connect(t)
	runOnce(t, h.ctx, h.runner(t, s2, "B"), true)
	checkInstance(t, h.ctx, e, created.ID, 2, "RUNNING")

	h.clock.Advance(time.Millisecond)
	rC := h.runner(t, s, "C")
	runOnce(t, h.ctx, rC, true)
	finished := checkInstance(t, h.ctx, e, created.ID, 3, "COMPLETED")
	if finished.Version != 1 || string(finished.Variables) != `{"exact":9007199254740993.125,"val":[0.01,null]}` ||
		finished.EndedAt == nil || !finished.EndedAt.Equal(h.clock.Now()) {
		t.Fatalf("finished instance = %+v, vars=%s", finished, finished.Variables)
	}
	checkJournal(t, h.ctx, s2, created.ID, "finish")
	runOnce(t, h.ctx, rA, false)
}

func testFairPolling(t *testing.T, h *harness) {
	s, _ := h.connect(t)
	e := engine.New(s, h.clock)
	r := h.runner(t, s, "A")
	accept(t, h.ctx, e, "publish", deploy(t, "finish"))
	runOnce(t, h.ctx, r, true)
	receipt := accept(t, h.ctx, e, "start", start(`{"processKey":"p"}`))
	runOnce(t, h.ctx, r, true)
	created := success[started](t, h.ctx, e, receipt)
	if replay := accept(t, h.ctx, e, "start", start(`{"processKey":"p","variables":{}}`)); replay.OperationID != receipt.OperationID || replay.Status != "SUCCEEDED" {
		t.Fatalf("default variables did not replay: %+v", replay)
	}
	pending := accept(t, h.ctx, e, "task", application.Command{
		Kind: application.CompleteTask, Target: map[string]string{"taskId": "missing"}, Payload: json.RawMessage(`{"fields":{}}`),
	})
	h.clock.Advance(time.Millisecond)
	backlog := accept(t, h.ctx, e, "backlog", start(`{"processKey":"missing"}`))
	r = h.runner(t, s, "B")
	runOnce(t, h.ctx, r, true)
	runOnce(t, h.ctx, r, true)
	failed(t, h.ctx, e, pending, "NOT_FOUND")
	checkInstance(t, h.ctx, e, created.ID, 2, "RUNNING")
	op, err := e.GetOperation(h.ctx, backlog.OperationID)
	must(t, err)
	if op.Status != "PENDING" {
		t.Fatalf("runner preferred a queued command over the ready step: %+v", op)
	}

	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, time.Hour) }()
	// Start a poller with a long interval and ensure it processes work before
	// the initial idle delay by observing the commit through the same port.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		ins, err := e.GetInstance(h.ctx, created.ID)
		must(t, err)
		if ins.Status == "COMPLETED" {
			break
		}
		select {
		case <-h.ctx.Done():
			t.Fatal("poller did not complete the ready step")
		case err := <-done:
			t.Fatalf("poller exited early: %v", err)
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("poller cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("poller ignored cancellation")
	}
}

func testVersions(t *testing.T, h *harness) {
	s1, _ := h.connect(t)
	s2, _ := h.connect(t)
	e := engine.New(s1, h.clock)
	a, b := h.runner(t, s1, "A"), h.runner(t, s2, "B")
	noHead := accept(t, h.ctx, e, "missing", start(`{"processKey":"missing"}`))
	runOnce(t, h.ctx, a, true)
	failed(t, h.ctx, e, noHead, "NOT_FOUND")
	pub1 := accept(t, h.ctx, e, "first", deploy(t, "old_end"))
	pub2 := accept(t, h.ctx, e, "second", deploy(t, "old_end"))
	gate := make(chan struct{})
	results := make(chan error, 2)
	for _, r := range []*engine.Runner{a, b} {
		go func() {
			<-gate
			done, err := r.RunOnce(h.ctx)
			if err == nil && !done {
				err = errors.New("publication work was not processed")
			}
			results <- err
		}()
	}
	close(gate)
	must(t, <-results)
	must(t, <-results)
	versions := []int64{
		success[publication](t, h.ctx, e, pub1).Version,
		success[publication](t, h.ctx, e, pub2).Version,
	}
	slices.Sort(versions)
	if !slices.Equal(versions, []int64{1, 2}) {
		t.Fatalf("concurrent versions = %v", versions)
	}
	invalid := accept(t, h.ctx, e, "invalid", deploySource(t, "<broken>"))
	runOnce(t, h.ctx, a, true)
	if failure := failed(t, h.ctx, e, invalid, "INVALID_MODEL"); failure.Details["diagnostics"] == nil {
		t.Fatalf("compile failure has no diagnostics: %+v", failure)
	}
	mapped := accept(t, h.ctx, e, "mapped", deploySource(t, `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL">
		<process id="p" isExecutable="true">
			<startEvent id="begin"/><serviceTask id="unsupported"/><endEvent id="finish"/>
			<sequenceFlow id="one" sourceRef="begin" targetRef="unsupported"/>
			<sequenceFlow id="two" sourceRef="unsupported" targetRef="finish"/>
		</process>
	</definitions>`))
	runOnce(t, h.ctx, a, true)
	failed(t, h.ctx, e, mapped, "UNSUPPORTED_FEATURE")
	old := accept(t, h.ctx, e, "old", start(`{"processKey":"p"}`))
	runOnce(t, h.ctx, a, true)
	oldInstance := success[started](t, h.ctx, e, old)
	if oldInstance.Version != 2 {
		t.Fatalf("expected v2 pin after invalid deploy, got %+v", oldInstance)
	}

	// Claim Start, commit another publication on B, then apply Start on A.
	// B starts with COMMAND priority so the old instance remains live.
	queued := accept(t, h.ctx, e, "queued", start(`{"processKey":"p"}`))
	b = h.runner(t, s2, "B2")
	paused := h.runner(t, applyHook{Store: s1, before: func() {
		next := accept(t, h.ctx, e, "third", deploy(t, "new_end"))
		runOnce(t, h.ctx, b, true)
		if published := success[publication](t, h.ctx, e, next); published.Version != 3 {
			t.Fatalf("successor publication = %+v", published)
		}
	}}, "paused")
	runOnce(t, h.ctx, paused, true)
	if current := success[started](t, h.ctx, e, queued); current.Version != 3 {
		t.Fatalf("start pinned at admission or claim: %+v", current)
	}
	page, err := e.ListDefinitionVersions(h.ctx, "p", 2, "")
	must(t, err)
	if len(page.Items) != 2 || page.Items[0].Label != "DRAINED" || page.Items[1].Label != "DRAINING" || page.NextCursor == nil {
		t.Fatalf("old version page = %+v", page)
	}
	_, err = e.ListDefinitionVersions(h.ctx, "other", 2, *page.NextCursor)
	wantError(t, err, 400, "INVALID_REQUEST")
	current, err := e.ListDefinitionVersions(h.ctx, "p", 2, *page.NextCursor)
	must(t, err)
	if len(current.Items) != 1 || current.Items[0].Version != 3 || current.Items[0].Label != "CURRENT" || current.NextCursor != nil {
		t.Fatalf("current version page = %+v", current)
	}
	for range 4 {
		runOnce(t, h.ctx, b, true)
	}
	checkJournal(t, h.ctx, s1, oldInstance.ID, "old_end")
	after, err := e.ListDefinitionVersions(h.ctx, "p", 0, "")
	must(t, err)
	if len(after.Items) != 3 || after.Items[1].Label != "DRAINED" {
		t.Fatalf("drained versions = %+v", after)
	}
}

func testTaskSubmission(t *testing.T, h *harness) {
	s, _ := h.connect(t)
	e := engine.New(s, h.clock)
	r := h.runner(t, s, "A")
	accept(t, h.ctx, e, "publish-task", deployBundle(t,
		model.NewSource("process.bpmn", taskModel("form", "")),
		model.NewSource("form.form", `{"id":"form","type":"default","schemaVersion":19,"components":[{"type":"textfield","key":"field"}]}`),
	))
	runOnce(t, h.ctx, r, true)
	startedReceipt := accept(t, h.ctx, e, "start-task", start(`{"processKey":"p","variables":{"seed":"kept"}}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	created := success[started](t, h.ctx, e, startedReceipt)
	page, err := e.ListTasks(h.ctx, created.ID, "", 10, "")
	must(t, err)
	if len(page.Items) != 1 || page.Items[0].ElementID != "wait" || page.Items[0].Status != "OPEN" {
		t.Fatalf("open task page = %+v", page)
	}
	task, err := e.GetTask(h.ctx, page.Items[0].TaskID)
	must(t, err)
	if task.FormID != "form" || task.ClosedAt != nil || task.Components == nil {
		t.Fatalf("saved form task = %+v", task)
	}

	invalid := accept(t, h.ctx, e, "invalid-submit", complete(page.Items[0].TaskID, `{"unknown":"x"}`))
	runOnce(t, h.ctx, r, true)
	failure := failed(t, h.ctx, e, invalid, "INVALID_FIELDS")
	if failure.Details["fields"] == nil {
		t.Fatalf("invalid field error missing field details: %+v", failure)
	}
	open, err := e.GetTask(h.ctx, task.TaskID)
	must(t, err)
	if open.Status != "OPEN" || open.ClosedAt != nil {
		t.Fatalf("invalid submission closed task: %+v", open)
	}
	_, err = e.Accept(h.ctx, "invalid-type", complete(task.TaskID, `{"field":7}`))
	wantError(t, err, 400, "INVALID_REQUEST")

	submitted := accept(t, h.ctx, e, "submit-task", complete(task.TaskID, `{"field":"accepted"}`))
	runOnce(t, h.ctx, r, true)
	result := success[struct {
		TaskID     string `json:"taskId"`
		InstanceID string `json:"instanceId"`
		Revision   int64  `json:"revision"`
	}](t, h.ctx, e, submitted)
	if result.TaskID != task.TaskID || result.InstanceID != created.ID || result.Revision != 4 {
		t.Fatalf("completion result = %+v", result)
	}
	drain(t, h, r)
	final := checkInstance(t, h.ctx, e, created.ID, 5, "COMPLETED")
	var variables map[string]any
	must(t, json.Unmarshal(final.Variables, &variables))
	if variables["seed"] != "kept" || variables["field"] != "accepted" {
		t.Fatalf("completed variables = %s", final.Variables)
	}
	closed, err := e.GetTask(h.ctx, task.TaskID)
	must(t, err)
	if closed.Status != "COMPLETED" || closed.ClosedAt == nil || closed.Reason != nil {
		t.Fatalf("completed task = %+v", closed)
	}
	secondSubmit := accept(t, h.ctx, e, "duplicate-submit", complete(task.TaskID, `{"field":"again"}`))
	runOnce(t, h.ctx, r, true)
	failed(t, h.ctx, e, secondSubmit, "TASK_NOT_OPEN")
}

func testBoundaryTimer(t *testing.T, h *harness) {
	s, _ := h.connect(t)
	e := engine.New(s, h.clock)
	r := h.runner(t, s, "A")
	accept(t, h.ctx, e, "publish-timer", deployBundle(t,
		model.NewSource("process.bpmn", taskModel("form", `<boundaryEvent id="alarm" attachedToRef="wait"><outgoing>timer-exit</outgoing><timerEventDefinition><timeDuration>PT1S</timeDuration></timerEventDefinition></boundaryEvent><sequenceFlow id="timer-exit" sourceRef="alarm" targetRef="timed-out"/>`)),
		model.NewSource("form.form", `{"id":"form","type":"default","schemaVersion":19,"components":[{"type":"textfield","key":"field"}]}`),
	))
	runOnce(t, h.ctx, r, true)
	startedReceipt := accept(t, h.ctx, e, "start-timer", start(`{"processKey":"p"}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	created := success[started](t, h.ctx, e, startedReceipt)
	page, err := e.ListTasks(h.ctx, created.ID, "", 10, "")
	must(t, err)
	if len(page.Items) != 1 || page.Items[0].DueAt == nil {
		t.Fatalf("timer task page = %+v", page)
	}
	taskID := page.Items[0].TaskID
	h.clock.Advance(time.Second - time.Millisecond)
	for range 3 {
		runOnce(t, h.ctx, r, false)
	}
	stillOpen, err := e.GetTask(h.ctx, taskID)
	must(t, err)
	if stillOpen.Status != "OPEN" {
		t.Fatalf("timer fired early: %+v", stillOpen)
	}
	h.clock.Advance(time.Millisecond)
	drain(t, h, r)
	closed, err := e.GetTask(h.ctx, taskID)
	must(t, err)
	if closed.Status != "CANCELLED" || closed.Reason == nil || *closed.Reason != "TIMER" || closed.ClosedAt == nil {
		t.Fatalf("timed out task = %+v", closed)
	}
	checkInstance(t, h.ctx, e, created.ID, 5, "COMPLETED")
	late := accept(t, h.ctx, e, "late-submit", complete(taskID, `{"field":"late"}`))
	runOnce(t, h.ctx, r, true)
	failed(t, h.ctx, e, late, "TASK_NOT_OPEN")

	secondStart := accept(t, h.ctx, e, "start-before-due-cutoff", start(`{"processKey":"p"}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	second := success[started](t, h.ctx, e, secondStart)
	secondPage, err := e.ListTasks(h.ctx, second.ID, "", 10, "")
	must(t, err)
	if len(secondPage.Items) != 1 || secondPage.Items[0].DueAt == nil {
		t.Fatalf("second timer task = %+v", secondPage)
	}
	// Eligibility is not a hard cutoff: completion may commit after dueAt if
	// the timer transition has not committed yet.
	h.clock.Advance(time.Second)
	completionRunner := h.runner(t, s, "completion-first")
	completion := accept(t, h.ctx, e, "complete-after-due", complete(secondPage.Items[0].TaskID, `{"field":"before-fire"}`))
	runOnce(t, h.ctx, completionRunner, true)
	success[struct {
		TaskID string `json:"taskId"`
	}](t, h.ctx, e, completion)
	drain(t, h, r)
	checkInstance(t, h.ctx, e, second.ID, 5, "COMPLETED")
	closed, err = e.GetTask(h.ctx, secondPage.Items[0].TaskID)
	must(t, err)
	if closed.Status != "COMPLETED" || closed.Reason != nil {
		t.Fatalf("completion did not beat the due timer: %+v", closed)
	}
}

func testParallelJoin(t *testing.T, h *harness) {
	s, _ := h.connect(t)
	e := engine.New(s, h.clock)
	r := h.runner(t, s, "A")
	accept(t, h.ctx, e, "publish-parallel", deployBundle(t,
		model.NewSource("process.bpmn", parallelModel()),
		model.NewSource("left.form", `{"id":"left-form","type":"default","schemaVersion":19,"components":[{"type":"textfield","key":"leftValue"}]}`),
		model.NewSource("right.form", `{"id":"right-form","type":"default","schemaVersion":19,"components":[{"type":"textfield","key":"rightValue"}]}`),
	))
	runOnce(t, h.ctx, r, true)
	startedReceipt := accept(t, h.ctx, e, "start-parallel", start(`{"processKey":"p","variables":{"joined":0}}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	created := success[started](t, h.ctx, e, startedReceipt)
	page, err := e.ListTasks(h.ctx, created.ID, "", 1, "")
	must(t, err)
	if len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("parallel task page = %+v", page)
	}
	taskByElement := map[string]string{}
	secondPage, err := e.ListTasks(h.ctx, created.ID, "", 1, *page.NextCursor)
	must(t, err)
	if len(secondPage.Items) != 1 || secondPage.NextCursor != nil {
		t.Fatalf("second parallel task page = %+v", secondPage)
	}
	for _, task := range append(page.Items, secondPage.Items...) {
		taskByElement[task.ElementID] = task.TaskID
	}
	if taskByElement["left"] == "" || taskByElement["right"] == "" {
		t.Fatalf("parallel tasks = %+v", page.Items)
	}
	first := accept(t, h.ctx, e, "complete-left", complete(taskByElement["left"], `{"leftValue":"L"}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	success[struct {
		TaskID string `json:"taskId"`
	}](t, h.ctx, e, first)
	instance, err := e.GetInstance(h.ctx, created.ID)
	must(t, err)
	var values map[string]any
	must(t, json.Unmarshal(instance.Variables, &values))
	if instance.Status != "RUNNING" || fmt.Sprint(values["joined"]) != "0" || values["leftValue"] != "L" {
		t.Fatalf("first branch did not park at join: %+v vars=%s", instance, instance.Variables)
	}
	stillOpen, err := e.GetTask(h.ctx, taskByElement["right"])
	must(t, err)
	if stillOpen.Status != "OPEN" {
		t.Fatalf("sibling task closed before submission: %+v", stillOpen)
	}
	second := accept(t, h.ctx, e, "complete-right", complete(taskByElement["right"], `{"rightValue":"R"}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	success[struct {
		TaskID string `json:"taskId"`
	}](t, h.ctx, e, second)
	final := checkInstance(t, h.ctx, e, created.ID, 11, "COMPLETED")
	must(t, json.Unmarshal(final.Variables, &values))
	if values["leftValue"] != "L" || values["rightValue"] != "R" || fmt.Sprint(values["joined"]) != "1" {
		t.Fatalf("parallel variables = %s", final.Variables)
	}
}

func testParallelReentry(t *testing.T, h *harness) {
	s, _ := h.connect(t)
	e := engine.New(s, h.clock)
	r := h.runner(t, s, "A")
	accept(t, h.ctx, e, "publish-looping-parallel", deployBundle(t,
		model.NewSource("process.bpmn", parallelLoopModel()),
		model.NewSource("left.form", `{"id":"left-form","type":"default","schemaVersion":19,"components":[{"type":"textfield","key":"leftValue"}]}`),
		model.NewSource("right.form", `{"id":"right-form","type":"default","schemaVersion":19,"components":[{"type":"textfield","key":"rightValue"}]}`),
	))
	runOnce(t, h.ctx, r, true)
	startedReceipt := accept(t, h.ctx, e, "start-looping-parallel", start(`{"processKey":"p","variables":{"joined":0}}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	created := success[started](t, h.ctx, e, startedReceipt)
	firstTasks := openTaskIDs(t, h.ctx, e, created.ID)
	if len(firstTasks) != 2 {
		t.Fatalf("first fork tasks = %+v", firstTasks)
	}
	completeBranchTasks(t, h, e, r, firstTasks, "round1")
	secondTasks := openTaskIDs(t, h.ctx, e, created.ID)
	if len(secondTasks) != 2 || secondTasks["left"] == firstTasks["left"] || secondTasks["right"] == firstTasks["right"] {
		t.Fatalf("second fork did not create fresh task activations: first=%v second=%v", firstTasks, secondTasks)
	}
	completeBranchTasks(t, h, e, r, secondTasks, "round2")
	final := checkInstance(t, h.ctx, e, created.ID, 23, "COMPLETED")
	var values map[string]any
	must(t, json.Unmarshal(final.Variables, &values))
	if fmt.Sprint(values["joined"]) != "2" || values["leftValue"] != "round2-left" || values["rightValue"] != "round2-right" {
		t.Fatalf("re-entered parallel result = %s", final.Variables)
	}
}

func testFixtureImmediateExit(t *testing.T, h *harness) {
	e, r, instance := startFixture(t, h, "fixture-immediate", `{"input":"hello"}`)
	task := firstOpenTask(t, h.ctx, e, instance.ID)
	view, err := e.GetTask(h.ctx, task.TaskID)
	must(t, err)
	if !strings.Contains(string(view.Components), "Caller says: hello") || !strings.Contains(string(view.Components), "Execution: 0") {
		t.Fatalf("fixture form view = %s", view.Components)
	}
	receipt := accept(t, h.ctx, e, "fixture-terminate", complete(task.TaskID, `{"text_command":"terminate"}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	success[struct {
		TaskID string `json:"taskId"`
	}](t, h.ctx, e, receipt)
	final := checkInstance(t, h.ctx, e, instance.ID, 8, "COMPLETED")
	if got := fixtureVariable(t, final.Variables, "executionCount"); got != "1" {
		t.Fatalf("immediate exit count = %s, want 1", got)
	}
	if got := fixtureVariable(t, final.Variables, "decision"); got != "break" {
		t.Fatalf("immediate exit decision = %s, want break", got)
	}
}

func testFixtureFirstMatch(t *testing.T, h *harness) {
	e, r, instance := startFixture(t, h, "fixture-stop", `{"input":"hello"}`)
	for count := 1; count <= 3; count++ {
		task := firstOpenTask(t, h.ctx, e, instance.ID)
		receipt := accept(t, h.ctx, e, fmt.Sprintf("fixture-stop-%d", count), complete(task.TaskID, `{"command":"stop"}`))
		runOnce(t, h.ctx, r, true)
		drain(t, h, r)
		success[struct {
			TaskID string `json:"taskId"`
		}](t, h.ctx, e, receipt)
		current, err := e.GetInstance(h.ctx, instance.ID)
		must(t, err)
		if got := fixtureVariable(t, current.Variables, "executionCount"); got != fmt.Sprint(count) {
			t.Fatalf("after stop submission %d count = %s", count, got)
		}
		if count < 3 && current.Status != "RUNNING" {
			t.Fatalf("stop ended early at %d: %+v", count, current)
		}
	}
	final, err := e.GetInstance(h.ctx, instance.ID)
	must(t, err)
	if final.Status != "COMPLETED" || fixtureVariable(t, final.Variables, "decision") != "break" {
		t.Fatalf("FIRST table result = %+v", final)
	}
}

func testFixtureMaximumLoop(t *testing.T, h *harness) {
	e, r, instance := startFixture(t, h, "fixture-maximum", `{"input":"hello"}`)
	for count := 1; count <= 11; count++ {
		task := firstOpenTask(t, h.ctx, e, instance.ID)
		receipt := accept(t, h.ctx, e, fmt.Sprintf("fixture-continue-%d", count), complete(task.TaskID, `{"command":"continue"}`))
		runOnce(t, h.ctx, r, true)
		drain(t, h, r)
		success[struct {
			TaskID string `json:"taskId"`
		}](t, h.ctx, e, receipt)
	}
	final, err := e.GetInstance(h.ctx, instance.ID)
	must(t, err)
	if final.Status != "COMPLETED" || fixtureVariable(t, final.Variables, "executionCount") != "11" ||
		fixtureVariable(t, final.Variables, "decision") != "break" {
		t.Fatalf("maximum loop result = %+v", final)
	}
	if page, err := e.ListTasks(h.ctx, instance.ID, "", 50, ""); err != nil || len(page.Items) != 0 {
		t.Fatalf("maximum loop left open tasks: %+v err=%v", page, err)
	}
}

func testFixtureTimer(t *testing.T, h *harness) {
	e, r, instance := startFixture(t, h, "fixture-timer", `{"input":"hello","command":"continue","text_command":""}`)
	first := firstOpenTask(t, h.ctx, e, instance.ID)
	h.clock.Advance(60 * time.Second)
	drain(t, h, r)
	old, err := e.GetTask(h.ctx, first.TaskID)
	must(t, err)
	if old.Status != "CANCELLED" || old.Reason == nil || *old.Reason != "TIMER" {
		t.Fatalf("fixture timeout task = %+v", old)
	}
	second := firstOpenTask(t, h.ctx, e, instance.ID)
	if second.TaskID == first.TaskID {
		t.Fatal("timer reused its task ID")
	}
	current, err := e.GetInstance(h.ctx, instance.ID)
	must(t, err)
	if fixtureVariable(t, current.Variables, "executionCount") != "1" {
		t.Fatalf("timer path count = %s", fixtureVariable(t, current.Variables, "executionCount"))
	}
	late := accept(t, h.ctx, e, "fixture-late-submit", complete(first.TaskID, `{"text_command":"terminate"}`))
	runOnce(t, h.ctx, r, true)
	failed(t, h.ctx, e, late, "TASK_NOT_OPEN")
	receipt := accept(t, h.ctx, e, "fixture-after-timer", complete(second.TaskID, `{"text_command":"terminate"}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	success[struct {
		TaskID string `json:"taskId"`
	}](t, h.ctx, e, receipt)
	final, err := e.GetInstance(h.ctx, instance.ID)
	must(t, err)
	if final.Status != "COMPLETED" || fixtureVariable(t, final.Variables, "executionCount") != "2" {
		t.Fatalf("fixture after timer = %+v", final)
	}
}

func testIncidentFreeze(t *testing.T, h *harness) {
	s, _ := h.connect(t)
	e := engine.New(s, h.clock)
	r := h.runner(t, s, "A")
	accept(t, h.ctx, e, "publish-bad-expression", deploySource(t, `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" xmlns:zeebe="http://camunda.org/schema/zeebe/1.0"><process id="p" isExecutable="true"><startEvent id="begin"/><scriptTask id="bad"><extensionElements><zeebe:script expression="= 1 + &quot;x&quot;" resultVariable="value"/></extensionElements></scriptTask><endEvent id="finish"/><sequenceFlow id="one" sourceRef="begin" targetRef="bad"/><sequenceFlow id="two" sourceRef="bad" targetRef="finish"/></process></definitions>`))
	runOnce(t, h.ctx, r, true)
	receipt := accept(t, h.ctx, e, "start-bad-expression", start(`{"processKey":"p","variables":{"value":0}}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	instance := success[started](t, h.ctx, e, receipt)
	result, err := e.GetInstance(h.ctx, instance.ID)
	must(t, err)
	if result.Status != "INCIDENT" || result.Incident == nil || result.Incident.Code != "EVALUATION_ERROR" || result.Incident.ElementID != "bad" {
		t.Fatalf("model failure was not isolated as an incident: %+v", result)
	}
	if fixtureVariable(t, result.Variables, "value") != "0" {
		t.Fatalf("failed transition partially wrote variables: %s", result.Variables)
	}
	drain(t, h, r)
	unchanged, err := e.GetInstance(h.ctx, instance.ID)
	must(t, err)
	if unchanged.Revision != result.Revision || unchanged.Status != "INCIDENT" {
		t.Fatalf("incident did not freeze the instance: %+v", unchanged)
	}
}

func testTimerCompletionRace(t *testing.T, h *harness) {
	s1, _ := h.connect(t)
	s2, _ := h.connect(t)
	e := engine.New(s1, h.clock)
	runner := h.runner(t, s1, "setup")
	accept(t, h.ctx, e, "publish-race", deployBundle(t,
		model.NewSource("process.bpmn", taskModel("form", `<boundaryEvent id="alarm" attachedToRef="wait"><outgoing>timer-exit</outgoing><timerEventDefinition><timeDuration>PT1S</timeDuration></timerEventDefinition></boundaryEvent><sequenceFlow id="timer-exit" sourceRef="alarm" targetRef="timed-out"/>`)),
		model.NewSource("form.form", `{"id":"form","type":"default","schemaVersion":19,"components":[{"type":"textfield","key":"field"}]}`),
	))
	runOnce(t, h.ctx, runner, true)
	startedReceipt := accept(t, h.ctx, e, "start-race", start(`{"processKey":"p"}`))
	runOnce(t, h.ctx, runner, true)
	drain(t, h, runner)
	created := success[started](t, h.ctx, e, startedReceipt)
	task := firstOpenTask(t, h.ctx, e, created.ID)

	timerRunner := h.runner(t, s2, "timer")
	for range 2 {
		runOnce(t, h.ctx, timerRunner, false)
	}
	completionRunner := h.runner(t, s1, "completion")
	h.clock.Advance(time.Second)
	receipt := accept(t, h.ctx, e, "race-complete", complete(task.TaskID, `{"field":"submitted"}`))
	gate := make(chan struct{})
	results := make(chan error, 2)
	for _, current := range []*engine.Runner{timerRunner, completionRunner} {
		go func(r *engine.Runner) {
			<-gate
			_, err := r.RunOnce(h.ctx)
			results <- err
		}(current)
	}
	close(gate)
	for range 2 {
		if err := <-results; err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("timer/task race runner error: %v", err)
		}
	}

	var operation application.Operation
	for range 12 {
		var err error
		operation, err = e.GetOperation(h.ctx, receipt.OperationID)
		must(t, err)
		if operation.Status != "PENDING" {
			break
		}
		h.clock.Advance(lease)
		for _, current := range []*engine.Runner{timerRunner, completionRunner, runner} {
			for range 3 {
				_, err := current.RunOnce(h.ctx)
				if err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrLeaseLost) {
					t.Fatalf("recover timer/task race: %v", err)
				}
			}
		}
	}
	if operation.Status == "PENDING" {
		t.Fatal("timer/task race did not settle")
	}
	drain(t, h, runner)
	closed, err := e.GetTask(h.ctx, task.TaskID)
	must(t, err)
	if closed.Status == "COMPLETED" {
		if operation.Status != "SUCCEEDED" || closed.Reason != nil {
			t.Fatalf("task completion won inconsistently: op=%+v task=%+v", operation, closed)
		}
	} else if closed.Status == "CANCELLED" {
		if operation.Status != "FAILED" || operation.Error == nil || operation.Error.Code != "TASK_NOT_OPEN" ||
			closed.Reason == nil || *closed.Reason != "TIMER" {
			t.Fatalf("timer won inconsistently: op=%+v task=%+v", operation, closed)
		}
	} else {
		t.Fatalf("race left task open: %+v", closed)
	}
	final := checkInstance(t, h.ctx, e, created.ID, 5, "COMPLETED")
	must(t, s1.WithTransaction(h.ctx, func(tx store.Tx) error {
		entry, err := tx.GetTransition(created.ID, 4)
		if err != nil {
			return err
		}
		if entry.Kind != store.TransitionStep || entry.ElementID == nil {
			return fmt.Errorf("race winner transition = %+v", entry)
		}
		want := "wait"
		if closed.Status == "CANCELLED" {
			want = "alarm"
		}
		if *entry.ElementID != want {
			return fmt.Errorf("race winner = %q, want %q", *entry.ElementID, want)
		}
		_, err = tx.GetTransition(created.ID, 6)
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("race produced a duplicate successor transition: %v", err)
		}
		return nil
	}))
	if final.Status != "COMPLETED" {
		t.Fatalf("race terminal state = %+v", final)
	}
}

func taskModel(formID, boundary string) string {
	timerEnd := ""
	if boundary != "" {
		timerEnd = `<endEvent id="timed-out"/>`
	}
	return `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" xmlns:zeebe="http://camunda.org/schema/zeebe/1.0">
		<process id="p" isExecutable="true"><startEvent id="begin"/><userTask id="wait"><extensionElements><zeebe:userTask/><zeebe:formDefinition formId="` + formID + `"/></extensionElements><outgoing>normal-exit</outgoing></userTask>
		` + boundary + timerEnd + `
		<endEvent id="finished"/><sequenceFlow id="start-wait" sourceRef="begin" targetRef="wait"/><sequenceFlow id="normal-exit" sourceRef="wait" targetRef="finished"/></process></definitions>`
}

func parallelModel() string {
	return `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" xmlns:zeebe="http://camunda.org/schema/zeebe/1.0">
	<process id="p" isExecutable="true"><startEvent id="begin"/><parallelGateway id="split"/><userTask id="left"><extensionElements><zeebe:userTask/><zeebe:formDefinition formId="left-form"/></extensionElements></userTask><userTask id="right"><extensionElements><zeebe:userTask/><zeebe:formDefinition formId="right-form"/></extensionElements></userTask><parallelGateway id="join"/><scriptTask id="increment"><extensionElements><zeebe:script expression="= joined + 1" resultVariable="joined"/></extensionElements></scriptTask><endEvent id="finished"/>
	<sequenceFlow id="start-split" sourceRef="begin" targetRef="split"/><sequenceFlow id="split-left" sourceRef="split" targetRef="left"/><sequenceFlow id="split-right" sourceRef="split" targetRef="right"/><sequenceFlow id="left-join" sourceRef="left" targetRef="join"/><sequenceFlow id="right-join" sourceRef="right" targetRef="join"/><sequenceFlow id="join-increment" sourceRef="join" targetRef="increment"/><sequenceFlow id="increment-end" sourceRef="increment" targetRef="finished"/>
	</process></definitions>`
}

func parallelLoopModel() string {
	return `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" xmlns:zeebe="http://camunda.org/schema/zeebe/1.0">
	<process id="p" isExecutable="true"><startEvent id="begin"/><exclusiveGateway id="entry"/><parallelGateway id="split"/><userTask id="left"><extensionElements><zeebe:userTask/><zeebe:formDefinition formId="left-form"/></extensionElements></userTask><userTask id="right"><extensionElements><zeebe:userTask/><zeebe:formDefinition formId="right-form"/></extensionElements></userTask><parallelGateway id="join"/><scriptTask id="increment"><extensionElements><zeebe:script expression="= joined + 1" resultVariable="joined"/></extensionElements></scriptTask><exclusiveGateway id="again" default="finish-flow"/><endEvent id="finished"/>
	<sequenceFlow id="start-entry" sourceRef="begin" targetRef="entry"/><sequenceFlow id="entry-split" sourceRef="entry" targetRef="split"/><sequenceFlow id="split-left" sourceRef="split" targetRef="left"/><sequenceFlow id="split-right" sourceRef="split" targetRef="right"/><sequenceFlow id="left-join" sourceRef="left" targetRef="join"/><sequenceFlow id="right-join" sourceRef="right" targetRef="join"/><sequenceFlow id="join-increment" sourceRef="join" targetRef="increment"/><sequenceFlow id="increment-again" sourceRef="increment" targetRef="again"/><sequenceFlow id="repeat" sourceRef="again" targetRef="entry"><conditionExpression>= joined &lt; 2</conditionExpression></sequenceFlow><sequenceFlow id="finish-flow" sourceRef="again" targetRef="finished"/></process></definitions>`
}

func deployBundle(t *testing.T, files ...model.Source) application.Command {
	t.Helper()
	raw, err := json.Marshal(map[string][]model.Source{"files": files})
	must(t, err)
	return application.Command{Kind: application.Deploy, Payload: raw}
}

func complete(taskID, fields string) application.Command {
	return application.Command{
		Kind: application.CompleteTask, Target: map[string]string{"taskId": taskID},
		Payload: json.RawMessage(`{"fields":` + fields + `}`),
	}
}

func drain(t *testing.T, h *harness, r *engine.Runner) {
	t.Helper()
	for i := 0; i < 100; i++ {
		worked, err := r.RunOnce(h.ctx)
		must(t, err)
		if !worked {
			return
		}

	}
	t.Fatal("runner did not become idle")
}

func openTaskIDs(t *testing.T, ctx context.Context, e application.Service, instanceID string) map[string]string {
	t.Helper()
	page, err := e.ListTasks(ctx, instanceID, "", 50, "")
	must(t, err)
	out := make(map[string]string, len(page.Items))
	for _, task := range page.Items {
		out[task.ElementID] = task.TaskID
	}
	return out
}

func completeBranchTasks(t *testing.T, h *harness, e application.Service, r *engine.Runner, tasks map[string]string, suffix string) {
	t.Helper()
	for _, element := range []string{"left", "right"} {
		key := element + "Value"
		receipt := accept(t, h.ctx, e, suffix+"-"+element, complete(tasks[element], `{"`+key+`":"`+suffix+`-`+element+`"}`))
		runOnce(t, h.ctx, r, true)
		drain(t, h, r)
		success[struct {
			TaskID string `json:"taskId"`
		}](t, h.ctx, e, receipt)
	}
}

func startFixture(t *testing.T, h *harness, suffix, variables string) (application.Service, *engine.Runner, started) {
	t.Helper()
	s, _ := h.connect(t)
	e := engine.New(s, h.clock)
	r := h.runner(t, s, "A")
	accept(t, h.ctx, e, "deploy-"+suffix, fixtureBundle(t))
	runOnce(t, h.ctx, r, true)
	receipt := accept(t, h.ctx, e, "start-"+suffix, start(`{"processKey":"Process_Collect_from_Form","variables":`+variables+`}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	return e, r, success[started](t, h.ctx, e, receipt)
}

func fixtureBundle(t *testing.T) application.Command {
	t.Helper()
	dir := filepath.Join("..", "..", "acceptance", "release1")
	var files []model.Source
	for _, name := range []string{"flow-form.bpmn", "looping_decision.dmn", "form-textfield.form"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		must(t, err)
		files = append(files, model.NewSource(name, string(content)))
	}
	return deployBundle(t, files...)
}

func firstOpenTask(t *testing.T, ctx context.Context, e application.Service, instanceID string) application.TaskSummary {
	t.Helper()
	page, err := e.ListTasks(ctx, instanceID, "", 50, "")
	must(t, err)
	if len(page.Items) != 1 {
		t.Fatalf("expected one open task, got %+v", page)
	}
	return page.Items[0]
}

func fixtureVariable(t *testing.T, raw json.RawMessage, key string) string {
	t.Helper()
	var values map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	must(t, decoder.Decode(&values))
	return fmt.Sprint(values[key])
}

type applyHook struct {
	store.Store
	before, after func()
}

func (h *harness) expireApply(t *testing.T, s store.Store) {
	t.Helper()
	r := h.runner(t, applyHook{Store: s, after: func() { h.clock.Advance(lease) }}, "stale")
	if done, err := r.RunOnce(h.ctx); done || !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expired apply: worked=%v, err=%v", done, err)
	}
}

func (s applyHook) Apply(ctx context.Context, lease store.Lease, fn func(store.Tx, store.WorkItem) (*store.Outcome, error)) error {
	if s.before != nil {
		s.before()
	}
	return s.Store.Apply(ctx, lease, func(tx store.Tx, item store.WorkItem) (*store.Outcome, error) {
		outcome, err := fn(tx, item)
		if err == nil && s.after != nil {
			s.after()
		}
		return outcome, err
	})
}

type unavailableAdmission struct{ store.Store }

func (unavailableAdmission) Admit(context.Context, store.Admission) (store.Operation, error) {
	return store.Operation{}, errors.New("cannot reach store")
}

type publication struct {
	Key     string `json:"processKey"`
	Version int64  `json:"version"`
}

type started struct {
	ID      string `json:"instanceId"`
	Version int64  `json:"version"`
}

func deploy(t *testing.T, end string) application.Command {
	t.Helper()
	return deploySource(t, `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL">
		<process id="p" isExecutable="true">
			<startEvent id="begin"/><endEvent id="`+end+`"/>
			<sequenceFlow id="go" sourceRef="begin" targetRef="`+end+`"/>
		</process>
	</definitions>`)
}

func deploySource(t *testing.T, xml string) application.Command {
	t.Helper()
	raw, err := json.Marshal(map[string][]model.Source{"files": {model.NewSource("path.bpmn", xml)}})
	must(t, err)
	return application.Command{Kind: application.Deploy, Payload: raw}
}

func start(payload string) application.Command {
	return application.Command{Kind: application.Start, Payload: json.RawMessage(payload)}
}

func accept(t *testing.T, ctx context.Context, svc application.Service, key string, command application.Command) application.Receipt {
	t.Helper()
	out, err := svc.Accept(ctx, key, command)
	must(t, err)
	return out
}

func success[T any](t *testing.T, ctx context.Context, e application.Service, r application.Receipt) T {
	t.Helper()
	op, err := e.GetOperation(ctx, r.OperationID)
	must(t, err)
	if op.Status != "SUCCEEDED" || op.Error != nil {
		t.Fatalf("expected successful operation, got %+v", op)
	}
	var result T
	must(t, json.Unmarshal(op.Result, &result))
	return result
}

func failed(t *testing.T, ctx context.Context, e application.Service, r application.Receipt, code string) *application.Failure {
	t.Helper()
	op, err := e.GetOperation(ctx, r.OperationID)
	must(t, err)
	if op.Status != "FAILED" || op.Result != nil || op.Error == nil || op.Error.Code != code {
		t.Fatalf("expected failed operation (%s), got %+v", code, op)
	}
	return op.Error
}

func runOnce(t *testing.T, ctx context.Context, r *engine.Runner, expected bool) {
	t.Helper()
	worked, err := r.RunOnce(ctx)
	must(t, err)
	if worked != expected {
		t.Fatalf("RunOnce worked = %v, want %v", worked, expected)
	}
}

func checkInstance(t *testing.T, ctx context.Context, svc application.Service, id string, revision int64, status string) application.Instance {
	t.Helper()
	in, err := svc.GetInstance(ctx, id)
	must(t, err)
	if in.Revision != revision || in.Status != status {
		t.Fatalf("want revision %d with status %s, got %+v", revision, status, in)
	}
	return in
}

func checkJournal(t *testing.T, ctx context.Context, s store.Store, id, final string) {
	t.Helper()
	must(t, s.WithTransaction(ctx, func(tx store.Tx) error {
		for i, want := range []struct {
			kind    store.TransitionKind
			element string
		}{{store.TransitionStarted, ""}, {store.TransitionStep, "begin"}, {store.TransitionCompleted, final}} {
			entry, err := tx.GetTransition(id, int64(i+1))
			if err != nil {
				return err
			}
			if entry.Kind != want.kind ||
				(want.element == "" && entry.ElementID != nil) ||
				(want.element != "" && (entry.ElementID == nil || *entry.ElementID != want.element)) {
				return fmt.Errorf("transition %d = %+v, want %+v", i+1, entry, want)
			}
		}
		_, err := tx.GetTransition(id, 4)
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("unexpected transition at revision 4: %v", err)
		}
		return nil
	}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var appErr *application.Error
	if !errors.As(err, &appErr) || appErr.Status != status || appErr.Failure.Code != code {
		t.Fatalf("error = %v, want %d %s", err, status, code)
	}
}
