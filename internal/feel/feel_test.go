package feel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

func value(t *testing.T, text string) Value {
	t.Helper()
	v, err := ParseJSON([]byte(text))
	if err != nil {
		t.Fatalf("ParseJSON(%q): %v", text, err)
	}
	return v
}

func eval(t *testing.T, source string, vars map[string]Value, want string) {
	t.Helper()
	expr, err := Compile(source)
	if err != nil {
		t.Fatalf("Compile(%q): %v", source, err)
	}
	got, err := expr.Evaluate(vars)
	if err != nil {
		t.Fatalf("Evaluate(%q): %v", source, err)
	}
	if !got.Equal(value(t, want)) {
		t.Errorf("Evaluate(%q) = %s; want %s", source, jsonText(t, got), want)
	}
}

func jsonText(t *testing.T, v Value) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExpressions(t *testing.T) {
	vars := map[string]Value{
		"executionCount": value(t, "2"),
		"decision":       value(t, `"loop"`),
		"input":          value(t, `"hello"`),
		"nothing":        value(t, "null"),
		"object":         value(t, `{"a":[1,{"b":true}],"z":null}`),
		"other":          value(t, `{"z":null,"a":[1.00,{"b":true}]}`),
		"different":      value(t, `{"z":null,"a":[1,{"b":false}]}`),
	}
	for _, tc := range []struct{ source, want string }{
		{"0.1 + 0.2", "0.3"},
		{"9999999999999999999999999999999999 + 1", "10000000000000000000000000000000000"},
		{"10000000000000000000000000000000000 + -9999999999999999999999999999999999", "1"},
		{"1e-6143 + 1e-6143", "2e-6143"},
		{"1e6144 + -1e6144", "0"},
		{"executionCount + 1", "3"},
		{`"Execution: " + string(executionCount)`, `"Execution: 2"`},
		{`"Caller says: " + input`, `"Caller says: hello"`},
		{`decision = "loop"`, "true"},
		{"missing", "null"},
		{"missing = nothing", "true"},
		{"object = other", "true"},
		{"object = different", "false"},
		{`object = 1`, "false"},
		{`"1" = 1`, "false"},
		{"-4 < -3", "true"},
		{"-1 < 0", "true"},
		{"0 > -1", "true"},
		{"0 < 1e-6143", "true"},
		{"-1.0001 < -1", "true"},
		{"1.0 <= 1", "true"},
		{"1.1 > 1", "true"},
		{"1 >= 1.01", "false"},
		{"1 + 2 = 3", "true"},
		{"(1 = 1) = true", "true"},
		{`string(-100.0100)`, `"-100.01"`},
		{`string(1e3)`, `"1000"`},
		{`string(true) + string(false) + string(null)`, `"truefalsenull"`},
		{`string("a\n\uD83D\uDE00")`, `"a\n😀"`},
		{`"a" + "b" + "c"`, `"abc"`},
	} {
		t.Run(tc.source, func(t *testing.T) { eval(t, tc.source, vars, tc.want) })
	}
}

func TestDecimalArithmeticAgainstRational(t *testing.T) {
	random := rand.New(rand.NewSource(42))
	sum, err := Compile("a + b")
	if err != nil {
		t.Fatal(err)
	}
	less, err := Compile("a < b")
	if err != nil {
		t.Fatal(err)
	}
	for range 250 {
		raw := func() string {
			return fmt.Sprintf("%de%d", random.Int63n(2_000_000_000_000)-1_000_000_000_000, random.Intn(13)-6)
		}
		a, b := raw(), raw()
		exactA, okA := new(big.Rat).SetString(a)
		exactB, okB := new(big.Rat).SetString(b)
		if !okA || !okB {
			t.Fatalf("invalid oracle inputs: %s, %s", a, b)
		}
		vars := map[string]Value{"a": value(t, a), "b": value(t, b)}
		got, err := sum.Evaluate(vars)
		if err != nil {
			t.Fatalf("%s + %s: %v", a, b, err)
		}
		text, ok := got.AsNumber()
		actual, parsed := new(big.Rat).SetString(text)
		expected := new(big.Rat).Add(exactA, exactB)
		if !ok || !parsed || actual.Cmp(expected) != 0 {
			t.Fatalf("%s + %s = %s; expected %s", a, b, text, expected)
		}
		got, err = less.Evaluate(vars)
		if err != nil {
			t.Fatal(err)
		}
		result, ok := got.AsBoolean()
		if !ok || result != (exactA.Cmp(exactB) < 0) {
			t.Fatalf("incorrect comparison %s < %s", a, b)
		}
	}
}

