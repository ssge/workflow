package engine_test

import (
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"github.com/ssge/workflow/db/store"
	"github.com/ssge/workflow/internal/engine"
	"github.com/ssge/workflow/internal/model"
)

func testLastArrivalRecovery(t *testing.T, h *harness, beforeCommit bool) {
	s, pool := h.connect(t)
	e, a := engine.New(s, h.clock), h.runner(t, s, "A")
	pub := accept(t, h.ctx, e, "deploy", deployBundle(t,
		model.NewSource("parallel.bpmn", parallelModel()),
		model.NewSource("left.form", `{"id":"left-form","type":"default","schemaVersion":19,"components":[]}`),
		model.NewSource("right.form", `{"id":"right-form","type":"default","schemaVersion":19,"components":[]}`),
	))
	runOnce(t, h.ctx, a, true)
	success[publication](t, h.ctx, e, pub)
	receipt := accept(t, h.ctx, e, "start", start(`{"processKey":"p","variables":{"joined":0}}`))
	drain(t, h, a)
	id := success[started](t, h.ctx, e, receipt).ID
	tasks := openTaskIDs(t, h.ctx, e, id)

	left := accept(t, h.ctx, e, "left", complete(tasks["left"], `{}`))
	drain(t, h, a)
	success[struct{}](t, h.ctx, e, left)
	right := accept(t, h.ctx, e, "right", complete(tasks["right"], `{}`))
	runOnce(t, h.ctx, a, true) // Complete the task, leaving only its join step ready.
	success[struct{}](t, h.ctx, e, right)
	before, err := e.GetInstance(h.ctx, id)
	must(t, err)
	checkJoinCheckpoint(t, h, s, pool, id, false)

	if beforeCommit {
		h.expireApply(t, s) // Domain writes succeeded inside Apply, but its final fence must roll them back.
	} else {
		runOnce(t, h.ctx, a, true) // Last arrival committed; stop before polling for its successor.
	}
	must(t, pool.Close())
	s, pool = h.connect(t)
	e, b := engine.New(s, h.clock), h.runner(t, s, "B")
	if beforeCommit {
		checkInstance(t, h.ctx, e, id, before.Revision, "RUNNING")
		checkJoinCheckpoint(t, h, s, pool, id, false)
		runOnce(t, h.ctx, b, true) // Reclaim just the expired arrival.
	}
	checkInstance(t, h.ctx, e, id, before.Revision+1, "RUNNING")
	checkJoinCheckpoint(t, h, s, pool, id, true)
	runOnce(t, h.ctx, b, true) // increment
	runOnce(t, h.ctx, b, true) // end
	h.clock.Advance(2 * lease)
	runOnce(t, h.ctx, b, false)
	final := checkInstance(t, h.ctx, e, id, before.Revision+3, "COMPLETED")
	if fixtureVariable(t, final.Variables, "joined") != "1" || len(openTaskIDs(t, h.ctx, e, id)) != 0 {
		t.Fatalf("expected a single increment and no open tasks, got %+v", final)
	}
	checkJoinCheckpoint(t, h, s, pool, id, true)
}

