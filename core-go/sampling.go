package core

import (
	"crypto/sha256"
	"fmt"
	"strconv"
)

// Version zero preserves historical max_tokens-only commitments byte-for-byte.
func SamplingParamsHash(version uint32, max uint32, enabled bool, thinking, answer uint32) [32]byte {
	if version == 0 {
		return sha256.Sum256([]byte(strconv.FormatUint(uint64(max), 10)))
	}
	return sha256.Sum256([]byte(fmt.Sprintf("ember-sampling-v2|max_tokens=%d|thinking_enabled=%t|thinking_budget=%d|answer_budget=%d", max, enabled, thinking, answer)))
}
