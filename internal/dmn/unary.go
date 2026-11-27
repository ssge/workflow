package dmn

import (
	"regexp"
	"strings"
)

const cellName = "_dmn_cell"

type cellTest struct {
	predicate compiledExpression // nil eval denotes a wildcard
	ordering  bool
}

func (t cellTest) matches(value any) (bool, error) {
	if t.predicate.eval == nil {
		return true, nil
	}
	if t.ordering && value == nil {
		return false, nil
	}
	result, err := t.predicate.evaluate(map[string]any{cellName: value})
	if err != nil {
		return false, err
	}
	ok, boolean := result.(bool)
	if !boolean {
		return false, t.predicate.at.fail(EvaluationError, "cell test must return boolean, got %T", result)
	}
	return ok, nil
}

func (compile Compiler) inputEntry(e *element, column scalarType) (cellTest, error) {
	source, err := e.expressionText("expressionLanguage", true)
	if err != nil {
		return cellTest{}, err
	}
	if source == "" || source == "-" {
		return cellTest{}, nil
	}

	at := e.at("inputEntry")
	if source[0] == '<' || source[0] == '>' {
		end := 1
		if len(source) > 1 && source[1] == '=' {
			end++
		}
		op, bound := source[:end], strings.TrimSpace(source[end:])
		if literalType(bound) != numberType {
			return cellTest{}, at.fail(UnsupportedFeature, "unary %q requires a numeric literal", op)
		}
		if column != numberType {
			return cellTest{}, at.fail(InvalidModel, "unary %q requires a number column", op)
		}
		return compile.cellTest(at, op, bound)
	}
	if literalType(source) == "" {
		return cellTest{}, at.fail(UnsupportedFeature, "unsupported unary test %q: expected a literal, wildcard, or numeric comparison", source)
	}
	return compile.cellTest(at, "=", source)
}

func (compile Compiler) outputValues(e *element, column scalarType) ([]cellTest, error) {
	source, err := e.expressionText("expressionLanguage", false)
	if err != nil {
		return nil, err
	}
	at := e.at("outputValues")
	var tests []cellTest
	for _, item := range splitLiterals(source) {
		literal := strings.TrimSpace(item)
		typ := literalType(literal)
		if typ == "" {
			return nil, at.fail(InvalidModel, "outputValues requires comma-separated literals; got %q", literal)
		}
		if typ != column {
			return nil, at.fail(InvalidModel, "outputValues literal %q does not match output type %s", literal, column)
		}
		test, err := compile.cellTest(at, "=", literal)
		if err != nil {
			return nil, err
		}
		tests = append(tests, test)
	}
	return tests, nil
}

func (compile Compiler) cellTest(at location, op, literal string) (cellTest, error) {
	predicate, err := compile.expression(at, cellName+" "+op+" "+literal)
	return cellTest{predicate: predicate, ordering: op != "="}, err
}

// These patterns only distinguish literal tokens from FEEL expressions.
// The injected compiler owns validation, decoding and comparison semantics.
var (
	numberLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
	stringLiteral = regexp.MustCompile(`^"([^"\\]|\\.)*"$`)
)

func literalType(source string) scalarType {
	switch {
	case source == "null":
		return "null"
	case source == "true" || source == "false":
		return booleanType
	case numberLiteral.MatchString(source):
		return numberType
	case stringLiteral.MatchString(source):
		return stringType
	default:
		return ""
	}
}

func splitLiterals(source string) []string {
	var items []string
	var quoted, escaped bool
	start := 0
	for i, r := range source {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case !quoted && r == ',':
			items = append(items, source[start:i])
			start = i + 1
		}
	}
	return append(items, source[start:])
}
