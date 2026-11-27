package db

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/ssge/workflow/db/store"
)

func sameCommand(a, b store.Command) (bool, error) {
	if a.Kind != b.Kind {
		return false, nil
	}
	equal, err := equalObjects(a.Target, b.Target)
	if err != nil || !equal {
		return equal, err
	}
	return equalObjects(a.Payload, b.Payload)
}

func equalObjects(a, b store.Object) (bool, error) {
	if a == b {
		return true, nil
	}
	decode := func(raw store.Object) (map[string]any, error) {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		var obj map[string]any
		if err := decoder.Decode(&obj); err != nil {
			return nil, err
		}
		if obj == nil || decoder.Decode(new(any)) != io.EOF {
			return nil, fmt.Errorf("%w: expected one JSON object", store.ErrInvalidArgument)
		}
		return obj, nil
	}
	left, err := decode(a)
	if err != nil {
		return false, err
	}
	right, err := decode(b)
	if err != nil {
		return false, err
	}
	return equalJSON(left, right), nil
}

func equalJSON(a, b any) bool {
	switch a := a.(type) {
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, av := range a {
			if bv, ok := b[key]; !ok || !equalJSON(av, bv) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i, av := range a {
			if !equalJSON(av, b[i]) {
				return false
			}
		}
		return true
	case json.Number:
		b, ok := b.(json.Number)
		if !ok {
			return false
		}
		ac, ae := normalizedDecimal(a)
		bc, be := normalizedDecimal(b)
		return ac == bc && ae.Cmp(&be) == 0
	default:
		// Other JSON values (strings, booleans and null) are comparable.
		return a == b
	}
}

// Represent a valid JSON number as a signed coefficient without leading or
// trailing zeros and a base-10 exponent. Never expand an exponent into digits
// or convert through a binary float.
func normalizedDecimal(n json.Number) (string, big.Int) {
	mantissa := string(n)
	var exponent big.Int
	if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
		exponent.SetString(mantissa[i+1:], 10)
		mantissa = mantissa[:i]
	}
	digits, negative := strings.CutPrefix(mantissa, "-")
	whole, frac, _ := strings.Cut(digits, ".")
	digits = strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return "", big.Int{}
	}
	coefficient := strings.TrimRight(digits, "0")
	exponent.Add(&exponent, big.NewInt(int64(len(digits)-len(coefficient)-len(frac))))
	if negative {
		coefficient = "-" + coefficient
	}
	return coefficient, exponent
}
