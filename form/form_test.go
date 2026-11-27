package form

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func fixtureCompiler(source string) (Evaluator, error) {
	switch source {
	case `"Caller says: " + input`:
		return func(vars map[string]any) (any, error) {
			s, ok := vars["input"].(string)
			if !ok {
				return nil, errors.New("input is not a string")
			}
			return "Caller says: " + s, nil
		}, nil
	case `"Execution: " + string(executionCount)`:
		return func(vars map[string]any) (any, error) {
			count, ok := vars["executionCount"].(int)
			if !ok {
				return nil, errors.New("executionCount is not a number")
			}
			return fmt.Sprintf("Execution: %d", count), nil
		}, nil
	default:
		return nil, errors.New("invalid expression")
	}
}

func fixture(t *testing.T) *Template {
	t.Helper()
	data, err := os.ReadFile("../acceptance/release1/form-textfield.form")
	if err != nil {
		t.Fatal(err)
	}
	form, err := Parse(data, fixtureCompiler)
	if err != nil {
		t.Fatal(err)
	}
	return form
}

func TestU4FixtureRenderAndSubmit(t *testing.T) {
	f := fixture(t)
	vars := map[string]any{"input": "hello", "executionCount": 2}
	view, err := f.Render(vars)
	if err != nil {
		t.Fatal(err)
	}
	if view.FormID != "Form_0w2uzn0" || len(view.Components) != 5 {
		t.Fatalf("unexpected view: %#v", view)
	}
	expected := []ComponentView{
		{"id": "Field_1mpfgnn", "type": "text", "text": "Caller says: hello", "layout": map[string]any{"row": "Row_0etr9ah", "columns": nil}},
		{"id": "Field_0b36nks", "type": "text", "text": "Execution: 2", "layout": map[string]any{"row": "Row_0etr9ah", "columns": nil}},
		{"id": "Field_1u8gtqz", "type": "select", "label": "Select", "key": "command", "value": "continue", "values": []Option{{"Continue", "continue"}, {"Stop", "stop"}, {"terminate", "terminate"}}, "layout": map[string]any{"row": "Row_0tmzh07", "columns": nil}},
		{"id": "Field_0sj8zfe", "type": "textfield", "label": "или пък команда", "key": "text_command", "value": nil, "layout": map[string]any{"row": "Row_1a4y7in", "columns": nil}},
		{"id": "Field_00asub0", "type": "button", "label": "Done", "action": "submit", "layout": map[string]any{"row": "Row_0rbh6hf", "columns": json.Number("4")}},
	}
	if !reflect.DeepEqual(view.Components, expected) {
		t.Fatalf("got %#v\nwant %#v", view.Components, expected)
	}
	if len(vars) != 2 {
		t.Fatalf("render wrote defaults: %#v", vars)
	}
	writes, fields := f.ValidateSubmission(map[string]any{"command": "stop"})
	if len(fields) != 0 || !reflect.DeepEqual(writes, map[string]any{"command": "stop"}) {
		t.Fatalf("submission: %#v %#v", writes, fields)
	}
	writes, fields = f.ValidateSubmission(map[string]any{"command": "bad", "other": "x", "text_command": "ok"})
	if writes != nil || len(fields) != 2 || fields[0].Key != "command" || fields[1].Key != "other" {
		t.Fatalf("invalid submission: %#v %#v", writes, fields)
	}
	second, err := f.Render(map[string]any{"input": "bye", "executionCount": 3, "command": "stop", "text_command": "yes"})
	if err != nil || second.Components[0]["text"] != "Caller says: bye" || second.Components[2]["value"] != "stop" || second.Components[3]["value"] != "yes" {
		t.Fatalf("second render: %#v %v", second, err)
	}
	second.Components[2]["values"].([]Option)[0].Label = "changed"
	second.Components[0]["layout"].(map[string]any)["row"] = "changed"
	again, err := f.Render(vars)
	if err != nil || !reflect.DeepEqual(view, again) {
		t.Fatalf("render leaked state: %#v %v", again, err)
	}
}

