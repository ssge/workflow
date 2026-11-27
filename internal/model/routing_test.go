package model_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ssge/workflow/internal/model"
)

// The compiler does not select a route: it supplies ordered flow IDs, compiled
// predicates and the optional default, including when no predicate is true.
func TestU2XORRoutingInputs(t *testing.T) {
	for _, flag := range []bool{true, false} {
		for _, fallback := range []bool{true, false} {
			body := begin + end + seq("enter", "entrée", "choice")
			exits := `<outgoing>b</outgoing><outgoing>a</outgoing>`
			attr := ""
			if fallback {
				attr = ` default="other"`
				exits += `<outgoing>other</outgoing>`
				body += seq("other", "choice", "fin")
			}
			body += `<exclusiveGateway id="choice"` + attr + `>` + exits + `</exclusiveGateway>`
			for _, id := range []string{"a", "b"} {
				body += `<sequenceFlow id="` + id + `" sourceRef="choice" targetRef="fin"><conditionExpression>= flag</conditionExpression></sequenceFlow>`
			}
			p := compile(t, model.NewSource(filename, document(body)))
			n, _ := p.Node("choice")
			var predicates []string
			for _, id := range n.Outgoing() {
				f, _ := p.Flow(id)
				if id == n.DefaultFlow && f.Condition == "" {
					continue
				}
				fn, ok := p.Expression(f.Condition)
				if !ok {
					t.Fatal("missing predicate", id)
				}
				v, err := fn(map[string]any{"flag": flag})
				if err != nil || v != flag {
					t.Fatal(v, err)
				}
				predicates = append(predicates, id)
			}
			if !slices.Equal(predicates, []string{"a", "b"}) || (n.DefaultFlow == "other") != fallback {
				t.Fatal(n, predicates)
			}
		}
	}
}

func TestU2BoundaryPathsInParallelRegions(t *testing.T) {
	form := model.NewSource("shared.form", `{"id":"F","type":"default","schemaVersion":19,"components":[]}`)
	task := func(id string) string {
		return `<userTask id="` + id + `"><extensionElements xmlns:z="http://camunda.org/schema/zeebe/1.0"><z:userTask/><z:formDefinition formId="F"/></extensionElements></userTask>`
	}
	nodes := begin + end + task("left") + task("right") + `<parallelGateway id="fork"/><parallelGateway id="join"/>
		<exclusiveGateway id="mergeLeft"/><boundaryEvent id="clock" attachedToRef="left"><timerEventDefinition><timeDuration>PT1S</timeDuration></timerEventDefinition></boundaryEvent>`
	body := nodes + seq("enter", "entrée", "fork") + seq("l", "fork", "left") + seq("r", "fork", "right") + seq("normal", "left", "mergeLeft") + seq("timeout", "clock", "mergeLeft") + seq("lDone", "mergeLeft", "join") + seq("rDone", "right", "join") + seq("exit", "join", "fin")
	p := compile(t, model.NewSource(filename, document(body)), form)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var loaded model.Plan
	if err = json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if fork, ok := loaded.ParallelPeer("join"); !ok || fork != "fork" {
		t.Fatal("lost matching split")
	}
	for _, target := range []string{"right", "fin", "join"} {
		bad := strings.Replace(body, seq("timeout", "clock", "mergeLeft"), seq("timeout", "clock", target), 1)
		d := rejected(t, model.NewSource(filename, document(bad)), form)
		if d.Code() != model.InvalidModel || d.File() != filename || d.Field() != "parallelGateway" {
			t.Fatal(d)
		}
	}
}

func TestU2PinnedResourceVersions(t *testing.T) {
	files := release1(t)
	old := compile(t, files...)
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	// Same process, form and decision IDs, but a newly deployed version.
	files[1] = model.NewSource(files[1].Name(), strings.Replace(files[1].Content(), `<text>"loop"</text>`, `<text>"break"</text>`, 1))
	files[2] = model.NewSource(files[2].Name(), `{"id":"Form_0w2uzn0","type":"default","schemaVersion":19,"components":[]}`)
	fresh := compile(t, files...)
	restored, err := (model.Compiler{}).Restore(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*model.Plan{fresh, old, restored} {
		decision, _ := p.Decision("Decision_0mepvum")
		value, err := decision.Evaluate(map[string]any{"executionCount": json.Number("1")})
		if err != nil || (value == "break") != (p == fresh) {
			t.Fatal("decision version was not pinned", value, err)
		}
		form, _ := p.Form("Form_0w2uzn0")
		view, err := form.Render(map[string]any{"input": "a", "executionCount": json.Number("0")})
		if err != nil || (len(view.Components) == 0) != (p == fresh) {
			t.Fatal("form version was not pinned", err)
		}
	}
}
