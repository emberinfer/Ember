package ledger

import (
	"encoding/hex"
	"time"

	chainapp "ember/services/chain"
)

type ConsumerStageObservation = chainapp.ConsumerStageObservation

type ConsumerLatencyRecorder interface {
	ObserveLatency(string, []byte, time.Duration, []ConsumerStageObservation) error
}

func (l *ChainLedger) ObserveLatency(id string, receiptRef []byte, elapsed time.Duration, stages []ConsumerStageObservation) error {
	hold, err := l.hold(id)
	if err != nil {
		return err
	}
	return l.withAccount(hold.Payer, chainapp.Action{Type: "observe_latency", HoldID: id,
		ConsumerLatency: &chainapp.ConsumerLatencyObservation{ReceiptRef: hex.EncodeToString(receiptRef),
			ObservedNS: uint64(elapsed.Nanoseconds()), Stages: stages}})
}
