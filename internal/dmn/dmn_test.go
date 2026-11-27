package dmn_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ssge/workflow/internal/dmn"
)

const namespace = "https://www.omg.org/spec/DMN/20191111/MODEL/"

// Each test registers the exact FEEL sources it expects DMN to compile.
type expressions map[string]dmn.Expression

func (e expressions) compile(source string) (dmn.Expression, error) {
	if expr, ok := e[source]; ok {
		return expr, nil
	}
	return nil, fmt.Errorf("unexpected FEEL source %q", source)
}

func variable(name string) dmn.Expression {
	return func(vars map[string]any) (any, error) { return vars[name], nil }
}

func constant(value any) dmn.Expression {
	return func(map[string]any) (any, error) { return value, nil }
}

func equals(value any) dmn.Expression {
	return func(vars map[string]any) (any, error) { return vars["_dmn_cell"] == value, nil }
}

func numberTest(bound string, pass func(int) bool) dmn.Expression {
	target, _ := new(big.Rat).SetString(bound)
	return func(vars map[string]any) (any, error) {
		n, ok := vars["_dmn_cell"].(json.Number)
		if !ok {
			return nil, fmt.Errorf("comparison received %T", vars["_dmn_cell"])
		}
		value, ok := new(big.Rat).SetString(n.String())
		if !ok {
			return nil, fmt.Errorf("invalid decimal: %q", n)
		}
		return pass(value.Cmp(target)), nil
	}
}

func TestU3LoopingDecision(t *testing.T) {
	const file = "looping_decision.dmn"
	source, err := os.ReadFile("../../acceptance/release1/" + file)
	if err != nil {
		t.Fatal(err)
	}
	model, err := dmn.Parse(file, source, expressions{
		"executionCount":          variable("executionCount"),
		"command":                 variable("command"),
		"text_command":            variable("text_command"),
		`"break"`:                 constant("break"),
		`"loop"`:                  constant("loop"),
		`_dmn_cell = "break"`:     equals("break"),
		`_dmn_cell = "loop"`:      equals("loop"),
		`_dmn_cell = "stop"`:      equals("stop"),
		`_dmn_cell = "terminate"`: equals("terminate"),
		"_dmn_cell > 10":          numberTest("10", func(c int) bool { return c > 0 }),
		"_dmn_cell < 3":           numberTest("3", func(c int) bool { return c < 0 }),
	}.compile)
	if err != nil {
		t.Fatal(err)
	}
	decision, ok := model.Decision("Decision_0mepvum")
	if !ok {
		t.Fatal("fixture decision was not indexed")
	}

	for _, tc := range []struct {
		name                string
		count               json.Number
		command, text, want any
	}{
		{"early count beats stop", "1", "stop", nil, "loop"},
		{"text terminate beats early count", "1", nil, "terminate", "break"},
		{"stop at threshold", "3", "stop", nil, "break"},
		{"continue at ten", "10", "continue", nil, "loop"},
		{"count past ten", "11", "continue", nil, "break"},
		{"command terminate beats early count", "1", "terminate", nil, "break"},
		{"text stop", "3", nil, "stop", "break"},
		{"missing inputs", "", nil, nil, "loop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]any{"text_command": tc.text}
			if tc.count != "" {
				vars["executionCount"] = tc.count
			}
			if tc.command != nil {
				vars["command"] = tc.command
			}
			checkResult(t, decision, vars, tc.want)
		})
	}
}

func TestFirstMatchAndInputEvaluationOrder(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, want string
		secondInput               bool
	}{
		{"specific first", `<text>"yes"</text>`, `<text> - </text>`, "first", true},
		{"wildcard first", "", `<text>"yes"</text>`, "first", true},
		{"first column fails", `<text>"no"</text>`, `<text>   </text>`, "second", true},
		{"second column fails", `<text>"yes"</text>`, `<text> - </text>`, "second", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			record := func(name string, value any) dmn.Expression {
				return func(map[string]any) (any, error) {
					calls = append(calls, name)
					return value, nil
				}
			}
			decision := parseDecision(t, table(
				input("string", "readFirst")+input("boolean", "readSecond"),
				output("string", ""),
				`<rule id="ruleZ"><inputEntry>`+tc.first+`</inputEntry><inputEntry><text>true</text></inputEntry>`+
					`<outputEntry><text>firstResult</text></outputEntry></rule>`+
					`<rule id="ruleA"><inputEntry>`+tc.second+`</inputEntry><inputEntry><text>-</text></inputEntry>`+
					`<outputEntry><text>secondResult</text></outputEntry></rule>`,
			), expressions{
				"readFirst":         record("first input", "yes"),
				"readSecond":        record("second input", tc.secondInput),
				"firstResult":       record("first", "first"),
				"secondResult":      record("second", "second"),
				`_dmn_cell = "yes"`: equals("yes"),
				`_dmn_cell = "no"`:  equals("no"),
				`_dmn_cell = true`:  equals(true),
			})
			checkResult(t, decision, nil, tc.want)
			if want := []string{"first input", "second input", tc.want}; !reflect.DeepEqual(calls, want) {
				t.Fatalf("evaluation order = %q, want %q", calls, want)
			}
		})
	}
}

