package httpapi

import (
	"encoding/json"
	"testing"
)

func TestDecodeStrictJSON(t *testing.T) {
	t.Run("preserves arbitrary precision numbers", func(t *testing.T) {
		var got struct {
			Value json.Number `json:"value"`
		}
		const input = `{"value":1.234567890123456789012345678901234}`
		if err := decodeStrictJSON([]byte(input), &got); err != nil {
			t.Fatal(err)
		}
		if got.Value.String() != "1.234567890123456789012345678901234" {
			t.Fatalf("number changed: %q", got.Value)
		}
	})

	for _, input := range []string{
		`{"value":1,"value":2}`,
		`{"nested":{"value":1,"value":2}}`,
		`{"items":[{"value":1,"value":2}]}`,
	} {
		t.Run("duplicate keys "+input, func(t *testing.T) {
			var got any
			if err := decodeStrictJSON([]byte(input), &got); err == nil {
				t.Fatal("duplicate key was accepted")
			}
		})
	}

	t.Run("rejects unknown fields", func(t *testing.T) {
		var got struct {
			Value string `json:"value"`
		}
		if err := decodeStrictJSON([]byte(`{"value":"ok","extra":true}`), &got); err == nil {
			t.Fatal("unknown field was accepted")
		}
	})

	t.Run("rejects non-exact field spelling", func(t *testing.T) {
		var got struct {
			Value string `json:"value"`
		}
		if err := decodeStrictJSON([]byte(`{"Value":"accepted by default encoding/json"}`), &got); err == nil {
			t.Fatal("case-insensitive field spelling was accepted")
		}
	})

	t.Run("rejects trailing values", func(t *testing.T) {
		var got any
		if err := decodeStrictJSON([]byte(`{} {}`), &got); err == nil {
			t.Fatal("trailing JSON value was accepted")
		}
	})

	t.Run("rejects invalid UTF-8", func(t *testing.T) {
		var got any
		if err := decodeStrictJSON([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}, &got); err == nil {
			t.Fatal("invalid UTF-8 was accepted")
		}
	})
}
