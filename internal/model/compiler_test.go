package model_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ssge/workflow/internal/model"
)

const (
	filename = "café.bpmn"
	ns       = `http://www.omg.org/spec/BPMN/20100524/MODEL`
	begin    = `<startEvent id="entrée" name="Begin"/>`
	end      = `<endEvent id="fin"/>`
	edge     = `<sequenceFlow id="go" name="Proceed" sourceRef="entrée" targetRef="fin"/>`
)

func document(body string) string {
	return `<definitions xmlns="` + ns + `" id="defs"><process id="p" name="A → B" isExecutable="true">` + body + `</process></definitions>`
}

func compile(t *testing.T, files ...model.Source) *model.Plan {
	t.Helper()
	p, ds := (model.Compiler{}).Compile(files)
	if p == nil || len(ds) != 0 {
		t.Fatalf("compile: plan=%v diagnostics=%v", p, ds)
	}
	return p
}

func rejected(t *testing.T, files ...model.Source) model.Diagnostic {
	t.Helper()
	p, ds := (model.Compiler{}).Compile(files)
	if p != nil || len(ds) == 0 || ds[0].Reason() == "" {
		t.Fatalf("want diagnostic and no plan, got %v, %v", p, ds)
	}
	return ds[0]
}

func TestU2MinimalPlanRoundTrip(t *testing.T) {
	xml := "\ufeff" + document(edge+end+begin)
	files := []model.Source{model.NewSource(filename, xml), model.NewSource(".process-application", " { }\n")}
	p := compile(t, files...)
	check := func(p *model.Plan) {
		t.Helper()
		if p.ProfileRevision() == "" || p.ProcessKey() != "p" || p.ProcessName() != "A → B" || p.StartID() != "entrée" {
			t.Fatalf("unexpected plan header: %+v", p)
		}
		if !slices.Equal(p.Sources(), files) || len(p.Nodes()) != 2 || len(p.Flows()) != 1 {
			t.Fatal("lost original bundle or graph")
		}
		start, ok := p.Node("entrée")
		if !ok || start.Kind != model.StartEvent || start.Name != "Begin" || len(start.Incoming()) != 0 || !slices.Equal(start.Outgoing(), []string{"go"}) {
			t.Fatalf("start: %+v", start)
		}
		finish, ok := p.Node("fin")
		if !ok || finish.Kind != model.EndEvent || !slices.Equal(finish.Incoming(), []string{"go"}) || len(finish.Outgoing()) != 0 {
			t.Fatalf("end: %+v", finish)
		}
		f, ok := p.Flow("go")
		if !ok || f.Name != "Proceed" || f.SourceRef != start.ID || f.TargetRef != finish.ID {
			t.Fatalf("flow: %+v", f)
		}
	}
	check(p)
	// The input slice, all exported collections, and decoded copies are independent.
	want := slices.Clone(files)
	files[0] = model.Source{}
	p.Sources()[0] = model.Source{}
	p.Nodes()[0].ID = "bad"
	p.Flows()[0].TargetRef = "bad"
	n, _ := p.Node("entrée")
	n.Outgoing()[0] = "bad"
	files = want
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
}

func TestU2NamespacesAndNonExecutingMaterial(t *testing.T) {
	body := `<startEvent id="entrée" name="Begin"><outgoing> go </outgoing></startEvent>` +
		`<endEvent id="fin"><incoming>go</incoming></endEvent>` + edge
	plain := compile(t, model.NewSource(filename, document(body)))
	prefixed := `<m:definitions xmlns:m="` + ns + `" xmlns:z="http://camunda.org/schema/zeebe/1.0" xmlns:d="http://www.omg.org/spec/BPMN/20100524/DI">
		<m:process id="p" isExecutable="1"><m:documentation>Notes</m:documentation>
		<m:startEvent id="entrée" name="Begin"><m:extensionElements><z:properties/></m:extensionElements><m:outgoing>go</m:outgoing></m:startEvent>
		<m:endEvent id="fin"><m:incoming>go</m:incoming></m:endEvent><m:sequenceFlow id="go" name="Proceed" sourceRef="entrée" targetRef="fin"/>
		</m:process><d:BPMNDiagram id="diagram"><d:BPMNPlane id="plane" bpmnElement="p"/></d:BPMNDiagram></m:definitions>`
	p := compile(t, model.NewSource(filename, prefixed))
	if !reflect.DeepEqual(plain.Nodes(), p.Nodes()) || !slices.Equal(plain.Flows(), p.Flows()) || p.StartID() != plain.StartID() {
		t.Fatal("namespace/metadata changed executable graph")
	}
}

