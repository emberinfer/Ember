package core

import (
	"errors"
	"math/big"
	"sort"
)

// StageWork is verified layer-token work, not time spent or a hardware claim.
// Callers must verify signed stage receipts before constructing this input.
type StageWork struct {
	NodeID     string
	StartLayer uint32
	EndLayer   uint32
	Tokens     uint32
}

type StageShare struct {
	NodeID string
	Work   uint64
	Amount int64
}

// SplitInvocationCharge aggregates independently signed layer invocations by
// node. Each StageWork entry is one or more verified token executions.
func SplitInvocationCharge(amount int64, invocations []StageWork) ([]StageShare, error) {
	if amount < 0 || len(invocations) == 0 {
		return nil, errors.New("invalid pipeline work")
	}
	work := map[string]uint64{}
	var total uint64
	for _, invocation := range invocations {
		if invocation.NodeID == "" || invocation.EndLayer <= invocation.StartLayer || invocation.Tokens == 0 {
			return nil, errors.New("invalid stage invocation")
		}
		value := uint64(invocation.EndLayer-invocation.StartLayer) * uint64(invocation.Tokens)
		if ^uint64(0)-total < value || ^uint64(0)-work[invocation.NodeID] < value {
			return nil, errors.New("pipeline work overflow")
		}
		total += value
		work[invocation.NodeID] += value
	}
	ids := make([]string, 0, len(work))
	for id := range work {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]StageShare, len(ids))
	remainders := make([]*big.Int, len(ids))
	divisor := new(big.Int).SetUint64(total)
	var assigned int64
	for i, id := range ids {
		product := new(big.Int).Mul(big.NewInt(amount), new(big.Int).SetUint64(work[id]))
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(product, divisor, remainder)
		out[i] = StageShare{NodeID: id, Work: work[id], Amount: quotient.Int64()}
		assigned += out[i].Amount
		remainders[i] = remainder
	}
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if cmp := remainders[a].Cmp(remainders[b]); cmp != 0 {
			return cmp > 0
		}
		return out[a].NodeID < out[b].NodeID
	})
	for i := int64(0); i < amount-assigned; i++ {
		out[order[i]].Amount++
	}
	return out, nil
}

// SplitStageCharge uses largest remainders, with node ID as the deterministic
// tie-break. It conserves every integer Spark subunit, even for tiny charges.
// This is arithmetic only; it does not perform or authorize settlement.
func SplitStageCharge(amount int64, stages []StageWork, totalLayers, tokens uint32) ([]StageShare, error) {
	if amount < 0 || totalLayers == 0 || tokens == 0 || len(stages) == 0 {
		return nil, errors.New("invalid pipeline charge")
	}
	sorted := append([]StageWork(nil), stages...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StartLayer < sorted[j].StartLayer })
	seen := map[string]bool{}
	var cursor uint32
	var total uint64
	out := make([]StageShare, len(sorted))
	for i, s := range sorted {
		if s.NodeID == "" || seen[s.NodeID] || s.StartLayer != cursor || s.EndLayer <= cursor || s.EndLayer > totalLayers || s.Tokens != tokens {
			return nil, errors.New("invalid stage identity, coverage or token count")
		}
		seen[s.NodeID] = true
		work := uint64(s.EndLayer-s.StartLayer) * uint64(s.Tokens)
		total += work // bounded by uint32 layers * uint32 tokens once coverage is checked
		out[i] = StageShare{NodeID: s.NodeID, Work: work}
		cursor = s.EndLayer
	}
	if cursor != totalLayers {
		return nil, errors.New("incomplete stage coverage")
	}
	remainders := make([]*big.Int, len(out))
	var assigned int64
	divisor := new(big.Int).SetUint64(total)
	for i, s := range out {
		product := new(big.Int).Mul(big.NewInt(amount), new(big.Int).SetUint64(s.Work))
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(product, divisor, remainder)
		out[i].Amount = quotient.Int64()
		assigned += out[i].Amount
		remainders[i] = remainder
	}
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if cmp := remainders[a].Cmp(remainders[b]); cmp != 0 {
			return cmp > 0
		}
		return out[a].NodeID < out[b].NodeID
	})
	for i := int64(0); i < amount-assigned; i++ {
		out[order[i]].Amount++
	}
	return out, nil
}
