package core

import (
	"bytes"
	"errors"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// MoeReceiptFormatV2 identifies transport compaction over the logical v1 receipt.
const MoeReceiptFormatV2 uint32 = 2

// ExpandMoeReceipt restores a compact receipt without changing its input.
// Coordinator ruling supersedes design 4.1's compact-wire signing wording:
// signatures cover the logical v1 receipt, and compaction preserves signatures
// and frame roots. Header constants are inside the signed payload after
// expansion, so header tampering breaks segment signatures and is detected by
// the existing verifier. Clearing the transport metadata restores exact v1 bytes.
func ExpandMoeReceipt(receipt *pb.PipelineExecutionReceipt) (*pb.PipelineExecutionReceipt, error) {
	if receipt == nil {
		return nil, errors.New("missing MoE receipt")
	}
	out := proto.Clone(receipt).(*pb.PipelineExecutionReceipt)
	data := out.Moe
	if data == nil {
		return out, nil
	}
	switch data.FormatVersion {
	case 0:
		if data.Header != nil {
			return nil, errors.New("v1 MoE receipt has transport header")
		}
		return out, nil
	case MoeReceiptFormatV2:
	default:
		return nil, errors.New("unsupported MoE receipt format")
	}
	header := data.Header
	if header == nil || header.ChainId == "" || len(header.ManifestRoot) != 32 || header.ExecutionProfileVer == "" || header.RequestId == "" || header.RequestId != out.RequestId {
		return nil, errors.New("invalid compact MoE receipt header")
	}
	for _, segment := range data.SegmentReceipts {
		if segment == nil || segment.ChainId != "" || len(segment.ManifestRoot) != 0 || segment.ExecutionProfileVer != "" || segment.RequestId != "" {
			return nil, errors.New("compact MoE segment contains header fields")
		}
		segment.ChainId = header.ChainId
		segment.ManifestRoot = append([]byte(nil), header.ManifestRoot...)
		segment.ExecutionProfileVer = header.ExecutionProfileVer
		segment.RequestId = header.RequestId
	}
	for _, frame := range data.FrameReceipts {
		if frame == nil || frame.RequestId != "" {
			return nil, errors.New("compact MoE frame contains request id")
		}
		frame.RequestId = header.RequestId
	}
	data.Header = nil
	data.FormatVersion = 0
	return out, nil
}

// CompactMoeReceipt removes only repeated request constants from an independently
// signed v1 logical receipt. It never signs, changes roots or mutates its input.
func CompactMoeReceipt(receipt *pb.PipelineExecutionReceipt) (*pb.PipelineExecutionReceipt, error) {
	if receipt == nil || receipt.Moe == nil || len(receipt.Moe.SegmentReceipts) == 0 {
		return nil, errors.New("missing MoE segments for compaction")
	}
	if receipt.Moe.FormatVersion == MoeReceiptFormatV2 {
		if _, err := ExpandMoeReceipt(receipt); err != nil {
			return nil, err
		}
		return proto.Clone(receipt).(*pb.PipelineExecutionReceipt), nil
	}
	out, err := ExpandMoeReceipt(receipt)
	if err != nil {
		return nil, err
	}
	data := out.Moe
	first := data.SegmentReceipts[0]
	if first == nil {
		return nil, errors.New("missing MoE segment")
	}
	header := &pb.MoeReceiptHeader{ChainId: first.ChainId, ManifestRoot: append([]byte(nil), first.ManifestRoot...), ExecutionProfileVer: first.ExecutionProfileVer, RequestId: first.RequestId}
	if header.ChainId == "" || len(header.ManifestRoot) != 32 || header.ExecutionProfileVer == "" || header.RequestId == "" || header.RequestId != out.RequestId {
		return nil, errors.New("invalid MoE request constants")
	}
	for _, segment := range data.SegmentReceipts {
		if segment == nil || segment.ChainId != header.ChainId || !bytes.Equal(segment.ManifestRoot, header.ManifestRoot) || segment.ExecutionProfileVer != header.ExecutionProfileVer || segment.RequestId != header.RequestId {
			return nil, errors.New("inconsistent MoE segment request constants")
		}
		segment.ChainId = ""
		segment.ManifestRoot = nil
		segment.ExecutionProfileVer = ""
		segment.RequestId = ""
	}
	for _, frame := range data.FrameReceipts {
		if frame == nil || frame.RequestId != header.RequestId {
			return nil, errors.New("inconsistent MoE frame request id")
		}
		frame.RequestId = ""
	}
	data.FormatVersion = MoeReceiptFormatV2
	data.Header = header
	return out, nil
}
