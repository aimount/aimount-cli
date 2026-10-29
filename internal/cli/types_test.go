package cli

import (
	"encoding/json"
	"testing"
)

func TestJSONBoundaryPreservesNumbersAndStrictness(t *testing.T) {
	for _, tc := range []struct {
		raw           string
		strict, valid bool
	}{
		{`{"number":1.0000000000000001,"large":1e9999999999999999999999}`, true, true},
		{`{"a":{"x":1,"x":2}}`, true, false},
		{`{"a":{"x":1,"x":2}}`, false, true},
		{`{"a":1,"\u0061":2}`, true, false},
		{`"\ud800"`, true, true},
		{`{"a":1} {}`, false, false},
		{`[1,]`, false, false},
		{string([]byte{34, 255, 34}), false, false},
	} {
		v, err := decodeJSON([]byte(tc.raw), tc.strict)
		if (err == nil) != tc.valid {
			t.Fatalf("%q strict=%t: %v", tc.raw, tc.strict, err)
		}
		if n := obj(v)["number"]; n != nil && n != json.Number("1.0000000000000001") {
			t.Fatalf("number rounded: %v", n)
		}
	}
}
