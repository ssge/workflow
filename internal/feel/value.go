// Package feel implements the R1 JSON value model and pure expression subset.
package feel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"
)

var (
	// ErrInvalidValue indicates invalid JSON, duplicate keys, or a number outside the profile.
	ErrInvalidValue = errors.New("invalid JSON value")
	// ErrSyntax identifies an invalid or unsupported expression at compilation.
	ErrSyntax = errors.New("invalid FEEL syntax")
	// ErrEvaluation identifies an expression that cannot be evaluated with its input values.
	ErrEvaluation = errors.New("FEEL evaluation error")
)

type Kind uint8

const (
	NullKind Kind = iota
	BooleanKind
	NumberKind
	StringKind
	ArrayKind
	ObjectKind
)

// Value is an immutable JSON value. Its zero value is null.
type Value struct {
	kind    Kind
	boolean bool
	number  decimal
	text    string
	array   []Value
	object  map[string]Value
}

func Null() Value          { return Value{} }
func Boolean(b bool) Value { return Value{kind: BooleanKind, boolean: b} }

// Number parses a JSON decimal without binary floating point or rounding.
func Number(s string) (Value, error) {
	d, err := parseDecimal(s)
	if err != nil {
		return Value{}, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	return Value{kind: NumberKind, number: d}, nil
}

func String(s string) (Value, error) {
	if !utf8.ValidString(s) {
		return Value{}, fmt.Errorf("%w: string is not UTF-8", ErrInvalidValue)
	}
	return Value{kind: StringKind, text: s}, nil
}

func Array(elements []Value) Value {
	return Value{kind: ArrayKind, array: append([]Value(nil), elements...)}
}

func Object(members map[string]Value) (Value, error) {
	copy := make(map[string]Value, len(members))
	for key, v := range members {
		if !utf8.ValidString(key) {
			return Value{}, fmt.Errorf("%w: object key is not UTF-8", ErrInvalidValue)
		}
		copy[key] = v
	}
	return Value{kind: ObjectKind, object: copy}, nil
}

func (v Value) Kind() Kind { return v.kind }

func (v Value) AsBoolean() (bool, bool)  { return v.boolean, v.kind == BooleanKind }
func (v Value) AsString() (string, bool) { return v.text, v.kind == StringKind }

// AsNumber returns the exact plain decimal representation.
func (v Value) AsNumber() (string, bool) {
	if v.kind != NumberKind {
		return "", false
	}
	return v.number.String(), true
}

func (v Value) Elements() ([]Value, bool) {
	if v.kind != ArrayKind {
		return nil, false
	}
	return append([]Value{}, v.array...), true
}

func (v Value) Members() (map[string]Value, bool) {
	if v.kind != ObjectKind {
		return nil, false
	}
	result := make(map[string]Value, len(v.object))
	for k, item := range v.object {
		result[k] = item
	}
	return result, true
}

// Interface returns a detached JSON-compatible value, preserving decimals as
// json.Number rather than converting them to binary floating point.
func (v Value) Interface() any {
	switch v.kind {
	case NullKind:
		return nil
	case BooleanKind:
		return v.boolean
	case NumberKind:
		return json.Number(v.number.String())
	case StringKind:
		return v.text
	case ArrayKind:
		result := make([]any, len(v.array))
		for i, item := range v.array {
			result[i] = item.Interface()
		}
		return result
	case ObjectKind:
		result := make(map[string]any, len(v.object))
		for key, item := range v.object {
			result[key] = item.Interface()
		}
		return result
	default:
		return nil
	}
}

// FromInterface converts exact JSON-compatible Go values. Float values are
// rejected; callers must preserve input decimals as json.Number.
func FromInterface(value any) (Value, error) {
	switch value := value.(type) {
	case nil:
		return Null(), nil
	case bool:
		return Boolean(value), nil
	case string:
		return String(value)
	case json.Number:
		return Number(value.String())
	case Value:
		return value, nil
	case json.RawMessage:
		return ParseJSON(value)
	case int:
		return Number(strconv.FormatInt(int64(value), 10))
	case int8:
		return Number(strconv.FormatInt(int64(value), 10))
	case int16:
		return Number(strconv.FormatInt(int64(value), 10))
	case int32:
		return Number(strconv.FormatInt(int64(value), 10))
	case int64:
		return Number(strconv.FormatInt(value, 10))
	case uint:
		return Number(strconv.FormatUint(uint64(value), 10))
	case uint8:
		return Number(strconv.FormatUint(uint64(value), 10))
	case uint16:
		return Number(strconv.FormatUint(uint64(value), 10))
	case uint32:
		return Number(strconv.FormatUint(uint64(value), 10))
	case uint64:
		return Number(strconv.FormatUint(value, 10))
	case float32, float64:
		return Value{}, fmt.Errorf("%w: floating-point values are not exact JSON decimals", ErrInvalidValue)
	case []Value:
		return Array(value), nil
	case []any:
		elements := make([]Value, len(value))
		for i, item := range value {
			converted, err := FromInterface(item)
			if err != nil {
				return Value{}, err
			}
			elements[i] = converted
		}
		return Array(elements), nil
	case map[string]Value:
		return Object(value)
	case map[string]any:
		members := make(map[string]Value, len(value))
		for key, item := range value {
			converted, err := FromInterface(item)
			if err != nil {
				return Value{}, err
			}
			members[key] = converted
		}
		return Object(members)
	default:
		return Value{}, fmt.Errorf("%w: unsupported Go value %T", ErrInvalidValue, value)
	}
}

// Bindings converts form/DMN variable maps without losing decimal precision.
func Bindings(values map[string]any) (map[string]Value, error) {
	result := make(map[string]Value, len(values))
	for key, value := range values {
		converted, err := FromInterface(value)
		if err != nil {
			return nil, fmt.Errorf("variable %q: %w", key, err)
		}
		result[key] = converted
	}
	return result, nil
}

// Get distinguishes a missing member from an explicit null.
func (v Value) Get(name string) (Value, bool) {
	if v.kind != ObjectKind {
		return Value{}, false
	}
	item, ok := v.object[name]
	return item, ok
}

// Equal compares JSON values recursively, with numbers compared by magnitude.
func (v Value) Equal(other Value) bool {
	if v.kind != other.kind {
		return false
	}
	switch v.kind {
	case NullKind:
		return true
	case BooleanKind:
		return v.boolean == other.boolean
	case NumberKind:
		return v.number.compare(other.number) == 0
	case StringKind:
		return v.text == other.text
	case ArrayKind:
		if len(v.array) != len(other.array) {
			return false
		}
		for i, item := range v.array {
			if !item.Equal(other.array[i]) {
				return false
			}
		}
		return true
	case ObjectKind:
		if len(v.object) != len(other.object) {
			return false
		}
		for key, item := range v.object {
			o, ok := other.object[key]
			if !ok || !item.Equal(o) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// MarshalJSON serializes exact decimals and preserves null, arrays and objects.
func (v Value) MarshalJSON() ([]byte, error) {
	switch v.kind {
	case NullKind:
		return []byte("null"), nil
	case BooleanKind:
		if v.boolean {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case NumberKind:
		return []byte(v.number.String()), nil
	case StringKind:
		return json.Marshal(v.text)
	case ArrayKind:
		var b bytes.Buffer
		b.WriteByte('[')
		for i, item := range v.array {
			if i != 0 {
				b.WriteByte(',')
			}
			data, err := item.MarshalJSON()
			if err != nil {
				return nil, err
			}
			b.Write(data)
		}
		b.WriteByte(']')
		return b.Bytes(), nil
	case ObjectKind:
		keys := make([]string, 0, len(v.object))
		for k := range v.object {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b bytes.Buffer
		b.WriteByte('{')
		for i, k := range keys {
			if i != 0 {
				b.WriteByte(',')
			}
			key, _ := json.Marshal(k)
			b.Write(key)
			b.WriteByte(':')
			data, err := v.object[k].MarshalJSON()
			if err != nil {
				return nil, err
			}
			b.Write(data)
		}
		b.WriteByte('}')
		return b.Bytes(), nil
	default:
		return nil, fmt.Errorf("%w: unknown value kind", ErrInvalidValue)
	}
}

// UnmarshalJSON applies the same validation as ParseJSON.
func (v *Value) UnmarshalJSON(data []byte) error {
	result, err := ParseJSON(data)
	if err != nil {
		return err
	}
	*v = result
	return nil
}

// ParseJSON rejects duplicate object keys and invalid UTF-8 or decimal values.
func ParseJSON(data []byte) (Value, error) {
	fail := func(err error) (Value, error) {
		return Value{}, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	if !utf8.Valid(data) {
		return fail(errors.New("input is not UTF-8"))
	}
	source := string(data)
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			end, _, err := readString(source, i)
			if err != nil {
				return fail(err)
			}
			i = end - 1
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	v, err := decodeValue(decoder)
	if err != nil {
		return fail(err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return fail(err)
	}
	return v, nil
}

func decodeValue(decoder *json.Decoder) (Value, error) {
	token, err := decoder.Token()
	if err != nil {
		return Value{}, err
	}
	switch token := token.(type) {
	case nil:
		return Null(), nil
	case bool:
		return Boolean(token), nil
	case string:
		return String(token)
	case json.Number:
		return Number(string(token))
	case json.Delim:
		switch token {
		case '[':
			var items []Value
			for decoder.More() {
				item, err := decodeValue(decoder)
				if err != nil {
					return Value{}, err
				}
				items = append(items, item)
			}
			if _, err := decoder.Token(); err != nil {
				return Value{}, err
			}
			return Array(items), nil
		case '{':
			items := make(map[string]Value)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return Value{}, err
				}
				name, ok := key.(string)
				if !ok {
					return Value{}, errors.New("object key must be a string")
				}
				if _, exists := items[name]; exists {
					return Value{}, fmt.Errorf("duplicate object key %q", name)
				}
				item, err := decodeValue(decoder)
				if err != nil {
					return Value{}, err
				}
				items[name] = item
			}
			if _, err := decoder.Token(); err != nil {
				return Value{}, err
			}
			return Object(items)
		}
	}
	return Value{}, errors.New("unexpected JSON token")
}

// readString validates JSON string escapes, including paired UTF-16 surrogates.
func readString(s string, start int) (int, string, error) {
	hex := func(c byte) int {
		switch {
		case c >= '0' && c <= '9':
			return int(c - '0')
		case c >= 'a' && c <= 'f':
			return int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			return int(c-'A') + 10
		}
		return -1
	}
	codeUnit := func(i int) int {
		if i+4 > len(s) {
			return -1
		}
		n := 0
		for j := i; j < i+4; j++ {
			digit := hex(s[j])
			if digit < 0 {
				return -1
			}
			n = n*16 + digit
		}
		return n
	}
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '"':
			var decoded string
			if err := json.Unmarshal([]byte(s[start:i+1]), &decoded); err != nil {
				return 0, "", err
			}
			return i + 1, decoded, nil
		case '\\':
			i++
			if i >= len(s) {
				return 0, "", errors.New("unterminated string escape")
			}
			switch s[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				code := codeUnit(i + 1)
				if code < 0 {
					return 0, "", errors.New("invalid Unicode escape")
				}
				i += 4
				if code >= 0xD800 && code <= 0xDBFF {
					if i+6 >= len(s) || s[i+1] != '\\' || s[i+2] != 'u' {
						return 0, "", errors.New("unpaired high surrogate")
					}
					low := codeUnit(i + 3)
					if low < 0xDC00 || low > 0xDFFF {
						return 0, "", errors.New("unpaired high surrogate")
					}
					i += 6
				} else if code >= 0xDC00 && code <= 0xDFFF {
					return 0, "", errors.New("unpaired low surrogate")
				}
			default:
				return 0, "", errors.New("invalid string escape")
			}
		default:
			if s[i] < 0x20 {
				return 0, "", errors.New("control character in string")
			}
		}
	}
	return 0, "", errors.New("unterminated string")
}

// ValidIdentifier reports whether name is a bindable root identifier.
func ValidIdentifier(name string) bool {
	if name == "" || name == "true" || name == "false" || name == "null" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isLetter(c) && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}
