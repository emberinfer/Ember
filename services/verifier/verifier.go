package verifier

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/ledger"
)

func challengeLayers(policyLayers []string, defaults []uint32) ([]uint32, error) {
	if len(policyLayers) == 0 {
		return defaults, nil
	}
	layers := make([]uint32, len(policyLayers))
	for i, layer := range policyLayers {
		index, err := strconv.ParseUint(layer, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid numeric policy layer %q: %w", layer, err)
		}
		layers[i] = uint32(index)
	}
	return layers, nil
}

// Draw signs the already-received receipt digest; the signature is a
// publicly checkable deterministic draw, not an unpredictable public beacon.
func Draw(receipt *pb.ExecutionReceipt, key ed25519.PrivateKey, percent uint32, now time.Time, policyLayers ...string) (*pb.AuditChallenge, bool, error) {
	if percent > 100 {
		return nil, false, errors.New("invalid audit rate")
	}
	if err := core.Verify(receipt); err != nil {
		return nil, false, err
	}
	ref, err := core.Digest(receipt)
	if err != nil {
		return nil, false, err
	}
	proof := ed25519.Sign(key, append([]byte("ember/audit/draw/v1\x00"), ref...))
	hash := sha256.Sum256(proof)
	drawn := binary.BigEndian.Uint64(hash[:8])%100 < uint64(percent)
	if !drawn {
		return nil, false, nil
	}
	count := receipt.OutputTokens
	if count == 0 {
		return nil, false, errors.New("empty receipt")
	}
	positions := []uint32{0, count / 2, count - 1}
	layers, err := challengeLayers(policyLayers, []uint32{0, 14})
	if err != nil {
		return nil, false, err
	}
	challenge := &pb.AuditChallenge{ReceiptRef: ref, Positions: positions, Layers: layers, DeadlineUnix: now.Add(time.Minute).Unix(), VrfProof: proof}
	if err := core.Sign(challenge, key); err != nil {
		return nil, false, err
	}
	return challenge, true, nil
}

func CheckDraw(challenge *pb.AuditChallenge, receipt *pb.ExecutionReceipt) error {
	if err := core.Verify(challenge); err != nil {
		return err
	}
	ref, err := core.Digest(receipt)
	if err != nil {
		return err
	}
	if string(ref) != string(challenge.ReceiptRef) || !ed25519.Verify(challenge.Signer, append([]byte("ember/audit/draw/v1\x00"), ref...), challenge.VrfProof) {
		return errors.New("invalid draw proof")
	}
	return nil
}

func DrawPipeline(receipt *pb.PipelineExecutionReceipt, key ed25519.PrivateKey, percent uint32, now time.Time, policyLayers ...string) (*pb.AuditChallenge, bool, error) {
	if percent > 100 {
		return nil, false, errors.New("invalid audit rate")
	}
	ref, err := PipelineReceiptRef(receipt)
	if err != nil || receipt.InputTokens == 0 || receipt.OutputTokens == 0 ||
		receipt.ElapsedMs <= 0 || len(receipt.OutputCommitment) != 32 {
		return nil, false, errors.New("invalid pipeline receipt")
	}
	proof := ed25519.Sign(key, append([]byte("ember/audit/draw/v1\x00"), ref...))
	hash := sha256.Sum256(proof)
	drawn := binary.BigEndian.Uint64(hash[:8])%100 < uint64(percent)
	if !drawn {
		return nil, false, nil
	}
	count := receipt.OutputTokens
	layers, err := challengeLayers(policyLayers, []uint32{0, 14, 27})
	if err != nil {
		return nil, false, err
	}
	challenge := &pb.AuditChallenge{ReceiptRef: ref, Positions: []uint32{0, count / 2, count - 1},
		Layers: layers, DeadlineUnix: now.Add(time.Minute).Unix(), VrfProof: proof}
	if err := core.Sign(challenge, key); err != nil {
		return nil, false, err
	}
	return challenge, true, nil
}

// Finalize only slashes a numerical discrepancy when two independent references
// both reject; a protocol-invalid opening is objective evidence on its own.
func Finalize(book ledger.Backend, id string, first, second string, objective bool, now time.Time, multiplier int64) error {
	verdict := "service_failure"
	if objective || (first == "dispute" && second == "dispute") {
		verdict = "reject"
	} else if first == "accept" {
		verdict = "accept"
	}
	if first == "dispute" && second != "dispute" && !objective {
		return errors.New("reference disagreement requires review")
	}
	return book.Finish(id, verdict, now, multiplier)
}

func Canary(prompt string) bool {
	digest := sha256.Sum256([]byte(prompt))
	return digest[0] < 3 // roughly 1% of requests
}
