package core

import (
	"crypto/sha256"
	"testing"
)

func TestSamplingHashVersions(t *testing.T) {
	old := SamplingParamsHash(0, 8, false, 0, 0)
	if old != sha256.Sum256([]byte("8")) {
		t.Fatal("legacy changed")
	}
	a := SamplingParamsHash(2, 8, true, 3, 5)
	if a == old || a == SamplingParamsHash(2, 8, false, 0, 5) || a == SamplingParamsHash(2, 8, true, 2, 5) {
		t.Fatal("uncommitted mode")
	}
}
