// Package form parses and resolves the R1 schema-19 form subset.
package form

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	InvalidModel       = "INVALID_MODEL"
	UnsupportedFeature = "UNSUPPORTED_FEATURE"
	EvaluationError    = "EVALUATION_ERROR"
)

// Error identifies a model or activation failure. Callers may add the source filename.
type Error struct {
	Code      string
	ElementID string
	Field     string
	Reason    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s (%s: %s)", e.Code, e.Reason, e.ElementID, e.Field)
}

// Evaluator and Compiler are supplied by the FEEL implementation. Compile must
// validate the expression; evaluators must be pure and not mutate variables.
// The expression passed to Compiler has its leading "=" and surrounding whitespace removed.
type Evaluator func(variables map[string]any) (any, error)
type Compiler func(expression string) (Evaluator, error)

type Option struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type component struct {
	kind, id, key string
	label         *string
	layout        map[string]any
	text          string
	evaluate      Evaluator
	options       []Option
	defaultValue  *string
}

// Template is a validated immutable form definition.
type Template struct {
	id         string
	components []component
	inputs     map[string]int
}

// ID returns the source form ID.
func (t *Template) ID() string { return t.id }

// ComponentView is a resolved component ready to encode as JSON.
type ComponentView map[string]any

type View struct {
	FormID     string          `json:"formId"`
	Components []ComponentView `json:"components"`
}

type FieldError struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

func failure(code, id, field, reason string) error {
	return &Error{Code: code, ElementID: id, Field: field, Reason: reason}
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validKey(key string) bool {
	return identifier.MatchString(key) && key != "true" && key != "false" && key != "null"
}

// Parse validates a complete form, compiling all dynamic text before returning.
func Parse(source []byte, compile Compiler) (*Template, error) {
	if !utf8.Valid(source) {
		return nil, failure(InvalidModel, "", "", "source is not UTF-8")
	}
	if err := checkJSON(source); err != nil {
		return nil, failure(InvalidModel, "", "", err.Error())
	}
	root, err := object(source, "", "")
	if err != nil {
		return nil, err
	}
	if err := allowed(root, "", "", "id", "type", "schemaVersion", "components", "executionPlatform", "executionPlatformVersion", "exporter"); err != nil {
		return nil, err
	}
	id, err := requiredString(root, "id", "", true)
	if err != nil {
		return nil, err
	}
	kind, err := requiredString(root, "type", id, true)
	if err != nil {
		return nil, err
	}
	if kind != "default" {
		return nil, failure(UnsupportedFeature, id, "type", "unsupported form type")
	}
	version, ok := root["schemaVersion"]
	if !ok || bytes.Equal(bytes.TrimSpace(version), []byte("null")) {
		return nil, failure(InvalidModel, id, "schemaVersion", "schema version is required")
	}
	var n int64
	if err := json.Unmarshal(version, &n); err != nil {
		return nil, failure(InvalidModel, id, "schemaVersion", "expected an integer")
	}
	if n != 19 {
		return nil, failure(UnsupportedFeature, id, "schemaVersion", "unsupported schema version")
	}
	rawComponents, ok := root["components"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawComponents), []byte("null")) {
		return nil, failure(InvalidModel, id, "components", "components array is required")
	}
	var list []json.RawMessage
	if err := json.Unmarshal(rawComponents, &list); err != nil {
		return nil, failure(InvalidModel, id, "components", "expected an array")
	}
	t := &Template{id: id, inputs: make(map[string]int), components: make([]component, 0, len(list))}
	ids := make(map[string]bool)
	for i, raw := range list {
		c, err := parseComponent(raw, compile)
		if err != nil {
			return nil, err
		}
		if c.id != "" {
			if ids[c.id] {
				return nil, failure(InvalidModel, c.id, "id", "duplicate component ID")
			}
			ids[c.id] = true
		}
		if c.kind == "select" || c.kind == "textfield" {
			if _, exists := t.inputs[c.key]; exists {
				return nil, failure(InvalidModel, c.id, "key", "duplicate input key")
			}
			t.inputs[c.key] = i
		}
		t.components = append(t.components, c)
	}
	return t, nil
}

