// Package dmn compiles and evaluates R1 FIRST decision tables.
package dmn

import (
	"encoding/json"
	"fmt"
)

// Compiler compiles and validates FEEL source without a leading "=" marker.
// In addition to input and output expressions, it receives simple comparisons
// generated from DMN literal tests. These run with a private binding for the
// cell value, not with the caller's variables.
type Compiler func(source string) (Expression, error)

// Expression evaluates pure FEEL over read-only variables. Scalars at this
// boundary are nil, string, bool, or json.Number containing an exact,
// profile-valid decimal. An absent identifier evaluates to nil.
type Expression func(vars map[string]any) (any, error)

const (
	InvalidModel       = "INVALID_MODEL"
	UnsupportedFeature = "UNSUPPORTED_FEATURE"
	EvaluationError    = "EVALUATION_ERROR"
)

// Error identifies the source field responsible for a parse or evaluation error.
type Error struct {
	Code, File, ElementID, Field, Reason string
	cause                                error
}

func (e *Error) Error() string {
	at := e.File
	if e.ElementID != "" {
		at += "#" + e.ElementID
	}
	if e.Field != "" {
		at += ":" + e.Field
	}
	return fmt.Sprintf("%s %s: %s", e.Code, at, e.Reason)
}

func (e *Error) Unwrap() error { return e.cause }

type location struct {
	file, id, field string
}

func (l location) fail(code, format string, args ...any) *Error {
	return &Error{Code: code, File: l.file, ElementID: l.id, Field: l.field, Reason: fmt.Sprintf(format, args...)}
}

type compiledExpression struct {
	at   location
	eval Expression
}

func (e compiledExpression) evaluate(vars map[string]any) (any, error) {
	value, err := e.eval(vars)
	if err != nil {
		d := e.at.fail(EvaluationError, "%v", err)
		d.cause = err
		return nil, d
	}
	return value, nil
}

type scalarType string

const (
	numberType  scalarType = "number"
	stringType  scalarType = "string"
	booleanType scalarType = "boolean"
)

func (t scalarType) accepts(value any) bool {
	switch value.(type) {
	case json.Number:
		return t == numberType
	case string:
		return t == stringType
	case bool:
		return t == booleanType
	default:
		return false
	}
}

type input struct {
	typ  scalarType
	expr compiledExpression
}

type rule struct {
	tests  []cellTest
	output compiledExpression
}

// Model contains independent decisions indexed by their XML IDs.
type Model struct {
	decisions map[string]*Decision
}

// Decision looks up a table decision by ID.
func (m *Model) Decision(id string) (*Decision, bool) {
	d, ok := m.decisions[id]
	return d, ok
}

// Decision is an immutable compiled FIRST table.
type Decision struct {
	id, name, outputName string
	inputs               []input
	outputType           scalarType
	allowed              []cellTest
	rules                []rule
}

func (d *Decision) ID() string         { return d.id }
func (d *Decision) Name() string       { return d.name }
func (d *Decision) OutputName() string { return d.outputName }

// Evaluate reads the input columns in order, then evaluates only the first
// matching rule's output. No match returns (nil, nil).
func (d *Decision) Evaluate(vars map[string]any) (any, error) {
	inputs := make([]any, len(d.inputs))
	for i, in := range d.inputs {
		value, err := in.expr.evaluate(vars)
		if err != nil {
			return nil, err
		}
		if value != nil && !in.typ.accepts(value) {
			return nil, in.expr.at.fail(EvaluationError, "input column %d expects %s, got %T", i+1, in.typ, value)
		}
		inputs[i] = value
	}

rules:
	for _, r := range d.rules {
		for i, test := range r.tests {
			ok, err := test.matches(inputs[i])
			if err != nil {
				return nil, err
			}
			if !ok {
				continue rules
			}
		}
		value, err := r.output.evaluate(vars)
		if err != nil {
			return nil, err
		}
		if !d.outputType.accepts(value) {
			return nil, r.output.at.fail(EvaluationError, "output %q expects %s, got %T", d.outputName, d.outputType, value)
		}
		if len(d.allowed) == 0 {
			return value, nil
		}
		for _, test := range d.allowed {
			ok, err := test.matches(value)
			if err != nil {
				return nil, err
			}
			if ok {
				return value, nil
			}
		}
		return nil, r.output.at.fail(EvaluationError, "output %q is not in outputValues", d.outputName)
	}
	return nil, nil
}