func TestUnaryTestsAndNoMatch(t *testing.T) {
	const large = "9999999999999999999999999999999998"
	for _, tc := range []struct {
		name, test, source string
		expr               dmn.Expression
		in                 any
		want               any
	}{
		{"less", `&lt; 2`, "_dmn_cell < 2", numberTest("2", func(c int) bool { return c < 0 }), json.Number("-1"), true},
		{"less excludes bound", `&lt; 2`, "_dmn_cell < 2", numberTest("2", func(c int) bool { return c < 0 }), json.Number("2"), nil},
		{"less or equal", `&lt;= 2`, "_dmn_cell <= 2", numberTest("2", func(c int) bool { return c <= 0 }), json.Number("2.0"), true},
		{"greater exact", `&gt;` + large, "_dmn_cell > " + large,
			numberTest(large, func(c int) bool { return c > 0 }), json.Number("9999999999999999999999999999999999"), true},
		{"greater or equal", `&gt;= -1.2`, "_dmn_cell >= -1.2", numberTest("-1.2", func(c int) bool { return c >= 0 }), json.Number("-1.20"), true},
		{"null not compared", `&gt; 2`, "_dmn_cell > 2", numberTest("2", func(c int) bool { return c > 0 }), nil, nil},
		{"literal equal magnitude", `1.2`, "_dmn_cell = 1.2", numberTest("1.2", func(c int) bool { return c == 0 }), json.Number("1.20"), true},
		{"null matches null", "null", "_dmn_cell = null", equals(nil), nil, true},
		{"null not equal to number", "null", "_dmn_cell = null", equals(nil), json.Number("1"), nil},
		{"null matches wildcard", "-", "", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := parseDecision(t, table(input("number", "x"), output("boolean", ""), rule(tc.test, "true")),
				expressions{"x": variable("x"), "true": constant(true), tc.source: tc.expr})
			checkResult(t, decision, map[string]any{"x": tc.in}, tc.want)
		})
	}
}

func TestEvaluationTypesAndOutputValues(t *testing.T) {
	const allowed = `"a,b", "a\"b", "c"`
	for _, tc := range []struct {
		name             string
		in, result       any
		outputType, list string
		field, reason    string
	}{
		{name: "string containing comma", in: json.Number("0"), result: "a,b", outputType: "string", list: allowed},
		{name: "escaped quote", result: `a"b`, outputType: "string", list: allowed},
		{name: "number membership by magnitude", result: json.Number("1.20"), outputType: "number", list: "1.2"},
		{name: "boolean membership", result: false, outputType: "boolean", list: "true,false"},
		{name: "wrong input despite wildcard", in: "0", result: "c", outputType: "string", field: "inputExpression", reason: "number"},
		{name: "no float coercion", in: 0.1, result: "c", outputType: "string", field: "inputExpression", reason: "number"},
		{name: "wrong output type", result: true, outputType: "string", list: allowed, field: "outputEntry", reason: "string"},
		{name: "null is not a typed output", outputType: "number", field: "outputEntry", reason: "number"},
		{name: "not in outputValues", result: "d", outputType: "string", list: allowed, field: "outputEntry", reason: "outputValues"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := parseDecision(t, table(input("number", "x"), output(tc.outputType, tc.list), rule("-", "result")),
				expressions{
					"x":                  variable("x"),
					"result":             variable("result"),
					`_dmn_cell = "a,b"`:  equals("a,b"),
					`_dmn_cell = "a\"b"`: equals(`a"b`),
					`_dmn_cell = "c"`:    equals("c"),
					`_dmn_cell = 1.2`:    numberTest("1.2", func(c int) bool { return c == 0 }),
					`_dmn_cell = true`:   equals(true),
					`_dmn_cell = false`:  equals(false),
				})
			vars := map[string]any{"x": tc.in, "result": tc.result}
			if tc.field == "" {
				checkResult(t, decision, vars, tc.result)
				return
			}
			got, err := decision.Evaluate(vars)
			if got != nil {
				t.Fatalf("failing decision returned %v", got)
			}
			checkError(t, err, dmn.EvaluationError, tc.field, tc.reason)
		})
	}
}

