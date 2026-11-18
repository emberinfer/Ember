package jsonspool

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactMatchesMarshal(t *testing.T) {
	for _, padding := range []int{0, 65520, 65521, 65522, 65523, 65524, 65525, 65526, 65527} {
		raw := json.RawMessage(` [ { "x": "` + strings.Repeat("x", padding) + "<>&\u2028\u2029" + `", "escaped": "\\\"<" } ] `)
		expected, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		if err = Compact(&got, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), expected) {
			t.Fatalf("padding=%d bytes changed", padding)
		}
	}
}