func TestCompileMarkedAndSyntax(t *testing.T) {
	e, err := CompileMarked(`  = decision = "loop"  `)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Evaluate(map[string]Value{"decision": value(t, `"loop"`)})
	if err != nil || !got.Equal(value(t, "true")) {
		t.Fatalf("marked expression: %v, %v", got, err)
	}
	for _, source := range []string{
		"", "=1", "1 = 2 = 3", "foo(1)", "string()", "string(1, 2)",
		"1 - 2", "+1", "1 * 2", "a.b", "a[0]", "[1]", "{a:1}",
		"1 != 2", "1 == 2", "not true", "true and false",
		"01", "1.", ".1", "1e", "1e+", "--1",
		`"\x41"`, `"\uD800"`, `"unclosed`, "1 2",
		"12345678901234567890123456789012345", "1e6145", "1e-6144",
	} {
		t.Run(source, func(t *testing.T) {
			if _, err := Compile(source); !errors.Is(err, ErrSyntax) {
				t.Fatalf("Compile(%q) error = %v; want ErrSyntax", source, err)
			}
		})
	}
	for _, source := range []string{"1", " ", "== 1"} {
		if _, err := CompileMarked(source); !errors.Is(err, ErrSyntax) {
			t.Errorf("CompileMarked(%q) error = %v; want ErrSyntax", source, err)
		}
	}
}

func TestEvaluationErrors(t *testing.T) {
	vars := map[string]Value{
		"obj": value(t, "{}"),
		"arr": value(t, "[]"),
	}
	for _, source := range []string{
		`"1" + 1`, `1 + null`, `true + false`, `"a" < "b"`,
		"null < 1", "string(obj)", "string(arr)",
		"9999999999999999999999999999999999 + 2",
		"9e6144 + 1e6144", "1e-6143 + 1",
		"1.000000000000000000000000000000001e-6143 + -1e-6143",
	} {
		t.Run(source, func(t *testing.T) {
			expr, err := Compile(source)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if _, err = expr.Evaluate(vars); !errors.Is(err, ErrEvaluation) {
				t.Errorf("Evaluate error = %v; want ErrEvaluation", err)
			}
		})
	}
}

func TestNumbersAndJSON(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"0.1000", "0.1"}, {"-0e999999999999999999999", "0"},
		{"1e+3", "1000"}, {"10000000000000000000000000000000000", "10000000000000000000000000000000000"},
		{"1234567890123456789012345678901234", "1234567890123456789012345678901234"},
		{"1e6144", "1" + strings.Repeat("0", 6144)},
		{"1e-6143", "0." + strings.Repeat("0", 6142) + "1"},
	} {
		v := value(t, tc.input)
		if got := jsonText(t, v); got != tc.want {
			t.Errorf("number %q serialized as %q, want %q", tc.input, got, tc.want)
		}
	}
	for _, input := range []string{
		`{"a":1,"a":2}`, `{"a":{"b":1,"\u0062":2}}`, `{"a":1} {}`,
		`{"a":1,}`, `[1,]`, `"\uDC00"`, `"unescaped
line"`,
		`{"a":1e6145}`, `1e-6144`, `1e999999999999999999999`,
		`12345678901234567890123456789012345`, `"bad\uD800"`, string([]byte{'"', 0xff, '"'}),
	} {
		if _, err := ParseJSON([]byte(input)); !errors.Is(err, ErrInvalidValue) {
			t.Errorf("ParseJSON(%q) error = %v; want ErrInvalidValue", input, err)
		}
	}
}

