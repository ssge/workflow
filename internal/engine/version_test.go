package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ssge/workflow/internal/application"
	"github.com/ssge/workflow/internal/engine"
	"github.com/ssge/workflow/internal/model"
)

func testPinnedResources(t *testing.T, h *harness) {
	s, _ := h.connect(t)
	e := engine.New(s, h.clock)
	r := h.runner(t, s, "C2")

	v1 := fixtureResourceSources(t, false)
	publishV1 := accept(t, h.ctx, e, "C2-publish-v1", deployBundle(t, v1...))
	runOnce(t, h.ctx, r, true)
	if got := success[publication](t, h.ctx, e, publishV1); got.Version != 1 {
		t.Fatalf("v1 publication = %+v", got)
	}

	startA := accept(t, h.ctx, e, "C2-start-A", start(`{"processKey":"Process_Collect_from_Form","variables":{"input":"A"}}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	a := success[started](t, h.ctx, e, startA)
	if a.Version != 1 {
		t.Fatalf("A pinned version = %d, want 1", a.Version)
	}
	taskA := firstOpenTask(t, h.ctx, e, a.ID)

	v2 := fixtureResourceSources(t, true)
	publishV2 := accept(t, h.ctx, e, "C2-publish-v2", deployBundle(t, v2...))
	runOnce(t, h.ctx, r, true)
	if got := success[publication](t, h.ctx, e, publishV2); got.Version != 2 {
		t.Fatalf("v2 publication = %+v", got)
	}

	startB := accept(t, h.ctx, e, "C2-start-B", start(`{"processKey":"Process_Collect_from_Form","variables":{"input":"B"}}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	b := success[started](t, h.ctx, e, startB)
	if b.Version != 2 {
		t.Fatalf("B pinned version = %d, want 2", b.Version)
	}
	taskB := firstOpenTask(t, h.ctx, e, b.ID)

	savedA, err := e.GetTask(h.ctx, taskA.TaskID)
	must(t, err)
	if !strings.Contains(string(savedA.Components), "Caller says: A") ||
		strings.Contains(string(savedA.Components), "Version 2 says:") {
		t.Fatalf("A's saved v1 form view = %s", savedA.Components)
	}
	viewB, err := e.GetTask(h.ctx, taskB.TaskID)
	must(t, err)
	if !strings.Contains(string(viewB.Components), "Version 2 says: B") ||
		strings.Contains(string(viewB.Components), "Caller says:") {
		t.Fatalf("B's v2 form view = %s", viewB.Components)
	}
	assertVersionLabels(t, h.ctx, e, map[int64]string{1: "DRAINING", 2: "CURRENT"})

	completeA := accept(t, h.ctx, e, "C2-complete-A", complete(taskA.TaskID, `{"command":"stop"}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	success[struct {
		TaskID string `json:"taskId"`
	}](t, h.ctx, e, completeA)
	loopingA, err := e.GetInstance(h.ctx, a.ID)
	must(t, err)
	if loopingA.Status != "RUNNING" || loopingA.Version != 1 ||
		fixtureVariable(t, loopingA.Variables, "decision") != "loop" {
		t.Fatalf("A's v1 decision result = %+v", loopingA)
	}

	nextTaskA := firstOpenTask(t, h.ctx, e, a.ID)
	nextViewA, err := e.GetTask(h.ctx, nextTaskA.TaskID)
	must(t, err)
	if !strings.Contains(string(nextViewA.Components), "Caller says: A") {
		t.Fatalf("A's subsequent v1 form view = %s", nextViewA.Components)
	}
	completeA = accept(t, h.ctx, e, "C2-complete-A-break", complete(nextTaskA.TaskID, `{"text_command":"terminate"}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	success[struct {
		TaskID string `json:"taskId"`
	}](t, h.ctx, e, completeA)
	finalA, err := e.GetInstance(h.ctx, a.ID)
	must(t, err)
	if finalA.Status != "COMPLETED" || finalA.Version != 1 ||
		fixtureVariable(t, finalA.Variables, "decision") != "break" {
		t.Fatalf("A's final v1 decision result = %+v", finalA)
	}
	assertVersionLabels(t, h.ctx, e, map[int64]string{1: "DRAINED", 2: "CURRENT"})

	completeB := accept(t, h.ctx, e, "C2-complete-B-break", complete(taskB.TaskID, `{"command":"stop"}`))
	runOnce(t, h.ctx, r, true)
	drain(t, h, r)
	success[struct {
		TaskID string `json:"taskId"`
	}](t, h.ctx, e, completeB)
	finalB, err := e.GetInstance(h.ctx, b.ID)
	must(t, err)
	if finalB.Status != "COMPLETED" || finalB.Version != 2 ||
		fixtureVariable(t, finalB.Variables, "decision") != "break" {
		t.Fatalf("B's v2 decision result = %+v", finalB)
	}
}

func fixtureResourceSources(t *testing.T, version2 bool) []model.Source {
	t.Helper()
	dir := filepath.Join("..", "..", "acceptance", "release1")
	var files []model.Source
	for _, name := range []string{"flow-form.bpmn", "looping_decision.dmn", "form-textfield.form"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		must(t, err)
		source := string(content)
		if version2 {
			switch name {
			case "form-textfield.form":
				source = strings.Replace(source, "Caller says: ", "Version 2 says: ", 1)
				if source == string(content) {
					t.Fatal("v2 form display text was not changed")
				}
			case "looping_decision.dmn":
				old := "id=\"LiteralExpression_087h79t\">\n          <text>\"loop\"</text>"
				replacement := "id=\"LiteralExpression_087h79t\">\n          <text>\"break\"</text>"
				source = strings.Replace(source, old, replacement, 1)
				if source == string(content) {
					t.Fatal("v2 decision result was not changed")
				}
			}
		}
		files = append(files, model.NewSource(name, source))
	}
	return files
}

func assertVersionLabels(t *testing.T, ctx context.Context, e application.Service, want map[int64]string) {
	t.Helper()
	page, err := e.ListDefinitionVersions(ctx, "Process_Collect_from_Form", 0, "")
	must(t, err)
	if len(page.Items) != len(want) {
		t.Fatalf("definition versions = %+v", page.Items)
	}
	for _, item := range page.Items {
		if expected, ok := want[item.Version]; !ok || item.Label != expected {
			t.Fatalf("version %d label = %q, want %q (all: %+v)", item.Version, item.Label, expected, page.Items)
		}
	}
}