func TestIndependentDecisionsWithChangedPrefix(t *testing.T) {
	body := table(input("number", "x"), output("boolean", ""), rule("-", "true"))
	second := strings.NewReplacer(`id="d"`, `id="d2"`, `<text>true</text>`, `<text>false</text>`).Replace(body)
	src := `<p:definitions xmlns:p="` + namespace + `">` +
		strings.NewReplacer("</", "</p:", "<", "<p:").Replace(body+second) + `</p:definitions>`
	model, err := dmn.Parse("test.dmn", []byte(src), expressions{
		"x": variable("x"), "true": constant(true), "false": constant(false),
	}.compile)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"d", "d2"} {
		d, ok := model.Decision(id)
		if !ok {
			t.Fatalf("missing decision %q", id)
		}
		checkResult(t, d, nil, i == 0)
	}
}

func TestInvalidModels(t *testing.T) {
	base := document(table(input("number", "x"), output("boolean", ""), rule("-", "true")))
	for _, tc := range []struct {
		name, before, after, code, field, reason string
	}{
		{"malformed XML", `</definitions>`, `</other>`, dmn.InvalidModel, "xml", "XML"},
		{"DTD", `<definitions`, `<!DOCTYPE definitions><definitions`, dmn.UnsupportedFeature, "xml", "directive"},
		{"trailing root", `</definitions>`, `</definitions><other/>`, dmn.InvalidModel, "xml", "root"},
		{"wrong root namespace", namespace, "urn:other", dmn.UnsupportedFeature, "definitions", "namespace"},
		{"wrong table namespace", `<decisionTable`, `<decisionTable xmlns="urn:other"`, dmn.UnsupportedFeature, "decisionTable", "namespace"},
		{"duplicate ID", `<inputEntry>`, `<inputEntry id="d">`, dmn.InvalidModel, "id", "duplicate"},
		{"duplicate attribute", `<decision id="d">`, `<decision id="d" id="e">`, dmn.InvalidModel, "id", "duplicate"},
		{"missing ID", ` id="d"`, "", dmn.InvalidModel, "id", "required"},
		{"requirement", `<decision id="d">`, `<decision id="d"><informationRequirement/>`, dmn.UnsupportedFeature, "informationRequirement", "not supported"},
		{"boxed expression", `<decision id="d">`, `<decision id="d"><literalExpression/>`, dmn.UnsupportedFeature, "literalExpression", "not supported"},
		{"external import", `</definitions>`, `<import locationURI="https://invalid.example"/></definitions>`, dmn.UnsupportedFeature, "import", "not supported"},
		{"collect", `FIRST`, `COLLECT`, dmn.UnsupportedFeature, "hitPolicy", "FIRST"},
		{"implicit hit policy", ` hitPolicy="FIRST"`, "", dmn.InvalidModel, "hitPolicy", "required"},
		{"aggregation", `hitPolicy="FIRST"`, `hitPolicy="FIRST" aggregation="SUM"`, dmn.UnsupportedFeature, "aggregation", "not supported"},
		{"foreign hit policy attribute", `hitPolicy="FIRST"`, `xmlns:other="urn:other" other:hitPolicy="FIRST"`, dmn.UnsupportedFeature, "hitPolicy", "namespace"},
		{"unknown dialect", `<inputExpression`, `<inputExpression expressionLanguage="javascript"`, dmn.UnsupportedFeature, "expressionLanguage", "FEEL"},
		{"nonempty extension", `<decision id="d">`, `<decision id="d"><extensionElements><handler/></extensionElements>`, dmn.UnsupportedFeature, "extensionElements", "empty"},
		{"no input", input("number", "x"), "", dmn.InvalidModel, "input", "at least one"},
		{"two outputs", output("boolean", ""), output("boolean", "") + output("boolean", ""), dmn.InvalidModel, "output", "one"},
		{"unnamed output", ` name="result"`, "", dmn.InvalidModel, "name", "required"},
		{"unknown input type", `typeRef="number"`, `typeRef="date"`, dmn.UnsupportedFeature, "typeRef", "date"},
		{"unknown output type", `typeRef="boolean"`, `typeRef="Any"`, dmn.UnsupportedFeature, "typeRef", "Any"},
		{"untyped input", ` typeRef="number"`, "", dmn.InvalidModel, "typeRef", "required"},
		{"missing input entry", `<inputEntry><text>-</text></inputEntry>`, "", dmn.InvalidModel, "inputEntry", "expected 1"},
		{"extra output entry", `</rule>`, `<outputEntry><text>true</text></outputEntry></rule>`, dmn.InvalidModel, "outputEntry", "expected 1"},
		{"unsupported operator", `<text>-</text>`, `<text>!= 1</text>`, dmn.UnsupportedFeature, "inputEntry", "literal"},
		{"range", `<text>-</text>`, `<text>[1..2]</text>`, dmn.UnsupportedFeature, "inputEntry", "literal"},
		{"nonliteral bound", `<text>-</text>`, `<text>&gt; x</text>`, dmn.UnsupportedFeature, "inputEntry", "numeric literal"},
		{"numeric operator on string column", input("number", "x") + output("boolean", "") + rule("-", "true"),
			input("string", "x") + output("boolean", "") + rule("&gt; 1", "true"), dmn.InvalidModel, "inputEntry", "number column"},
		{"invalid outputValues syntax", output("boolean", ""), output("boolean", "true,"), dmn.InvalidModel, "outputValues", "literal"},
		{"nonliteral outputValues", output("boolean", ""), output("boolean", "true, x"), dmn.InvalidModel, "outputValues", "literal"},
		{"wrong outputValues type", output("boolean", ""), output("boolean", `true, "x"`), dmn.InvalidModel, "outputValues", "boolean"},
		{"empty outputValues", output("boolean", ""), `<output name="result" typeRef="boolean"><outputValues><text/></outputValues></output>`, dmn.InvalidModel, "outputValues", "required"},
		{"invalid input FEEL", `<text>x</text>`, `<text>f(x)</text>`, dmn.InvalidModel, "inputExpression", "unexpected FEEL"},
		{"BPMN expression marker", `<text>x</text>`, `<text>=x</text>`, dmn.InvalidModel, "inputExpression", "marker"},
		{"invalid output FEEL in unselected rule", `</decisionTable>`, rule("-", "f(x)") + `</decisionTable>`, dmn.InvalidModel, "outputEntry", "unexpected FEEL"},
		{"literal rejected by FEEL compiler", output("boolean", ""), output("number", "1e7000"), dmn.InvalidModel, "outputValues", "unexpected FEEL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(base, tc.before) {
				t.Fatalf("invalid test replacement %q", tc.before)
			}
			src := strings.Replace(base, tc.before, tc.after, 1)
			model, err := dmn.Parse("test.dmn", []byte(src),
				expressions{"x": variable("x"), "true": constant(true), "_dmn_cell = true": equals(true)}.compile)
			if model != nil {
				t.Fatal("invalid source returned a model")
			}
			checkError(t, err, tc.code, tc.field, tc.reason)
		})
	}
}

