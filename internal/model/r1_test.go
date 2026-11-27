package model_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ssge/workflow/internal/evaluator"
	"github.com/ssge/workflow/internal/model"
)

func release1(t *testing.T) []model.Source {
	t.Helper()
	var files []model.Source
	for _, name := range []string{"flow-form.bpmn", "looping_decision.dmn", "form-textfield.form", ".process-application"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "acceptance", "release1", name))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, model.NewSource(name, string(b)))
	}
	return files
}

func TestU2Release1PinnedPlan(t *testing.T) {
	files := release1(t)
	check := func(p *model.Plan) {
		t.Helper()
		start, _ := p.Node(p.StartID())
		assign := start.OutputMappings[0]
		fn, ok := p.Expression(assign.Source)
		if !ok || assign.Target != "executionCount" {
			t.Fatal("missing compiled start mapping")
		}
		v, err := fn(nil)
		if err != nil || v != json.Number("0") {
			t.Fatalf("start expression: %v, %v", v, err)
		}
		start.OutputMappings[0].Target = "changed"
		start, _ = p.Node(p.StartID())
		if start.OutputMappings[0].Target != "executionCount" {
			t.Fatal("mutable start mappings")
		}
		xor, _ := p.Node("Gateway_CheckCount")
		if xor.DefaultFlow != "Flow_Complete" || !slices.Equal(xor.Outgoing(), []string{"Flow_Repeat", "Flow_Complete"}) {
			t.Fatal("lost XOR priority or default")
		}
		host, _ := p.Node("Activity_1hidco5")
		timer, ok := p.Boundary(host.ID)
		if !ok || timer.DurationSeconds != 60 || timer.AttachedToRef != host.ID {
			t.Fatal("missing boundary timer")
		}
		tpl, ok := p.Form(host.FormID)
		if !ok {
			t.Fatal("missing pinned form")
		}
		if _, err := tpl.Render(map[string]any{"input": "hello", "executionCount": v}); err != nil {
			t.Fatal(err)
		}
		rule, _ := p.Node("Activity_0modv4y")
		d, ok := p.Decision(rule.DecisionID)
		if !ok || rule.ResultVariable != "decision" {
			t.Fatal("missing pinned decision")
		}
		if out, err := d.Evaluate(map[string]any{"executionCount": json.Number("1")}); err != nil || out != "loop" {
			t.Fatalf("decision: %v, %v", out, err)
		}
		script, _ := p.Node("Activity_IncrementCount")
		fn, ok = p.Expression(script.Expression)
		if !ok {
			t.Fatal("missing script")
		}
		if out, err := fn(map[string]any{"executionCount": v}); err != nil || out != json.Number("1") {
			t.Fatalf("script: %v, %v", out, err)
		}
	}
	p := compile(t, files...)
	check(p)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var loaded model.Plan
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	check(&loaded)
	files[0] = model.NewSource("renamed.bpmn", strings.NewReplacer("bpmn:", "flow:", "xmlns:bpmn=", "xmlns:flow=", "zeebe:", "z:", "xmlns:zeebe=", "xmlns:z=").Replace(files[0].Content()))
	check(compile(t, files...))
}

