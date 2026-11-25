package verifier

import (
	"crypto/ed25519"
	"slices"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
)

func TestDrawPolicyLayers(t *testing.T) {
	seed := [32]byte{1}
	key := ed25519.NewKeyFromSeed(seed[:])
	receipt := &pb.ExecutionReceipt{RequestId: "whole", OutputTokens: 33}
	if err := core.Sign(receipt, key); err != nil {
		t.Fatal(err)
	}
	pipeline := &pb.PipelineExecutionReceipt{RequestId: "pipeline", InputTokens: 1,
		OutputTokens: 33, ElapsedMs: 1, OutputCommitment: make([]byte, 32)}
	now := time.Now()
	for _, test := range []struct {
		name           string
		layers         []string
		whole, sharded []uint32
	}{
		{"numeric", []string{"0", "11", "23"}, []uint32{0, 11, 23}, []uint32{0, 11, 23}},
		{"legacy", nil, []uint32{0, 14}, []uint32{0, 14, 27}},
	} {
		t.Run(test.name, func(t *testing.T) {
			whole, drawn, err := Draw(receipt, key, 100, now, test.layers...)
			if err != nil || !drawn || !slices.Equal(whole.GetLayers(), test.whole) {
				t.Fatalf("whole-model draw: layers=%v drawn=%v err=%v", whole.GetLayers(), drawn, err)
			}
			if err := CheckDraw(whole, receipt); err != nil {
				t.Fatal(err)
			}
			sharded, drawn, err := DrawPipeline(pipeline, key, 100, now, test.layers...)
			if err != nil || !drawn || !slices.Equal(sharded.GetLayers(), test.sharded) {
				t.Fatalf("pipeline draw: layers=%v drawn=%v err=%v", sharded.GetLayers(), drawn, err)
			}
			if err := core.Verify(sharded); err != nil {
				t.Fatal(err)
			}
		})
	}
}
