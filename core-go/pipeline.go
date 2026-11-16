package core

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sort"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

type PipelineInvocation struct {
	Request  []byte  `json:"request"`
	Response []byte  `json:"response"`
	Profile  []byte  `json:"profile"`
	StageMS  float64 `json:"stage_ms"`
}

type StageUse struct {
	NodeID     string
	StartLayer uint32
	EndLayer   uint32
}

type verifiedInvocation struct {
	request    *pb.ShardForwardRequest
	response   *pb.ShardForwardResponse
	use        StageUse
	start, end uint32
	terminal   bool
}

func MessageHash(message proto.Message) ([]byte, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(raw)
	return h[:], nil
}

func PipelineReceiptRef(receipt *pb.PipelineExecutionReceipt) ([]byte, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte("ember/pipeline-receipt/v1\x00"))
	h.Write(raw)
	return h.Sum(nil), nil
}

// VerifyPipelineExecution binds every invocation to an authenticated resource
// profile and requires exact per-token layer coverage.
func VerifyPipelineExecution(receipt *pb.PipelineExecutionReceipt, invocations []PipelineInvocation,
	chain string, manifest []byte, executionProfile string, totalLayers uint32) ([]StageUse, error) {
	if receipt == nil || receipt.RequestId == "" || receipt.InputTokens == 0 || receipt.OutputTokens == 0 || receipt.ElapsedMs <= 0 ||
		len(receipt.OutputCommitment) != 32 || totalLayers == 0 || len(invocations) != len(receipt.StageReceipts) {
		return nil, errors.New("invalid pipeline receipt")
	}
	byStep := make(map[uint32][]verifiedInvocation)
	uses := make([]StageUse, len(invocations))
	for i, wire := range invocations {
		request := &pb.ShardForwardRequest{}
		response := &pb.ShardForwardResponse{}
		profile := &pb.ResourceProfile{}
		if err := proto.Unmarshal(wire.Request, request); err != nil {
			return nil, fmt.Errorf("invocation %d request: %w", i, err)
		}
		if err := proto.Unmarshal(wire.Response, response); err != nil {
			return nil, fmt.Errorf("invocation %d response: %w", i, err)
		}
		if err := proto.Unmarshal(wire.Profile, profile); err != nil {
			return nil, fmt.Errorf("invocation %d profile: %w", i, err)
		}
		stage := response.Receipt
		if stage == nil || !proto.Equal(stage, receipt.StageReceipts[i]) || Verify(stage) != nil || Verify(profile) != nil {
			return nil, fmt.Errorf("invocation %d has invalid signed stage/profile", i)
		}
		if profile.NodeId != NodeID(profile.Signer) || stage.NodeId != profile.NodeId ||
			!bytes.Equal(stage.Signer, profile.Signer) || profile.ChainId != chain || stage.ChainId != chain ||
			!bytes.Equal(profile.ManifestRoot, manifest) || !bytes.Equal(stage.ManifestRoot, manifest) ||
			profile.ExecutionProfileVer != executionProfile || stage.ExecutionProfileVer != executionProfile ||
			stage.RequestId != receipt.RequestId || request.RequestId != receipt.RequestId ||
			stage.Step != request.Step || request.Step >= receipt.OutputTokens ||
			stage.UnitKind != profile.ShardUnitKind || stage.ShardIndex != profile.ShardIndex || (!profile.ShardDynamicRange && (stage.StartLayer != profile.ShardStartLayer || stage.EndLayer != profile.ShardEndLayer)) ||
			profile.ShardTotalLayers != totalLayers || stage.EndLayer > totalLayers ||
			!bytes.Equal(stage.ParametersHash, profile.ShardParametersHash) ||
			response.ShardIndex != profile.ShardIndex ||
			!bytes.Equal(response.ShardParametersHash, profile.ShardParametersHash) {
			return nil, fmt.Errorf("invocation %d binding mismatch", i)
		}
		if profile.ShardDynamicRange && (!request.DynamicRange || request.ExecutedStartLayer != stage.StartLayer || request.ExecutedEndLayer != stage.EndLayer) {
			return nil, errors.New("request/receipt executed-range mismatch")
		}
		if !profile.ShardDynamicRange && request.DynamicRange {
			return nil, errors.New("dynamic request for pinned stage")
		}
		start, end, spanErr := ExecutedSpan(profile, stage.StartLayer, stage.EndLayer)
		if spanErr != nil {
			return nil, spanErr
		}
		requestHash, err := MessageHash(request)
		if err != nil || !bytes.Equal(requestHash, stage.RequestHash) {
			return nil, fmt.Errorf("invocation %d request hash mismatch", i)
		}
		unsigned := proto.Clone(response).(*pb.ShardForwardResponse)
		unsigned.Receipt = nil
		responseHash, err := MessageHash(unsigned)
		if err != nil || !bytes.Equal(responseHash, stage.ResponseHash) {
			return nil, fmt.Errorf("invocation %d response hash mismatch", i)
		}
		if stage.UnitKind != "" {
			var input, output []byte
			if len(request.HiddenBf16) > 0 {
				input = ActivationHash(manifest, stage.StartLayer, request.HiddenBf16, request.HiddenShape)
			}
			if len(response.HiddenBf16) > 0 {
				output = ActivationHash(manifest, stage.EndLayer, response.HiddenBf16, response.HiddenShape)
			}
			if !bytes.Equal(input, stage.InputActivationHash) || !bytes.Equal(output, stage.OutputActivationHash) {
				return nil, errors.New("activation boundary commitment mismatch")
			}
		}
		use := StageUse{NodeID: stage.NodeId, StartLayer: stage.StartLayer, EndLayer: stage.EndLayer}
		uses[i] = use
		byStep[stage.Step] = append(byStep[stage.Step], verifiedInvocation{
			request: request, response: response, use: use, start: start, end: end, terminal: profile.ShardHasLmHead && stage.EndLayer == totalLayers,
		})
	}
	var initialTokens []int64
	generatedTokens := make([]int64, 0, receipt.OutputTokens)
	for step := uint32(0); step < receipt.OutputTokens; step++ {
		pipeline := byStep[step]
		sort.Slice(pipeline, func(i, j int) bool { return pipeline[i].start < pipeline[j].start })
		var cursor uint32
		for i, invocation := range pipeline {
			if invocation.start != cursor {
				return nil, fmt.Errorf("step %d pipeline gap or overlap at layer %d", step, cursor)
			}
			request, response := invocation.request, invocation.response
			terminal := invocation.terminal
			if i == 0 {
				if len(request.InputIds) == 0 || len(request.HiddenBf16) != 0 || len(request.HiddenShape) != 0 {
					return nil, fmt.Errorf("step %d first stage has invalid input", step)
				}
				expectedLength := int(receipt.InputTokens + step)
				if len(request.InputIds) != expectedLength {
					return nil, fmt.Errorf("step %d token prefix length mismatch", step)
				}
				if step == 0 {
					initialTokens = append([]int64(nil), request.InputIds...)
				} else {
					expected := append(append([]int64(nil), initialTokens...), generatedTokens...)
					if !equalInt64(request.InputIds, expected) {
						return nil, fmt.Errorf("step %d token prefix mismatch", step)
					}
				}
			} else {
				previous := pipeline[i-1].response
				if len(request.InputIds) != 0 || !validHidden(request.HiddenBf16, request.HiddenShape) ||
					!bytes.Equal(request.HiddenBf16, previous.HiddenBf16) || !equalUint32(request.HiddenShape, previous.HiddenShape) {
					return nil, fmt.Errorf("step %d hidden-state chain mismatch at layer %d", step, invocation.use.StartLayer)
				}
			}
			if terminal {
				if len(response.HiddenBf16) != 0 || len(response.HiddenShape) != 0 ||
					len(response.TopkIds) == 0 || len(response.TopkIds) != len(response.TopkLogits) ||
					response.TokenId != response.TopkIds[0] || !finiteFloat32(response.TopkLogits) {
					return nil, fmt.Errorf("step %d terminal stage has invalid output", step)
				}
				generatedTokens = append(generatedTokens, response.TokenId)
			} else if !validHidden(response.HiddenBf16, response.HiddenShape) ||
				len(response.TopkIds) != 0 || len(response.TopkLogits) != 0 {
				return nil, fmt.Errorf("step %d nonterminal stage has invalid output at layer %d", step, invocation.use.EndLayer)
			}
			cursor = invocation.end
		}
		if cursor != totalLayers+2 {
			return nil, fmt.Errorf("step %d incomplete pipeline coverage at layer %d", step, cursor)
		}
	}
	return uses, nil
}

func validHidden(raw []byte, shape []uint32) bool {
	if len(raw) == 0 || len(shape) != 3 {
		return false
	}
	count := uint64(1)
	for _, dimension := range shape {
		if dimension == 0 || count > math.MaxUint64/uint64(dimension) {
			return false
		}
		count *= uint64(dimension)
	}
	return count <= math.MaxUint64/2 && count*2 == uint64(len(raw))
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalUint32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func finiteFloat32(values []float32) bool {
	for _, value := range values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}