func TestJSONUnmarshal(t *testing.T) {
	var got Value
	if err := json.Unmarshal([]byte(`{"n":0.1000000000000000000000000000000001,"a":[true,null]}`), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(value(t, `{"a":[true,null],"n":0.1000000000000000000000000000000001}`)) {
		t.Fatalf("unmarshaled wrong value: %s", jsonText(t, got))
	}
	original := got
	if err := json.Unmarshal([]byte(`{"bad":1,"bad":2}`), &got); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("duplicate error = %v", err)
	}
	if !got.Equal(original) {
		t.Fatal("invalid JSON changed existing value")
	}
}

func TestObjectsMissingAndImmutability(t *testing.T) {
	object := value(t, `{"present":null,"arr":[1],"o":{"x":"ok"}}`)
	if v, ok := object.Get("present"); !ok || !v.Equal(value(t, "null")) {
		t.Fatal("explicit null not present")
	}

	if _, ok := object.Get("absent"); ok {
		t.Fatal("absent key reported present")
	}
	members, ok := object.Members()
	if !ok {
		t.Fatal("expected object")
	}
	delete(members, "present")
	if _, ok := object.Get("present"); !ok {
		t.Fatal("Members exposed mutable object")
	}
	arr, _ := object.Get("arr")
	elements, ok := arr.Elements()
	if !ok {
		t.Fatal("expected array")
	}
	elements[0] = value(t, "2")
	if got := jsonText(t, arr); got != "[1]" {
		t.Fatalf("Elements exposed mutable array: %s", got)
	}
}

func TestValueConstructorsAndIdentifiers(t *testing.T) {
	one, err := Number("1.00")
	if err != nil {
		t.Fatal(err)
	}
	text, err := String("text")
	if err != nil {
		t.Fatal(err)
	}
	elements := []Value{one}
	array := Array(elements)
	elements[0] = text
	members := map[string]Value{"value": array}
	object, err := Object(members)
	if err != nil {
		t.Fatal(err)
	}
	delete(members, "value")
	if got := jsonText(t, object); got != `{"value":[1]}` {
		t.Fatalf("constructors did not copy: %s", got)
	}
	if _, err := String(string([]byte{0xff})); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid UTF-8 string: %v", err)
	}
	if _, err := Object(map[string]Value{string([]byte{0xff}): Null()}); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid UTF-8 key: %v", err)
	}
	for _, name := range []string{"a", "_1", "a_B2", "string"} {
		if !ValidIdentifier(name) {
			t.Errorf("%q should be a valid identifier", name)
		}
	}
	for _, name := range []string{"", "1a", "a-b", "a.b", "é", "true", "false", "null"} {
		if ValidIdentifier(name) {
			t.Errorf("%q should not be a valid identifier", name)
		}
	}
}

func TestGoValueBridgePreservesDecimals(t *testing.T) {
	value, err := FromInterface(map[string]any{
		"amount": json.Number("0.3000000000000000000000000000000001"),
		"nested": []any{nil, true, "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value.Interface())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"amount":0.3000000000000000000000000000000001,"nested":[null,true,"text"]}`; got != want {
		t.Fatalf("round trip = %s, want %s", got, want)
	}
	if _, err := FromInterface(0.1); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("binary float was accepted: %v", err)
	}
	bindings, err := Bindings(map[string]any{"count": json.Number("2")})
	if err != nil {
		t.Fatal(err)
	}
	expression, err := Compile("count + 1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := expression.Evaluate(bindings)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := result.AsNumber(); !ok || got != "3" {
		t.Fatalf("evaluated number = %q, %t", got, ok)
	}
}