func TestRenderBoundValuesAndEvaluation(t *testing.T) {
	f := fixture(t)
	for _, vars := range []map[string]any{
		{"input": "x", "executionCount": 2, "command": "bad"},
		{"input": "x", "executionCount": 2, "command": 1},
		{"input": "x", "executionCount": 2, "text_command": false},
		{"executionCount": 2},
	} {
		if view, err := f.Render(vars); err == nil || len(view.Components) != 0 {
			t.Fatalf("accepted bad binding %#v", vars)
		}
	}
	view, err := f.Render(map[string]any{"input": "x", "executionCount": 2, "command": nil})
	if err != nil || view.Components[2]["value"] != "continue" {
		t.Fatalf("null binding should use default: %#v %v", view, err)
	}
	f, err = Parse([]byte(`{"type":"default","id":"f","schemaVersion":19,"components":[{"type":"text","text":"= 1"}]}`),
		func(string) (Evaluator, error) { return func(map[string]any) (any, error) { return 1, nil }, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Render(nil); err == nil {
		t.Fatal("accepted non-string dynamic text")
	}
}

func TestSubmissionOmissionsAndTypes(t *testing.T) {
	f := fixture(t)
	writes, fields := f.ValidateSubmission(map[string]any{})
	if len(fields) != 0 || len(writes) != 0 {
		t.Fatalf("empty submit: %#v %#v", writes, fields)
	}
	for _, value := range []any{nil, true, 1, []any{"stop"}} {
		writes, fields := f.ValidateSubmission(map[string]any{"command": value})
		if writes != nil || len(fields) != 1 || fields[0].Key != "command" {
			t.Fatalf("accepted %#v: %#v %#v", value, writes, fields)
		}
	}
}

func TestOptionalValuesAndLiteralText(t *testing.T) {
	source := `{"id":"f","type":"default","schemaVersion":19,"components":[
		{"type":"text","text":" literal = text ","layout":{"row":"r","columns":null,"annotation":"ignored"}},
		{"type":"select","key":"choice","values":[{"label":"A","value":"a"}]},
		{"type":"textfield","key":"message"},
		{"type":"button","label":"送信","action":"submit"}]}`
	f, err := Parse([]byte(source), nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.Render(nil)
	if err != nil {
		t.Fatal(err)
	}
	if view.Components[0]["text"] != " literal = text " || view.Components[1]["value"] != nil || view.Components[2]["value"] != nil || view.Components[3]["label"] != "送信" {
		t.Fatalf("unexpected view: %#v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil || !strings.Contains(string(encoded), `"value":null`) || strings.Contains(string(encoded), `"annotation"`) {
		t.Fatalf("unexpected JSON: %s %v", encoded, err)
	}
}

func TestDynamicTextCompilerContract(t *testing.T) {
	source := []byte(`{"id":"f","type":"default","schemaVersion":19,"components":[{"id":"a","type":"text","text":" \t=  \"x\"  "}]}`)
	var got string
	f, err := Parse(source, func(s string) (Evaluator, error) {
		got = s
		return func(map[string]any) (any, error) { return "x", nil }, nil
	})
	if err != nil || got != `"x"` {
		t.Fatalf("compile source %q, error %v", got, err)
	}
	view, err := f.Render(nil)
	if err != nil || view.Components[0]["text"] != "x" {
		t.Fatalf("render %#v, error %v", view, err)
	}
	if _, err := Parse(source, nil); err == nil {
		t.Fatal("dynamic text compiled without a compiler")
	}
	if _, err := Parse(source, func(string) (Evaluator, error) { return nil, nil }); err == nil {
		t.Fatal("accepted a nil evaluator")
	}
}

func TestParseRejectsInvalidAndUnsupported(t *testing.T) {
	valid := `{"id":"f","type":"default","schemaVersion":19,"components":[{"id":"s","type":"select","key":"choice","values":[{"label":"A","value":"a"}]}]}`
	tests := []struct{ name, source, code string }{
		{"invalid UTF-8", string([]byte{0xff}), InvalidModel},
		{"invalid JSON", `{"id":`, InvalidModel},
		{"duplicate root key", strings.Replace(valid, `"id":"f"`, `"id":"f","id":"g"`, 1), InvalidModel},
		{"duplicate nested key", strings.Replace(valid, `"label":"A"`, `"label":"A","label":"B"`, 1), InvalidModel},
		{"trailing data", valid + `{}`, InvalidModel},
		{"wrong schema", strings.Replace(valid, `"schemaVersion":19`, `"schemaVersion":18`, 1), UnsupportedFeature},
		{"string schema", strings.Replace(valid, `"schemaVersion":19`, `"schemaVersion":"19"`, 1), InvalidModel},
		{"fractional schema", strings.Replace(valid, `"schemaVersion":19`, `"schemaVersion":19.0`, 1), InvalidModel},
		{"missing form ID", strings.Replace(valid, `"id":"f",`, "", 1), InvalidModel},
		{"missing components", strings.Replace(valid, `,"components":[{"id":"s","type":"select","key":"choice","values":[{"label":"A","value":"a"}]}]`, "", 1), InvalidModel},
		{"unknown component", strings.Replace(valid, `"select"`, `"checkbox"`, 1), UnsupportedFeature},
		{"unknown logic", strings.Replace(valid, `"key":"choice"`, `"key":"choice","conditional":{}`, 1), UnsupportedFeature},
		{"unknown top logic", strings.Replace(valid, `"id":"f"`, `"id":"f","conditions":[]`, 1), UnsupportedFeature},
		{"invalid key", strings.Replace(valid, `"choice"`, `"a.b"`, 1), InvalidModel},
		{"reserved key", strings.Replace(valid, `"choice"`, `"null"`, 1), InvalidModel},
		{"missing key", strings.Replace(valid, `,"key":"choice"`, "", 1), InvalidModel},
		{"duplicate values", strings.Replace(valid, `{"label":"A","value":"a"}`, `{"label":"A","value":"a"},{"label":"B","value":"a"}`, 1), InvalidModel},
		{"bad default", strings.Replace(valid, `"key":"choice"`, `"key":"choice","defaultValue":"missing"`, 1), InvalidModel},
		{"bad option", strings.Replace(valid, `"value":"a"`, `"value":1`, 1), InvalidModel},
		{"missing options", strings.Replace(valid, `,"values":[{"label":"A","value":"a"}]`, "", 1), InvalidModel},
		{"empty options", strings.Replace(valid, `[{"label":"A","value":"a"}]`, `[]`, 1), InvalidModel},
		{"duplicate inputs", strings.Replace(valid, `]}]}`, `]},{"type":"textfield","key":"choice"}]}`, 1), InvalidModel},
		{"duplicate component IDs", strings.Replace(valid, `]}]}`, `]},{"id":"s","type":"text","text":"x"}]}`, 1), InvalidModel},
		{"wrong button action", `{"id":"f","type":"default","schemaVersion":19,"components":[{"type":"button","action":"reset"}]}`, UnsupportedFeature},
		{"invalid expression", `{"id":"f","type":"default","schemaVersion":19,"components":[{"type":"text","text":"= nope"}]}`, InvalidModel},
		{"invalid layout", strings.Replace(valid, `"key":"choice"`, `"key":"choice","layout":{"columns":"4"}`, 1), InvalidModel},
		{"wrong field for component", strings.Replace(valid, `"key":"choice"`, `"key":"choice","text":"oops"`, 1), UnsupportedFeature},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.source), fixtureCompiler)
			var modelErr *Error
			if !errors.As(err, &modelErr) || modelErr.Code != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
		})
	}
}
