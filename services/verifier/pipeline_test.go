package verifier

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func pipelineFixture(t *testing.T) (*pb.PipelineExecutionReceipt, []PipelineInvocation) {
	t.Helper()
	receipt := &pb.PipelineExecutionReceipt{RequestId: "request", OutputCommitment: bytes.Repeat([]byte{9}, 32), InputTokens: 2, OutputTokens: 2, ElapsedMs: 10}
	var invocations []PipelineInvocation
	ranges := [][2]uint32{{0, 9}, {9, 18}, {18, 28}}
	generated := []int64{7, 8}
	for step := uint32(0); step < 2; step++ {
		var hidden []byte
		for index, layers := range ranges {
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(index + 1)}, 32))
			profile := &pb.ResourceProfile{ChainId: "ember-p", ManifestRoot: bytes.Repeat([]byte{1}, 32),
				ExecutionProfileVer: "p", ShardIndex: uint32(index), ShardStartLayer: layers[0],
				ShardEndLayer: layers[1], ShardTotalLayers: 28, ShardHasEmbedding: index == 0,
				ShardHasLmHead: index == 2, ShardParametersHash: bytes.Repeat([]byte{byte(index + 4)}, 32)}
			if err := core.Sign(profile, key); err != nil {
				t.Fatal(err)
			}
			profile.NodeId = core.NodeID(profile.Signer)
			_ = core.Sign(profile, key)
			request := &pb.ShardForwardRequest{RequestId: "request", Step: step}
			response := &pb.ShardForwardResponse{ShardIndex: uint32(index), ShardParametersHash: profile.ShardParametersHash}
			if index == 0 {
				request.InputIds = append([]int64{1, 2}, generated[:step]...)
			} else {
				request.HiddenBf16, request.HiddenShape = hidden, []uint32{1, 1, 1}
			}
			if index == 2 {
				response.TokenId, response.TopkIds, response.TopkLogits = generated[step], []int64{generated[step]}, []float32{1}
			} else {
				hidden = []byte{byte(step), byte(index + 1)}
				response.HiddenBf16, response.HiddenShape = hidden, []uint32{1, 1, 1}
			}
			requestHash, _ := messageHash(request)
			responseHash, _ := messageHash(response)
			stage := &pb.ShardStepReceipt{NodeId: profile.NodeId, ChainId: "ember-p",
				ManifestRoot: profile.ManifestRoot, ExecutionProfileVer: "p", RequestId: "request",
				Step: step, ShardIndex: uint32(index), StartLayer: layers[0], EndLayer: layers[1],
				RequestHash: requestHash, ResponseHash: responseHash, ParametersHash: profile.ShardParametersHash}
			if err := core.Sign(stage, key); err != nil {
				t.Fatal(err)
			}
			response.Receipt = stage
			receipt.StageReceipts = append(receipt.StageReceipts, proto.Clone(stage).(*pb.ShardStepReceipt))
			requestWire, _ := proto.MarshalOptions{Deterministic: true}.Marshal(request)
			responseWire, _ := proto.MarshalOptions{Deterministic: true}.Marshal(response)
			profileWire, _ := proto.MarshalOptions{Deterministic: true}.Marshal(profile)
			invocations = append(invocations, PipelineInvocation{Request: requestWire, Response: responseWire, Profile: profileWire})
		}
	}
	return receipt, invocations
}

func resignPipelineInvocation(t *testing.T, receipt *pb.PipelineExecutionReceipt, invocations []PipelineInvocation, index int,
	mutate func(*pb.ShardForwardRequest, *pb.ShardForwardResponse)) {
	t.Helper()
	request := &pb.ShardForwardRequest{}
	response := &pb.ShardForwardResponse{}
	if err := proto.Unmarshal(invocations[index].Request, request); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(invocations[index].Response, response); err != nil {
		t.Fatal(err)
	}
	mutate(request, response)
	stage := proto.Clone(response.Receipt).(*pb.ShardStepReceipt)
	requestHash, _ := messageHash(request)
	stage.RequestHash = requestHash
	response.Receipt = nil
	responseHash, _ := messageHash(response)
	stage.ResponseHash = responseHash
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(stage.ShardIndex + 1)}, 32))
	if err := core.Sign(stage, key); err != nil {
		t.Fatal(err)
	}
	response.Receipt = stage
	receipt.StageReceipts[index] = proto.Clone(stage).(*pb.ShardStepReceipt)
	invocations[index].Request, _ = proto.MarshalOptions{Deterministic: true}.Marshal(request)
	invocations[index].Response, _ = proto.MarshalOptions{Deterministic: true}.Marshal(response)
}

func TestVerifyPipelineExecution(t *testing.T) {
	receipt, invocations := pipelineFixture(t)
	uses, err := VerifyPipelineExecution(receipt, invocations, "ember-p", bytes.Repeat([]byte{1}, 32), "p", 28)
	if err != nil || len(uses) != 6 {
		t.Fatalf("uses=%d err=%v", len(uses), err)
	}
	response := &pb.ShardForwardResponse{}
	_ = proto.Unmarshal(invocations[5].Response, response)
	response.TokenId = 99
	invocations[5].Response, _ = proto.Marshal(response)
	if _, err := VerifyPipelineExecution(receipt, invocations, "ember-p", bytes.Repeat([]byte{1}, 32), "p", 28); err == nil {
		t.Fatal("mutated stage response accepted")
	}
}

func TestVerifyPipelineExecutionRejectsSignedBrokenHiddenChain(t *testing.T) {
	receipt, invocations := pipelineFixture(t)
	resignPipelineInvocation(t, receipt, invocations, 1, func(request *pb.ShardForwardRequest, _ *pb.ShardForwardResponse) {
		request.HiddenBf16 = []byte{9, 9}
	})
	if _, err := VerifyPipelineExecution(receipt, invocations, "ember-p", bytes.Repeat([]byte{1}, 32), "p", 28); err == nil {
		t.Fatal("independently signed hidden-state substitution accepted")
	}
}

func TestVerifyPipelineExecutionRejectsSignedBrokenTokenPrefix(t *testing.T) {
	receipt, invocations := pipelineFixture(t)
	resignPipelineInvocation(t, receipt, invocations, 3, func(request *pb.ShardForwardRequest, _ *pb.ShardForwardResponse) {
		request.InputIds[2] = 99
	})
	if _, err := VerifyPipelineExecution(receipt, invocations, "ember-p", bytes.Repeat([]byte{1}, 32), "p", 28); err == nil {
		t.Fatal("independently signed token-prefix substitution accepted")
	}
}

func TestVerifyPipelineExecutionRejectsSignedInvalidStageOutputs(t *testing.T) {
	for _, test := range []struct {
		name   string
		index  int
		mutate func(*pb.ShardForwardRequest, *pb.ShardForwardResponse)
	}{
		{name: "nonterminal missing hidden", index: 0, mutate: func(_ *pb.ShardForwardRequest, response *pb.ShardForwardResponse) {
			response.HiddenBf16 = nil
			response.HiddenShape = nil
		}},
		{name: "terminal top-k mismatch", index: 2, mutate: func(_ *pb.ShardForwardRequest, response *pb.ShardForwardResponse) {
			response.TokenId++
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt, invocations := pipelineFixture(t)
			resignPipelineInvocation(t, receipt, invocations, test.index, test.mutate)
			if _, err := VerifyPipelineExecution(receipt, invocations, "ember-p", bytes.Repeat([]byte{1}, 32), "p", 28); err == nil {
				t.Fatal("independently signed invalid stage output accepted")
			}
		})
	}
}