func parseComponent(raw json.RawMessage, compile Compiler) (component, error) {
	c := component{}
	obj, err := object(raw, "", "components")
	if err != nil {
		return c, err
	}
	if c.id, err = optionalString(obj, "id", "", true); err != nil {
		return c, err
	}
	if c.kind, err = requiredString(obj, "type", c.id, true); err != nil {
		return c, err
	}
	fields := []string{"id", "type", "layout", "label"}
	switch c.kind {
	case "text":
		fields = append(fields, "text")
	case "select":
		fields = append(fields, "key", "values", "defaultValue")
	case "textfield":
		fields = append(fields, "key")
	case "button":
		fields = append(fields, "action")
	default:
		return c, failure(UnsupportedFeature, c.id, "type", "unsupported component type")
	}
	if err := allowed(obj, c.id, "", fields...); err != nil {
		return c, err
	}
	if _, ok := obj["label"]; ok {
		label, err := requiredString(obj, "label", c.id, false)
		if err != nil {
			return c, err
		}
		c.label = &label
	}
	if rawLayout, ok := obj["layout"]; ok {
		c.layout, err = parseLayout(rawLayout, c.id)
		if err != nil {
			return c, err
		}
	}
	switch c.kind {
	case "text":
		c.text, err = requiredString(obj, "text", c.id, false)
		if err != nil {
			return c, err
		}
		trimmed := strings.TrimSpace(c.text)
		if strings.HasPrefix(trimmed, "=") {
			if compile == nil {
				return c, failure(InvalidModel, c.id, "text", "dynamic text requires an expression compiler")
			}
			c.evaluate, err = compile(strings.TrimSpace(trimmed[1:]))
			if err != nil {
				return c, failure(InvalidModel, c.id, "text", err.Error())
			}
			if c.evaluate == nil {
				return c, failure(InvalidModel, c.id, "text", "compiler returned no evaluator")
			}
		}
	case "select", "textfield":
		c.key, err = requiredString(obj, "key", c.id, true)
		if err != nil {
			return c, err
		}
		if !validKey(c.key) {
			return c, failure(InvalidModel, c.id, "key", "input key must be a root identifier")
		}
		if c.kind == "select" {
			if err := parseSelect(&c, obj); err != nil {
				return c, err
			}
		}
	case "button":
		action, err := requiredString(obj, "action", c.id, true)
		if err != nil {
			return c, err
		}
		if action != "submit" {
			return c, failure(UnsupportedFeature, c.id, "action", "only submit buttons are supported")
		}
	}
	return c, nil
}

func parseSelect(c *component, obj map[string]json.RawMessage) error {
	raw, ok := obj["values"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return failure(InvalidModel, c.id, "values", "values array is required")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return failure(InvalidModel, c.id, "values", "expected a non-empty array")
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		option, err := object(entry, c.id, "values")
		if err != nil {
			return err
		}
		if err := allowed(option, c.id, "values.", "label", "value"); err != nil {
			return err
		}
		label, err := requiredString(option, "label", c.id, false)
		if err != nil {
			return err
		}
		value, err := requiredString(option, "value", c.id, false)
		if err != nil {
			return err
		}
		if seen[value] {
			return failure(InvalidModel, c.id, "values", "duplicate select value")
		}
		seen[value] = true
		c.options = append(c.options, Option{Label: label, Value: value})
	}
	if raw, ok := obj["defaultValue"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		value, err := requiredString(obj, "defaultValue", c.id, false)
		if err != nil {
			return err
		}
		if !seen[value] {
			return failure(InvalidModel, c.id, "defaultValue", "default is not a select value")
		}
		c.defaultValue = &value
	}
	return nil
}

func parseLayout(raw json.RawMessage, id string) (map[string]any, error) {
	obj, err := object(raw, id, "layout")
	if err != nil {
		return nil, err
	}
	layout := make(map[string]any, len(obj))
	if _, ok := obj["row"]; ok {
		row, err := requiredString(obj, "row", id, false)
		if err != nil {
			return nil, err
		}
		layout["row"] = row
	}
	if columns, ok := obj["columns"]; ok {
		if bytes.Equal(bytes.TrimSpace(columns), []byte("null")) {
			layout["columns"] = nil
		} else {
			var n int64
			if err := json.Unmarshal(columns, &n); err != nil {
				return nil, failure(InvalidModel, id, "layout.columns", "expected an integer or null")
			}
			layout["columns"] = json.Number(string(bytes.TrimSpace(columns)))
		}
	}
	return layout, nil
}

