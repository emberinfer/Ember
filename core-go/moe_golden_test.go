package core

import (
	"encoding/hex"
	"testing"

	pb "ember/proto"
)

// Golden vectors shared with core-py/ember_core/moe.py: the Python mirror
// must reproduce these exact values (see core-py/test_core.py history and
// the MOE-2 verification notes). If a value changes, the wire semantics
// changed and both implementations must move together.
func TestMoeGoldenVectors(t *testing.T) {
	rec := &pb.RoutingRecord{Frame: 1, Layer: 2, Positions: 3, Experts: []uint32{0, 5, 5, 7, 2, 2, 1, 0, 3, 3, 4, 6}, InputHash: make([]byte, 32)}
	rh, _ := MoeRoutingRecordHash(rec)
	if got := hex.EncodeToString(rh); got != "96d65b4b61ccb50242013f9fb34aeaf2d8ac65e94bef286b81c59425496fd2b2" {
		t.Fatal("record hash drift: " + got)
	}
	rec2 := &pb.RoutingRecord{Frame: 1, Layer: 3, Positions: 3, Experts: []uint32{1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6}, InputHash: make([]byte, 32)}
	sh, _ := MoeRoutingSetHash([]*pb.RoutingRecord{rec2, rec})
	if got := hex.EncodeToString(sh); got != "704f665d2497b2f4825ea1e0602ad5cfbb6cd42dcc4b8681ad47d6a129d3a83f" {
		t.Fatal("set hash drift: " + got)
	}
	seg := &pb.SegmentReceipt{NodeId: "abc", ChainId: "c", RequestId: "r", Kind: "expert", Layer: 2, Expert: 5, Frame: 1, StartLayer: 2, EndLayer: 3, InputHash: make([]byte, 32), OutputHash: make([]byte, 32), RoutingRecordHash: rh, ParamsHash: make([]byte, 32), TokenRows: 4}
	sgh, _ := MoeSegmentReceiptHash(seg)
	if got := hex.EncodeToString(sgh); got != "a29a3402d45650522e5bceafe1f10d4825250e328e43ca12c53b165c7ef6be02" {
		t.Fatal("segment hash drift: " + got)
	}
	if got := hex.EncodeToString(MoeFrameRoot([][]byte{sgh, rh})); got != "f4b65f1683af09eb885601960d7b4e84d272fabff30672857fc160166b1b4d1d" {
		t.Fatal("frame root drift: " + got)
	}
	cands := []MoeSegmentCandidate{
		{Ref: &pb.SegmentRef{Kind: "expert", Layer: 0, Expert: 0, Frame: 0}, Work: 100},
		{Ref: &pb.SegmentRef{Kind: "expert", Layer: 0, Expert: 1, Frame: 0}, Work: 300},
		{Ref: &pb.SegmentRef{Kind: "attention", Layer: 0, Frame: 0, EndLayer: 2}, Work: 600},
		{Ref: &pb.SegmentRef{Kind: "expert", Layer: 1, Expert: 3, Frame: 1}, Work: 50},
	}
	drawn := DrawMoeSegments([]byte("golden-seed"), cands, 3)
	want := []string{"attention", "expert", "expert"}
	if len(drawn) != 3 {
		t.Fatal("draw length")
	}
	for i, w := range want {
		if drawn[i].Kind != w {
			t.Fatalf("draw %d kind = %s, want %s", i, drawn[i].Kind, w)
		}
	}
	if drawn[1].Expert != 1 || drawn[2].Expert != 0 {
		t.Fatal("draw expert order drift")
	}
}
