package chain

import (
	"testing"

	pb "ember/proto"
)

// Freeze-side universal-expert exemption: a unit whose expert carries a
// pinned routing-share envelope is reserved at the exempted cap
// C_e = max(ceil(beta*k*T/E), ceil(u_e*T)), so its holder's worst-case
// reservation (and stake exposure) reflects the concentration the
// settlement check will actually accept. Toy layout here uses k=1,
// beta=1 so the exemption binds: T = 3+2 = 5, ordinary unit cap =
// ceil(1*1*5/4) = 2 (work 2*2*50 = 200), universal unit (0,0) at
// u=0.9 -> cap ceil(0.9*5) = 5 (work 500).
func TestMoeFreezeUniversalExemptionReserve(t *testing.T) {
	table := make([]uint32, 8)
	table[0] = 900_000 // (layer 0, expert 0)
	f := moeChainStateLayout(t, func(l *pb.MoeLayout) {
		l.TopK = 1
		l.RoutingCapBeta = 1
		l.RoutingUniversalPpm = table
	})
	apply(t, f.s, f.a["payer"], 1, Action{Type: "freeze_moe", HoldID: "moe-req",
		Payer: f.a["payer"].id, Amount: 1_000_000, MoeInputTokens: 3, MoeMaxTokens: 2,
		MoeAuthorizations: f.authorizations()}, 2)
	h := f.s.Holds["moe-req"]
	if h == nil || h.Moe == nil {
		t.Fatal("no MoE hold after freeze")
	}
	reserved := map[string]int64{}
	for _, auth := range h.Moe.Authorizations {
		reserved[auth.NodeID] = auth.Reserved
	}
	// expB holds the universal unit (0,0) plus three ordinary units:
	// work 500+200+200+200 = 1100 vs expC's 4*200 = 800. Reservations are
	// proportional to capped work, so expB must reserve strictly more.
	if reserved[f.expB.id] <= reserved[f.expC.id] {
		t.Fatalf("universal holder reserved %d, ordinary holder %d; want strictly greater",
			reserved[f.expB.id], reserved[f.expC.id])
	}
	// WorstSegRows tracks the largest per-expert cap:
	// max(cap 5, MaxFramePositions 4) * 8 = 40.
	if h.Moe.WorstSegRows != 40 {
		t.Fatalf("WorstSegRows = %d, want 40", h.Moe.WorstSegRows)
	}

	// Control: the identical layout without the pinned table reserves
	// both expert nodes equally and keeps WorstSegRows at the frame
	// bound max(cap 2, MaxFramePositions 4) * 8 = 32.
	f2 := moeChainStateLayout(t, func(l *pb.MoeLayout) {
		l.TopK = 1
		l.RoutingCapBeta = 1
	})
	apply(t, f2.s, f2.a["payer"], 1, Action{Type: "freeze_moe", HoldID: "moe-req",
		Payer: f2.a["payer"].id, Amount: 1_000_000, MoeInputTokens: 3, MoeMaxTokens: 2,
		MoeAuthorizations: f2.authorizations()}, 2)
	h2 := f2.s.Holds["moe-req"]
	reserved2 := map[string]int64{}
	for _, auth := range h2.Moe.Authorizations {
		reserved2[auth.NodeID] = auth.Reserved
	}
	if reserved2[f2.expB.id] != reserved2[f2.expC.id] {
		t.Fatalf("control reservations differ: expB %d expC %d",
			reserved2[f2.expB.id], reserved2[f2.expC.id])
	}
	if h2.Moe.WorstSegRows != 32 {
		t.Fatalf("control WorstSegRows = %d, want 32", h2.Moe.WorstSegRows)
	}
}
