package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// IdempotencyKey is lowercase hex SHA256(payer_account_bytes || client_request_nonce).
// Decode the account's hex ID to its raw bytes; encode uint64 nonces as eight
// big-endian bytes, or use the client's opaque nonce bytes verbatim. No framing
// or separator is inserted. Retries retain the nonce. Automatic thinking-off
// resend after truncation is a NEW logical request and MUST use a new nonce/key.
func IdempotencyKey(payerAccount, clientRequestNonce []byte) string {
	h := sha256.New()
	h.Write(payerAccount)
	h.Write(clientRequestNonce)
	return hex.EncodeToString(h.Sum(nil))
}

// freezeEconomicallyEqual is used only for keyed returns. freezePayload remains
// the exact original payload for request identity and legacy first-data finality.
func freezeEconomicallyEqual(storedPayload []byte, incoming Action) (bool, error) {
	var stored Action
	if err := json.Unmarshal(storedPayload, &stored); err != nil {
		return false, err
	}
	left, err := freezeEconomicProjection(stored)
	if err != nil {
		return false, err
	}
	right, err := freezeEconomicProjection(incoming)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

// Copy the whole Action so all fields are strict by default. Only transport
// identity and executor routing are removed; coverage remains economic.
func freezeEconomicProjection(a Action) ([]byte, error) {
	a.HoldID, a.IdempotencyKey, a.Node = "", "", ""
	a.PipelineAuthorizations = append([]PipelineAuthorization(nil), a.PipelineAuthorizations...)
	for i := range a.PipelineAuthorizations {
		p := &a.PipelineAuthorizations[i]
		p.NodeID, p.Profile, p.Reserved = "", nil, 0
	}
	var coverage []MoeAuthorization
	units := map[MoeUnitRef]bool{}
	for _, auth := range a.MoeAuthorizations {
		auth.NodeID, auth.Profile, auth.Reserved = "", nil, 0
		if auth.Kind == "expert" {
			for _, unit := range auth.Units {
				units[unit] = true
			}
			auth.Units = nil
			// Non-coverage fields are still strict, including unexpected spans.
			if auth.StartLayer != 0 || auth.EndLayer != 0 {
				coverage = append(coverage, auth)
			}
		} else {
			// Attention spans retain their order; unknown kinds stay strict.
			coverage = append(coverage, auth)
		}
	}
	if len(units) > 0 {
		ordered := make([]MoeUnitRef, 0, len(units))
		for unit := range units {
			ordered = append(ordered, unit)
		}
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].Layer != ordered[j].Layer {
				return ordered[i].Layer < ordered[j].Layer
			}
			return ordered[i].Expert < ordered[j].Expert
		})
		coverage = append(coverage, MoeAuthorization{Kind: "expert", Units: ordered})
	}
	a.MoeAuthorizations = coverage
	if len(a.PricingCommitment) > 0 {
		var c pb.ExecutionCommitment
		if proto.Unmarshal(a.PricingCommitment, &c) == nil {
			// Seed and commitment ClientNonce deliberately remain economic.
			c.RequestId, c.Signer, c.Signature = "", nil, nil
			canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(&c)
			if err != nil {
				return nil, err
			}
			a.PricingCommitment = canonical
		} // Malformed protobuf stays byte-exact, rather than being discarded.
	}
	return json.Marshal(a)
}