func object(raw json.RawMessage, id, field string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, failure(InvalidModel, id, field, "expected an object")
	}
	return obj, nil
}

func allowed(obj map[string]json.RawMessage, id, prefix string, keys ...string) error {
	known := make(map[string]bool, len(keys))
	for _, key := range keys {
		known[key] = true
	}
	var unknown []string
	for key := range obj {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) != 0 {
		sort.Strings(unknown)
		return failure(UnsupportedFeature, id, prefix+unknown[0], "unsupported field")
	}
	return nil
}

func requiredString(obj map[string]json.RawMessage, field, id string, nonempty bool) (string, error) {
	raw, ok := obj[field]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", failure(InvalidModel, id, field, "string is required")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || (nonempty && value == "") {
		return "", failure(InvalidModel, id, field, "expected a valid string")
	}
	return value, nil
}

func optionalString(obj map[string]json.RawMessage, field, id string, nonempty bool) (string, error) {
	if _, ok := obj[field]; !ok {
		return "", nil
	}
	return requiredString(obj, field, id, nonempty)
}

func checkJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := checkValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func checkValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			token, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return fmt.Errorf("expected object key")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = true
			if err := checkValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := checkValue(dec); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = dec.Token()
	return err
}

// Render resolves a fresh view without modifying the template or variables.
func (t *Template) Render(variables map[string]any) (View, error) {
	view := View{FormID: t.id, Components: make([]ComponentView, 0, len(t.components))}
	for _, c := range t.components {
		out := ComponentView{"type": c.kind}
		if c.id != "" {
			out["id"] = c.id
		}
		if c.label != nil {
			out["label"] = *c.label
		}
		if c.layout != nil {
			layout := make(map[string]any, len(c.layout))
			for key, value := range c.layout {
				layout[key] = value
			}
			out["layout"] = layout
		}
		switch c.kind {
		case "text":
			text := c.text
			if c.evaluate != nil {
				result, err := c.evaluate(variables)
				if err != nil {
					return View{}, failure(EvaluationError, c.id, "text", err.Error())
				}
				var ok bool
				text, ok = result.(string)
				if !ok {
					return View{}, failure(EvaluationError, c.id, "text", "display expression must return a string")
				}
			}
			out["text"] = text
		case "textfield", "select":
			out["key"] = c.key
			value := variables[c.key]
			if value != nil {
				s, ok := value.(string)
				if !ok {
					return View{}, failure(EvaluationError, c.id, "value", "bound value must be a string or null")
				}
				if c.kind == "select" && !contains(c.options, s) {
					return View{}, failure(EvaluationError, c.id, "value", "bound value is not a select option")
				}
			} else if c.kind == "select" && c.defaultValue != nil {
				value = *c.defaultValue
			}
			out["value"] = value
			if c.kind == "select" {
				out["values"] = append([]Option(nil), c.options...)
			}
		case "button":
			out["action"] = "submit"
		}
		view.Components = append(view.Components, out)
	}
	return view, nil
}

func contains(options []Option, value string) bool {
	for _, option := range options {
		if option.Value == value {
			return true
		}
	}
	return false
}

// ValidateSubmission returns only supplied writes, or field errors and no writes.
func (t *Template) ValidateSubmission(fields map[string]any) (map[string]any, []FieldError) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writes := make(map[string]any, len(fields))
	var errors []FieldError
	for _, key := range keys {
		i, ok := t.inputs[key]
		if !ok {
			errors = append(errors, FieldError{key, "unknown input key"})
			continue
		}
		value, ok := fields[key].(string)
		if !ok {
			errors = append(errors, FieldError{key, "value must be a string"})
			continue
		}
		c := t.components[i]
		if c.kind == "select" && !contains(c.options, value) {
			errors = append(errors, FieldError{key, "value is not a select option"})
			continue
		}
		writes[key] = value
	}
	if len(errors) != 0 {
		return nil, errors
	}
	return writes, nil
}
