package tunnel

import (
	"bytes"
	"context"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/evidence"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *Server) FrameLimit() (uint32, error) {
	if s.EvidenceLimit != nil {
		return s.EvidenceLimit()
	}
	return evidence.DefaultMaxFrameTokens, nil
}

// FetchEvidence authorizes against the finalized hold, never caller-supplied roles.
func (s *Server) FetchEvidence(ctx context.Context, request *pb.TunnelFrame) (*pb.EvidenceChunk, error) {
	if request.Kind != "fetch_evidence" || core.Verify(request) != nil || request.NodeId != core.NodeID(request.Signer) || s.AuthorizeEvidence == nil || s.Evidence == nil {
		return nil, status.Error(codes.PermissionDenied, "evidence access denied")
	}
	query := &pb.EvidenceChunk{}
	if proto.Unmarshal(request.Payload, query) != nil || len(query.ManifestRoot) != 32 || len(query.Data) != 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid evidence query")
	}
	m, limit, err := s.AuthorizeEvidence(request.RequestId, request.NodeId)
	if err != nil || m == nil || !bytes.Equal(m.MerkleRoot, query.ManifestRoot) {
		return nil, status.Error(codes.PermissionDenied, "unknown or unauthorized evidence root")
	}
	if query.Index >= m.ChunkCount {
		return nil, status.Error(codes.NotFound, "unknown evidence index")
	}
	data, err := s.Evidence.Fetch(m, query.Index, limit)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "evidence unavailable")
	}
	return &pb.EvidenceChunk{ManifestRoot: query.ManifestRoot, Index: query.Index, Data: data}, nil
}
