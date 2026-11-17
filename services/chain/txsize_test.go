package chain

import (
	abci "github.com/cometbft/cometbft/abci/types"
	"strings"
	"testing"
)

func TestVersionedEnvelopeBoundaries(t *testing.T) {
	for _, version := range []uint64{11, 12} {
		for _, size := range []int{(1 << 20) - 1, 1 << 20, (1 << 20) + 1, 1 << 24, (1 << 24) + 1} {
			s, actors := testState(t)
			s.Version = version
			a := Action{Type: "transfer", To: actors["node"].id, Amount: 1, Reason: "x"}
			raw, err := Sign(s.Parameters.ChainID, actors["payer"].key, 1, a)
			if err != nil {
				t.Fatal(err)
			}
			a.Reason = strings.Repeat("x", size-len(raw)+1)
			raw, err = Sign(s.Parameters.ChainID, actors["payer"].key, 1, a)
			if err != nil || len(raw) != size {
				t.Fatalf("fixture %d %v", len(raw), err)
			}
			_, _, decodeErr := DecodeAndVerifyVersion(raw, version)
			_, applyErr := s.Apply(raw, 1)
			limit := 1 << 20
			if version >= 12 {
				limit = 1 << 24
			}
			if size > limit {
				if decodeErr == nil || decodeErr.Error() != "invalid transaction envelope" || applyErr == nil || applyErr.Error() != decodeErr.Error() {
					t.Fatalf("v%d size%d: decode=%v apply=%v", version, size, decodeErr, applyErr)
				}
			} else if decodeErr != nil || applyErr != nil {
				t.Fatalf("v%d size%d: decode=%v apply=%v", version, size, decodeErr, applyErr)
			}
		}
	}
}

func TestV12Activation(t *testing.T) {
	for _, trial := range []bool{false, true} {
		for _, height := range []int64{9, 10, 11} {
			s, _ := testState(t)
			s.Version = 11
			s.Height = height - 1
			app := &Application{upgradeFromVersion: 11, upgrade: &MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 10}}
			var err error
			if trial {
				err = app.activateMembershipTrial(s, height)
			} else {
				err = app.activateMembership(s, &abci.FinalizeBlockRequest{Height: height})
			}
			if height == 11 {
				if err == nil || err.Error() != "v12 activation height missed" {
					t.Fatal(err)
				}
			} else if err != nil || (height == 10 && s.Version != 12) || (height == 9 && s.Version != 11) {
				t.Fatalf("height=%d version=%d err=%v", height, s.Version, err)
			}
		}
	}
}