func TestU2InvalidGraph(t *testing.T) {
	for _, tc := range []struct{ label, body, id, field string }{
		{"duplicate node", begin + end + begin + edge, "entrée", "id"},
		{"flow shadows process", begin + end + strings.Replace(edge, `id="go"`, `id="p"`, 1), "p", "id"},
		{"definitions ID is not a node", begin + end + `<sequenceFlow id="f" sourceRef="defs" targetRef="fin"/>`, "f", "sourceRef"},
		{"dangling target", begin + end + strings.Replace(edge, "fin", "missing", 1), "go", "targetRef"},
		{"missing source", begin + end + strings.Replace(edge, ` sourceRef="entrée"`, "", 1), "go", "sourceRef"},
		{"false outgoing", `<startEvent id="entrée"><outgoing>bad</outgoing></startEvent>` + end + edge, "entrée", "outgoing"},
		{"repeated incoming", begin + `<endEvent id="fin"><incoming>go</incoming><incoming>go</incoming></endEvent>` + edge, "fin", "incoming"},
		{"unreachable end", begin + end + edge + `<endEvent id="island"/>`, "island", ""},
		{"two starts", begin + end + edge + `<startEvent id="again"/>`, "again", ""},
		{"no end", begin, "", ""},
		{"no start", end, "", ""},
		{"end as source", begin + end + edge + `<sequenceFlow id="again" sourceRef="fin" targetRef="fin"/>`, "fin", "outgoing"},
		{"two exits", begin + end + edge + `<sequenceFlow id="again" sourceRef="entrée" targetRef="fin"/>`, "entrée", "outgoing"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			d := rejected(t, model.NewSource(filename, document(tc.body)))
			if d.Code() != model.InvalidModel || d.File() != filename || d.Field() == "" ||
				(tc.id != "" && d.ElementID() != tc.id) || (tc.field != "" && d.Field() != tc.field) {
				t.Fatalf("unexpected diagnostic: %v", d)
			}
		})
	}
}

func TestU2Unsupported(t *testing.T) {
	for _, tc := range []struct{ body, id, field string }{
		{`<serviceTask id="task"/>`, "task", "serviceTask"},
		{`<inclusiveGateway id="join"/>`, "join", "inclusiveGateway"},
		{`<startEvent id="s"><messageEventDefinition/></startEvent>`, "s", "messageEventDefinition"},
		{`<endEvent id="e" xmlns:x="urn:vendor" x:listener="run"/>`, "e", "listener"},
		{`<startEvent xmlns="urn:foreign" id="foreign"/>`, "foreign", "startEvent"},
		{`<sequenceFlow id="f"><conditionExpression language="javascript">true</conditionExpression></sequenceFlow>`, "f", "language"},
		{`<startEvent id="s"><extensionElements><properties xmlns="http://camunda.org/schema/zeebe/1.0"><property/></properties></extensionElements></startEvent>`, "s", "property"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			d := rejected(t, model.NewSource(filename, document(tc.body)))
			if d.Code() != model.UnsupportedFeature || d.File() != filename || d.ElementID() != tc.id || d.Field() != tc.field {
				t.Fatalf("unexpected diagnostic: %v", d)
			}
			b, err := json.Marshal(d)
			var again model.Diagnostic
			if err != nil || json.Unmarshal(b, &again) != nil || again != d {
				t.Fatalf("diagnostic round trip: %s", b)
			}
		})
	}
}

