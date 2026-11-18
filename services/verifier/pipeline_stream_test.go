package verifier

import (
	"bytes"
	core "ember/core-go"
	pb "ember/proto"
	"fmt"
	"reflect"
	"testing"
)

func TestPipelineStreamingParity(t *testing.T) {
	for _, name := range []string{"valid", "arbitrary arrival order", "deleted", "reordered without receipts", "request hash", "profile signature", "signed hidden", "signed prefix", "signed terminal", "signed nonterminal", "phase precedence"} {
		t.Run(name, func(t *testing.T) {
			receipt, inv := pipelineFixture(t)
			switch name {
			case "arbitrary arrival order":
				for i, j := 0, len(inv)-1; i < j; i, j = i+1, j-1 {
					inv[i], inv[j] = inv[j], inv[i]
					receipt.StageReceipts[i], receipt.StageReceipts[j] = receipt.StageReceipts[j], receipt.StageReceipts[i]
				}
			case "deleted":
				inv = inv[:len(inv)-1]
			case "reordered without receipts":
				inv[0], inv[1] = inv[1], inv[0]
			case "request hash":
				inv[0].Request[len(inv[0].Request)-1] ^= 1
			case "profile signature":
				inv[0].Profile[len(inv[0].Profile)-1] ^= 1
			case "signed hidden", "phase precedence":
				resignPipelineInvocation(t, receipt, inv, 1, func(q *pb.ShardForwardRequest, _ *pb.ShardForwardResponse) { q.HiddenBf16 = []byte{9, 9} })
			case "signed prefix":
				resignPipelineInvocation(t, receipt, inv, 3, func(q *pb.ShardForwardRequest, _ *pb.ShardForwardResponse) { q.InputIds[2] = 99 })
			case "signed terminal":
				resignPipelineInvocation(t, receipt, inv, 2, func(_ *pb.ShardForwardRequest, r *pb.ShardForwardResponse) { r.TokenId++ })
			case "signed nonterminal":
				resignPipelineInvocation(t, receipt, inv, 0, func(_ *pb.ShardForwardRequest, r *pb.ShardForwardResponse) { r.HiddenBf16 = nil; r.HiddenShape = nil })
			}
			if name == "phase precedence" {
				inv[5].Profile[len(inv[5].Profile)-1] ^= 1
			}
			old, oldErr := core.VerifyPipelineExecution(receipt, inv, "ember-p", bytes.Repeat([]byte{1}, 32), "p", 28)
			got, newErr := core.VerifyPipelineExecutionStream(receipt, len(inv), func(i int) (core.PipelineInvocation, error) { return inv[i], nil }, "ember-p", bytes.Repeat([]byte{1}, 32), "p", 28, t.TempDir())
			if fmt.Sprint(oldErr) != fmt.Sprint(newErr) || !reflect.DeepEqual(old, got) {
				t.Fatalf("old=%v %v new=%v %v", old, oldErr, got, newErr)
			}
			if name == "valid" || name == "arbitrary arrival order" {
				if newErr != nil {
					t.Fatal(newErr)
				}
			} else if newErr == nil {
				t.Fatal("negative accepted")
			}
		})
	}
}
