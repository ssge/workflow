package evaluator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ssge/workflow/form"
	"github.com/ssge/workflow/internal/dmn"
)

func TestFixtureFormsUseExactFEELValues(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "acceptance", "release1", "form-textfield.form"))
	if err != nil {
		t.Fatal(err)
	}
	template, err := form.Parse(source, CompileForm)
	if err != nil {
		t.Fatal(err)
	}
	variables, err := ParseVariables(json.RawMessage(`{"input":"hello","executionCount":0.1}`))
	if err != nil {
		t.Fatal(err)
	}
	view, err := template.Render(variables)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := view.Components[0]["text"], "Caller says: hello"; got != want {
		t.Fatalf("caller text = %v, want %v", got, want)
	}
	if got, want := view.Components[1]["text"], "Execution: 0.1"; got != want {
		t.Fatalf("execution text = %v, want %v", got, want)
	}
	if got, want := view.Components[3]["label"], "или пък команда"; got != want {
		t.Fatalf("Unicode label = %v, want %v", got, want)
	}
}

func TestFixtureDMNUsesFirstMatch(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "acceptance", "release1", "looping_decision.dmn"))
	if err != nil {
		t.Fatal(err)
	}
	model, err := dmn.Parse("looping_decision.dmn", source, CompileDMN)
	if err != nil {
		t.Fatal(err)
	}
	decision, ok := model.Decision("Decision_0mepvum")
	if !ok {
		t.Fatal("fixture decision missing")
	}
	for _, test := range []struct {
		variables map[string]any
		want      string
	}{
		{map[string]any{"executionCount": json.Number("1"), "command": "stop"}, "loop"},
		{map[string]any{"executionCount": json.Number("1"), "text_command": "terminate"}, "break"},
		{map[string]any{"executionCount": json.Number("11")}, "break"},
		{map[string]any{"executionCount": json.Number("10"), "command": "continue"}, "loop"},
	} {
		got, err := decision.Evaluate(test.variables)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("Evaluate(%#v) = %v, want %q", test.variables, got, test.want)
		}
	}
}

func TestVariableParsingPreservesAndRejectsDecimalProfile(t *testing.T) {
	variables, err := ParseVariables(json.RawMessage(`{"value":0.1234567890123456789012345678901234}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := variables["value"].(json.Number).String(); got != "0.1234567890123456789012345678901234" {
		t.Fatalf("number changed: %s", got)
	}
	if _, err := ParseVariables(json.RawMessage(`{"value":0.12345678901234567890123456789012345}`)); err == nil {
		t.Fatal("accepted a decimal with too much precision")
	}
	if _, err := ParseVariables(json.RawMessage(`{"value":1,"value":2}`)); err == nil {
		t.Fatal("accepted duplicate object keys")
	}
}
