package chain

import (
	"bytes"
	"context"
	abci "github.com/cometbft/cometbft/abci/types"
	"path/filepath"
	"testing"
)

func TestSamplingSevenToEightActivationAndColdStart(t *testing.T) {
	s, a := testState(t)
	s.Version = 7
	s.Parameters.AuditSampling = AuditSamplingParameters{}
	s.Holds["historical"] = &Hold{ID: "historical", Node: a["node"].id, Auditor: a["auditor"].id, Actual: 16, State: "settled", AuditVerdict: "accept", FreezeHeight: 1, SettleHeight: 3}
	s.Height = 9
	app := &Application{committed: s}
	on := true
	p := AuditSamplingParameters{}
	p.defaults()
	p.Enabled = true
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 10, AuditSampling: &p, MoeEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	res, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 10, Hash: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if app.staged.Version != 8 || res.ConsensusParamUpdates == nil || res.ConsensusParamUpdates.Version.App != 8 {
		t.Fatal("version update not atomic")
	}
	if !app.staged.Parameters.AuditSampling.Enabled || !app.staged.Parameters.MoeEnabled || app.staged.NodeAuditStats[a["node"].id].AuditedAccepts != 1 {
		t.Fatal("migration/manifest")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, app.staged); err != nil {
		t.Fatal(err)
	}
	cold, err := OpenApplication(path)
	if err != nil {
		t.Fatal("cold start requires manifest", err)
	}
	got, _ := cold.committed.Hash()
	want, _ := app.staged.Hash()
	if !bytes.Equal(got, want) {
		t.Fatal("cold hash drift")
	}
}