// An activation is retained even after consumption: counting visits detects a
// second release even if the engine later discarded it as obsolete.
func checkVisitCount(t *testing.T, h *harness, pool *sql.DB, id, element string, want int) {
	t.Helper()
	var n int
	must(t, pool.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM activations WHERE instance_id = $1 AND element_id = $2`,
		id, element).Scan(&n))
	if n != want {
		t.Fatalf("%s visits to %s = %d, want %d", id, element, n, want)
	}
}

func checkJoinCheckpoint(t *testing.T, h *harness, s store.Store, pool *sql.DB, id string, released bool) {
	t.Helper()
	var forkID string
	must(t, pool.QueryRowContext(h.ctx, `SELECT fork_id FROM forks WHERE instance_id = $1`, id).Scan(&forkID))
	forkState, visitState, flows, successors := store.ForkOpen, store.ActivationWaiting, []string{"left-join"}, 0
	if released {
		forkState, visitState, flows, successors = store.ForkClosed, store.ActivationConsumed, []string{"left-join", "right-join"}, 1
	}
	must(t, s.WithTransaction(h.ctx, func(tx store.Tx) error {
		fork, err := tx.GetFork(forkID)
		if err != nil {
			return err
		}
		arrivals, err := tx.ListJoinArrivals(forkID)
		if err != nil {
			return err
		}
		var got []string
		for _, arrival := range arrivals {
			got = append(got, arrival.IncomingFlowID)
			activation, err := tx.GetActivation(arrival.ActivationID)
			if err != nil {
				return err
			}
			if activation.State != visitState {
				t.Errorf("%s arrival state = %s, want %s", arrival.IncomingFlowID, activation.State, visitState)
			}
		}
		if fork.State != forkState || !slices.Equal(got, flows) {
			t.Errorf("fork %s: state=%s arrivals=%v, want %s %v", forkID, fork.State, got, forkState, flows)
		}
		return nil
	}))
	checkVisitCount(t, h, pool, id, "increment", successors)
}

func testIncidentIsolationAfterRestart(t *testing.T, h *harness) {
	s, pool := h.connect(t)
	e, a := engine.New(s, h.clock), h.runner(t, s, "A")
	pub := accept(t, h.ctx, e, "deploy", deploySource(t, `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" xmlns:z="http://camunda.org/schema/zeebe/1.0">
		<process id="p" isExecutable="true"><startEvent id="begin"/><scriptTask id="add"><extensionElements>
		<z:script expression="= amount + 1" resultVariable="result"/></extensionElements></scriptTask><endEvent id="end"/>
		<sequenceFlow id="one" sourceRef="begin" targetRef="add"/><sequenceFlow id="two" sourceRef="add" targetRef="end"/></process></definitions>`))
	runOnce(t, h.ctx, a, true)
	success[publication](t, h.ctx, e, pub)
	bad := accept(t, h.ctx, e, "bad", start(`{"processKey":"p","variables":{"amount":"bad"}}`))
	drain(t, h, a)
	badID := success[started](t, h.ctx, e, bad).ID
	frozen, err := e.GetInstance(h.ctx, badID)
	must(t, err)
	if frozen.Status != "INCIDENT" || frozen.Incident == nil || frozen.Incident.ElementID != "add" || frozen.Incident.Code != "EVALUATION_ERROR" {
		t.Fatalf("expected an evaluation incident at add, got %+v", frozen)
	}
	good := accept(t, h.ctx, e, "good", start(`{"processKey":"p","variables":{"amount":2}}`))
	runOnce(t, h.ctx, a, true)
	goodID := success[started](t, h.ctx, e, good).ID
	checkInstance(t, h.ctx, e, goodID, 1, "RUNNING")
	must(t, pool.Close())

	h.clock.Advance(2 * lease)
	s, pool = h.connect(t)
	e, b := engine.New(s, h.clock), h.runner(t, s, "B")
	drain(t, h, b)
	final := checkInstance(t, h.ctx, e, goodID, 4, "COMPLETED")
	var values map[string]json.RawMessage
	must(t, json.Unmarshal(final.Variables, &values))
	if string(values["result"]) != "3" {
		t.Fatalf("healthy instance result = %s, want 3", final.Variables)
	}
	h.clock.Advance(2 * lease)
	runOnce(t, h.ctx, b, false)
	stillFrozen := checkInstance(t, h.ctx, e, badID, frozen.Revision, "INCIDENT")
	if stillFrozen.Incident == nil || *stillFrozen.Incident != *frozen.Incident || string(stillFrozen.Variables) != `{"amount":"bad"}` {
		t.Fatalf("failed instance changed or retained a partial write: %+v", stillFrozen)
	}
	checkVisitCount(t, h, pool, badID, "end", 0)
}