func TestEvaluatorErrorKeepsOrigin(t *testing.T) {
	issue := errors.New("bad operands")
	decision := parseDecision(t, table(input("number", "x"), output("boolean", ""),
		`<rule><inputEntry/><outputEntry id="rhs"><text>result</text></outputEntry></rule>`), expressions{
		"x":      variable("x"),
		"result": func(map[string]any) (any, error) { return nil, issue },
	})
	_, err := decision.Evaluate(nil)
	diagnostic := checkError(t, err, dmn.EvaluationError, "outputEntry", "bad operands")
	if !errors.Is(err, issue) || diagnostic.ElementID != "rhs" {
		t.Fatalf("lost error cause or location: %+v", diagnostic)
	}
}

func document(body string) string {
	return `<definitions xmlns="` + namespace + `">` + body + `</definitions>`
}

func table(inputs, outputs, rules string) string {
	return `<decision id="d"><decisionTable hitPolicy="FIRST">` + inputs + outputs + rules + `</decisionTable></decision>`
}

func input(typ, source string) string {
	return `<input><inputExpression typeRef="` + typ + `"><text>` + source + `</text></inputExpression></input>`
}

func output(typ, list string) string {
	o := `<output name="result" typeRef="` + typ + `">`
	if list != "" {
		o += `<outputValues><text>` + list + `</text></outputValues>`
	}
	return o + `</output>`
}

func rule(test, result string) string {
	return `<rule><inputEntry><text>` + test + `</text></inputEntry>` +
		`<outputEntry><text>` + result + `</text></outputEntry></rule>`
}

func parseDecision(t *testing.T, body string, e expressions) *dmn.Decision {
	t.Helper()
	model, err := dmn.Parse("test.dmn", []byte(document(body)), e.compile)
	if err != nil {
		t.Fatal(err)
	}
	decision, ok := model.Decision("d")
	if !ok {
		t.Fatal("missing decision d")
	}
	return decision
}

func checkResult(t *testing.T, d *dmn.Decision, vars map[string]any, want any) {
	t.Helper()
	if got, err := d.Evaluate(vars); err != nil || got != want {
		t.Fatalf("Evaluate(%v) = (%v, %v), want (%v, nil)", vars, got, err, want)
	}
}

func checkError(t *testing.T, err error, code, field, reason string) *dmn.Error {
	t.Helper()
	var diagnostic *dmn.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != code || diagnostic.File != "test.dmn" ||
		diagnostic.Field != field || !strings.Contains(diagnostic.Reason, reason) {
		t.Fatalf("error = %v, want %s at test.dmn:%s containing %q", err, code, field, reason)
	}
	return diagnostic
}