func TestBundleValidation(t *testing.T) {
	xml := document(begin + end + edge)
	src := model.NewSource(filename, xml)
	for _, tc := range []struct {
		label string
		files []model.Source
	}{
		{"empty bundle", nil},
		{"duplicate filename", []model.Source{src, src}},
		{"two BPMN files", []model.Source{src, model.NewSource("second.bpmn", xml)}},
		{"logical name only", []model.Source{model.NewSource("../model.bpmn", xml)}},
		{"invalid UTF-8", []model.Source{model.NewSource(filename, xml+"\xff")}},
		{"nonempty packaging", []model.Source{src, model.NewSource(".process-application", `{"x":1,"x":2}`)}},
		{"false executable", []model.Source{model.NewSource(filename, strings.Replace(xml, `"true"`, `"false"`, 1))}},
		{"two executable", []model.Source{model.NewSource(filename, strings.Replace(xml, "</definitions>", `<process id="p2" isExecutable="true"/></definitions>`, 1))}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if d := rejected(t, tc.files...); d.Code() != model.InvalidModel {
				t.Fatal(d)
			}
		})
	}
	for _, name := range []string{"case.cmmn", "script.js"} {
		d := rejected(t, src, model.NewSource(name, ""))
		if d.Code() != model.UnsupportedFeature || d.File() != name {
			t.Fatal(d)
		}
	}
}

func TestXMLValidation(t *testing.T) {
	xml := document(begin + end + edge)
	withDiagram := strings.Replace(xml, "</definitions>", `<d:BPMNDiagram xmlns:d="http://www.omg.org/spec/BPMN/20100524/DI" id="picture"/></definitions>`, 1)
	for _, text := range []string{
		xml + "<another/>", xml + "trailing", xml[:len(xml)-1],
		`<!DOCTYPE definitions SYSTEM "never-read.dtd">` + xml,
		strings.Replace(xml, "A → B", "&undefined;", 1),
		strings.Replace(xml, `id="defs"`, `id="defs" id="other"`, 1),
		strings.Replace(withDiagram, `id="picture"`, `id="fin"`, 1),
		strings.Replace(withDiagram, `targetRef="fin"`, `targetRef="picture"`, 1),
	} {
		if d := rejected(t, model.NewSource(filename, text)); d.File() != filename {
			t.Fatal(d)
		}
	}
}

func TestNamespaceScope(t *testing.T) {
	for _, doc := range []string{
		document(strings.Replace(begin, `id=`, `xmlns:q="" q:id=`, 1) + end + edge),
		document(begin + end + edge + `<documentation><undeclared:note/></documentation>`),
		document(begin + end + edge + `<documentation xmlns:xml="urn:other"/>`),
		document(begin + end + edge + `<documentation xmlns:t="urn:note"><t:p/></documentation><documentation><t:p/></documentation>`),
	} {
		if d := rejected(t, model.NewSource(filename, doc)); d.Code() != model.InvalidModel {
			t.Fatal(d)
		}
	}
}

func TestExecutableProcessScope(t *testing.T) {
	xml := strings.Replace(document(begin+end+edge), "</definitions>",
		`<process id="sketch" isExecutable="false"><endEvent id="outside"/></process></definitions>`, 1)
	p := compile(t, model.NewSource(filename, xml))
	if _, found := p.Node("outside"); found {
		t.Fatal("non-executable node in plan")
	}
	for _, tc := range []struct{ old, new, id, field string }{
		{`targetRef="fin"`, `targetRef="outside"`, "go", "targetRef"},
		{`id="outside"`, `id="fin"`, "fin", "id"},
		{`isExecutable="false"`, `isExecutable=""`, "sketch", "isExecutable"},
	} {
		d := rejected(t, model.NewSource(filename, strings.Replace(xml, tc.old, tc.new, 1)))
		if d.Code() != model.InvalidModel || d.ElementID() != tc.id || d.Field() != tc.field {
			t.Fatal(d)
		}
	}
}
