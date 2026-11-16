package ledger

import (
	chain "ember/services/chain"
)

// PricedBackend is required for token-priced holds. Legacy SQLite backends
// fail closed for these profiles rather than silently accepting a trusted fee.
type PricedBackend interface {
	FreezePriced(id, payer, node string, amount int64, commitment []byte) error
	FreezePipelinePriced(id, payer string, amount int64, auth []StageAuthorization, total uint32, commitment []byte) error
}

func (l *ChainLedger) FreezePriced(id, payer, node string, amount int64, commitment []byte) error {
	return l.withAccount(payer, chain.Action{Type: "freeze", HoldID: id, Payer: payer, Node: node, Amount: amount, PricingCommitment: commitment})
}

func (l *ChainLedger) FreezePipelinePriced(id, payer string, amount int64, auth []StageAuthorization, total uint32, commitment []byte) error {
	a := make([]chain.PipelineAuthorization, len(auth))
	for i, v := range auth {
		a[i] = chain.PipelineAuthorization{NodeID: v.NodeID, StartLayer: v.StartLayer, EndLayer: v.EndLayer}
	}
	return l.withAccount(payer, chain.Action{Type: "freeze_pipeline", HoldID: id, Payer: payer, Amount: amount, TotalLayers: total, PipelineAuthorizations: a, PricingCommitment: commitment})
}