func TestU2Release1Diagnostics(t *testing.T) {
	base := release1(t)
	for _, tc := range []struct {
		old, new, id, field string
		code                model.Code
	}{
		{`formId="Form_0w2uzn0"`, `formId="absent"`, "Activity_1hidco5", "formId", model.InvalidModel},
		{`decisionId="Decision_0mepvum"`, `decisionId="absent"`, "Activity_0modv4y", "decisionId", model.InvalidModel},
		{`expression="= executionCount + 1"`, `expression="= ("`, "Activity_IncrementCount", "expression", model.InvalidModel},
		{`target="executionCount"`, `target="obj.count"`, "StartEvent_1", "target", model.InvalidModel},
		{`attachedToRef=`, `cancelActivity="false" attachedToRef=`, "Event_14nbybj", "cancelActivity", model.UnsupportedFeature},
		{"PT60s", "PT2147483648S", "Event_14nbybj", "timeDuration", model.InvalidModel},
		{`default="Flow_Complete"`, `default="Flow_CheckCount"`, "Gateway_CheckCount", "default", model.InvalidModel},
		{`formId=`, `bindingType="latest" formId=`, "Activity_1hidco5", "bindingType", model.UnsupportedFeature},
		{`xsi:type="bpmn:tFormalExpression"`, `xmlns:f="urn:wrong" xsi:type="f:tFormalExpression"`, "Flow_Repeat", "type", model.UnsupportedFeature},
	} {
		t.Run(tc.field, func(t *testing.T) {
			files := slices.Clone(base)
			files[0] = model.NewSource(base[0].Name(), strings.Replace(base[0].Content(), tc.old, tc.new, 1))
			d := rejected(t, files...)
			if d.Code() != tc.code || d.File() != files[0].Name() || d.ElementID() != tc.id || d.Field() != tc.field {
				t.Fatal(d)
			}
		})
	}
	for i, ext := range []string{".dmn", ".form"} {
		name := "duplicate" + ext
		d := rejected(t, append(slices.Clone(base), model.NewSource(name, base[i+1].Content()))...)
		if d.Code() != model.InvalidModel || d.File() != name || d.Field() != "id" {
			t.Fatal(d)
		}
	}
	c := model.Compiler{CompileFEEL: func(source string) (model.Expression, error) {
		if source == "executionCount + 1" {
			return nil, fmt.Errorf("injected failure")
		}
		return evaluator.CompileDMN(source)
	}}
	if p, ds := c.Compile(base); p != nil || len(ds) == 0 || ds[0].ElementID() != "Activity_IncrementCount" {
		t.Fatal(ds)
	}
}

func seq(id, from, to string) string {
	return fmt.Sprintf(`<sequenceFlow id="%s" sourceRef="%s" targetRef="%s"/>`, id, from, to)
}

func TestU2ParallelRegions(t *testing.T) {
	script := func(id string) string {
		return `<scriptTask id="` + id + `"><extensionElements><script xmlns="http://camunda.org/schema/zeebe/1.0" expression="=1" resultVariable="v"/></extensionElements></scriptTask>`
	}
	nodes := begin + end + `<parallelGateway id="split"/><parallelGateway id="join"/>` + script("a") + script("b")
	base := nodes + seq("enter", "entrée", "split") + seq("left", "split", "a") + seq("right", "split", "b") + seq("arriveA", "a", "join") + seq("arriveB", "b", "join") + seq("exit", "join", "fin")
	p := compile(t, model.NewSource(filename, document(base)))
	if join, ok := p.ParallelPeer("split"); !ok || join != "join" {
		t.Fatal("no paired join")
	}
	// A loop outside a complete region is not a cycle inside a branch.
	cond := `<sequenceFlow id="again" sourceRef="choice" targetRef="merge"><conditionExpression>=true</conditionExpression></sequenceFlow>`
	loop := strings.Replace(strings.Replace(base, seq("enter", "entrée", "split"), seq("enter", "entrée", "merge")+seq("go", "merge", "split"), 1), seq("exit", "join", "fin"), seq("exit", "join", "choice"), 1) + `<exclusiveGateway id="merge"/><exclusiveGateway id="choice" default="done"/>` + cond + seq("done", "choice", "fin")
	compile(t, model.NewSource(filename, document(loop)))
	for name, body := range map[string]string{
		"early end":    strings.Replace(base, seq("arriveA", "a", "join"), seq("arriveA", "a", "fin"), 1),
		"cross branch": strings.Replace(base, seq("left", "split", "a"), seq("left", "split", "b"), 1),
		"side entry":   strings.Replace(loop, `targetRef="merge"><conditionExpression>`, `targetRef="a"><conditionExpression>`, 1),
		"inner cycle":  strings.Replace(base, seq("arriveA", "a", "join"), seq("arriveA", "a", "choice")+`<exclusiveGateway id="choice" default="ok"/>`+seq("ok", "choice", "join")+strings.Replace(cond, `targetRef="merge"`, `targetRef="a"`, 1), 1),
		"nested":       strings.Replace(base, seq("arriveA", "a", "join"), seq("arriveA", "a", "nested")+`<parallelGateway id="nested"/><parallelGateway id="innerEnd"/>`+seq("x", "nested", "innerEnd")+seq("y", "nested", "innerEnd")+seq("z", "innerEnd", "join"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			d := rejected(t, model.NewSource(filename, document(body)))
			if d.Code() != model.InvalidModel || d.File() != filename || d.ElementID() == "" {
				t.Fatal(d)
			}
		})
	}
}
