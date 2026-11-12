package chain

import (
	"bytes"
	"crypto/sha256"
	"errors"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// FrozenFee pins the signed pricing policy and serving-stack tokenization.
// Omission preserves historical holds and transaction encodings.
type FrozenFee struct {
	Profile      []byte `json:"profile"`
	Commitment   []byte `json:"commitment"`
	OutputTokens uint32 `json:"output_tokens,omitempty"`
}

func (s *State) freezeFee(a Action, model, node string) (*FrozenFee, error) {
	raw, _ := modelArtifacts(s.Models[model])
	g := &pb.ChainGenesis{}
	if len(raw) == 0 {
		if len(a.PricingCommitment) != 0 {
			return nil, errors.New("pricing requires anchored profile")
		}
		return nil, nil
	}
	if proto.Unmarshal(raw, g) != nil {
		return nil, errors.New("invalid pricing profile")
	}
	if len(a.PricingCommitment) == 0 {
		if g.PriceInput != 0 || g.PriceContext != 0 {
			return nil, errors.New("tokenization commitment required")
		}
		if g.PriceOutput > 0 {
			return &FrozenFee{Profile: append([]byte(nil), raw...)}, nil
		}
		return nil, nil
	}
	c := &pb.ExecutionCommitment{}
	if proto.Unmarshal(a.PricingCommitment, c) != nil || core.Verify(c) != nil || c.RequestId != a.HoldID || c.ChainId != model || c.ProfileVer != g.ExecutionProfileVer || c.InputTokens == 0 || c.MaxOutputTokens == 0 || len(c.InputCommitment) != 32 {
		return nil, errors.New("invalid tokenization commitment")
	}
	if (node != "" && core.NodeID(c.Signer) != node) || (node == "" && !s.Coordinators[core.NodeID(c.Signer)]) {
		return nil, errors.New("unauthorized tokenization signer")
	}
	fee, err := core.TokenFee(g, c.InputTokens, c.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	if a.Amount != fee.Total {
		return nil, errors.New("freeze amount does not match token fee")
	}
	return &FrozenFee{Profile: append([]byte(nil), raw...), Commitment: append([]byte(nil), a.PricingCommitment...)}, nil
}

func verifyTokenFee(f *FrozenFee, input, output uint32, amount int64, commitmentRef []byte) error {
	if f == nil {
		return nil
	}
	g, c := &pb.ChainGenesis{}, &pb.ExecutionCommitment{}
	if proto.Unmarshal(f.Profile, g) != nil {
		return errors.New("invalid frozen pricing policy")
	}
	// Historical output-only requests keep their original signed receipt bytes.
	if len(f.Commitment) == 0 {
		if g.PriceInput != 0 || g.PriceContext != 0 {
			return errors.New("missing frozen tokenization")
		}
		fee, err := core.TokenFee(g, 0, output)
		if err != nil {
			return err
		}
		if amount != fee.Total {
			return errors.New("settlement amount does not match token fee")
		}
		return nil
	}
	if proto.Unmarshal(f.Commitment, c) != nil || input != c.InputTokens || output > c.MaxOutputTokens {
		return errors.New("receipt counts differ from frozen tokenization")
	}
	if commitmentRef != nil {
		ref := sha256.Sum256(f.Commitment)
		if !bytes.Equal(ref[:], commitmentRef) {
			return errors.New("receipt tokenization reference mismatch")
		}
	}
	fee, err := core.TokenFee(g, input, output)
	if err != nil {
		return err
	}
	if amount != fee.Total {
		return errors.New("settlement amount does not match token fee")
	}
	return nil
}

func verifyFrozenSettlement(h *Hold) error {
	if h.TokenFee == nil {
		return nil
	}
	c := &pb.ExecutionCommitment{}
	if len(h.TokenFee.Commitment) > 0 && proto.Unmarshal(h.TokenFee.Commitment, c) != nil {
		return errors.New("invalid frozen tokenization")
	}
	return verifyTokenFee(h.TokenFee, c.InputTokens, h.TokenFee.OutputTokens, h.Actual, nil)
}
