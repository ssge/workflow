package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

func decodeStrictJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("request body is not valid UTF-8")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	if err := rejectCaseInsensitiveFieldNames(data, reflect.TypeOf(target)); err != nil {
		return err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains more than one JSON value")
		}
		return err
	}
	return nil
}

var rawMessageType = reflect.TypeOf(json.RawMessage{})

func rejectCaseInsensitiveFieldNames(data []byte, targetType reflect.Type) error {
	for targetType.Kind() == reflect.Pointer {
		targetType = targetType.Elem()
	}
	if targetType == rawMessageType {
		return nil
	}
	switch targetType.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
			return err
		}
		for name, value := range fields {
			fieldType, found := jsonFieldType(targetType, name)
			if !found {
				for i := 0; i < targetType.NumField(); i++ {
					field := targetType.Field(i)
					expected := strings.Split(field.Tag.Get("json"), ",")[0]
					if expected != "" && expected != "-" && strings.EqualFold(expected, name) {
						return fmt.Errorf("JSON field %q must use exact spelling %q", name, expected)
					}
				}
				continue
			}
			if err := rejectCaseInsensitiveFieldNames(value, fieldType); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := rejectCaseInsensitiveFieldNames(value, targetType.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		if targetType.Key().Kind() != reflect.String {
			return nil
		}
		var values map[string]json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil || values == nil {
			return err
		}
		for _, value := range values {
			if err := rejectCaseInsensitiveFieldNames(value, targetType.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonFieldType(targetType reflect.Type, name string) (reflect.Type, bool) {
	for i := 0; i < targetType.NumField(); i++ {
		field := targetType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		fieldName := strings.Split(field.Tag.Get("json"), ",")[0]
		if fieldName == "" {
			fieldName = field.Name
		}
		if fieldName == name {
			return field.Type, true
		}
	}
	return nil, false
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains more than one JSON value")
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		return consumeClosingDelim(decoder, '}')
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		return consumeClosingDelim(decoder, ']')
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func consumeClosingDelim(decoder *json.Decoder, expected json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != expected {
		return fmt.Errorf("unexpected JSON delimiter %v, want %q", token, expected)
	}
	return nil
}
