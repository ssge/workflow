package evaluator

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ssge/workflow/form"
	"github.com/ssge/workflow/internal/dmn"
	"github.com/ssge/workflow/internal/feel"
)

func CompileForm(source string) (form.Evaluator, error) {
	expression, err := feel.Compile(source)
	if err != nil {
		return nil, err
	}
	return evaluate(expression), nil
}

func CompileDMN(source string) (dmn.Expression, error) {
	expression, err := feel.Compile(source)
	if err != nil {
		return nil, err
	}
	return evaluate(expression), nil
}

func evaluate(expression *feel.Expression) func(map[string]any) (any, error) {
	return func(variables map[string]any) (any, error) {
		bindings, err := feel.Bindings(variables)
		if err != nil {
			return nil, err
		}
		result, err := expression.Evaluate(bindings)
		if err != nil {
			return nil, err
		}
		return result.Interface(), nil
	}
}

func ParseVariables(raw json.RawMessage) (map[string]any, error) {
	value, err := feel.ParseJSON(raw)
	if err != nil {
		return nil, err
	}
	members, ok := value.Members()
	if !ok {
		return nil, fmt.Errorf("%w: variables must be a JSON object", feel.ErrInvalidValue)
	}
	variables := make(map[string]any, len(members))
	for key, item := range members {
		variables[key] = item.Interface()
	}
	return variables, nil
}

func MarshalVariables(variables map[string]any) (json.RawMessage, error) {
	value, err := feel.FromInterface(variables)
	if err != nil {
		return nil, err
	}
	if _, ok := value.Members(); !ok {
		return nil, errors.New("variables must be a JSON object")
	}
	data, err := json.Marshal(value)
	return json.RawMessage(data), err
}
