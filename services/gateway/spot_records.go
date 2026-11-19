package gateway

// Pipeline record commitments are separate from ETV1 range/shard tensor leaves.
// Original protobuf/JSON records are hashed without reserializing or running a model.
import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	pb "ember/proto"
	chain "ember/services/chain"
	"google.golang.org/protobuf/proto"
)

const PipelineRecordFormat = "pipeline-record-hashes-v1"

type SpotRecordDigests struct {
	Indices        []int    `json:"indices"`
	RequestHashes  []string `json:"request_hashes"`
	ResponseHashes []string `json:"response_hashes"`
	OpeningHash    string   `json:"opening_hash"`
}

type PipelineSpotCommitment struct {
	Kind           string              `json:"kind"`
	RequestID      string              `json:"request_id"`
	CommitmentRoot string              `json:"commitment_root"`
	SpotStepCount  uint64              `json:"spot_step_count"`
	Leaves         []map[string]any    `json:"leaves"`
	Records        []SpotRecordDigests `json:"records"`
}

func recordDigest(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.CopyBuffer(h, r, make([]byte, 64<<10)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func compositeDigest(values []string) ([32]byte, error) {
	h := sha256.New()
	if len(values) == 0 {
		return [32]byte{}, errors.New("empty digest table")
	}
	for _, v := range values {
		raw, err := hex.DecodeString(v)
		if err != nil || len(raw) != 32 {
			return [32]byte{}, errors.New("invalid record digest")
		}
		h.Write(raw)
	}
	return spotDigest(h), nil
}

// BuildPipelineSpotCommitment traverses indexed, caller-owned spool records.
// At most one invocation is decoded at a time; only hashes/indices remain resident.
func BuildPipelineSpotCommitment(e *PipelineExecution, requestID, profileID string) (*PipelineSpotCommitment, error) {
	rid, err := hex.DecodeString(requestID)
	if err != nil || len(rid) != 32 {
		return nil, errors.New("request ID must be 32 bytes")
	}
	if e.Source == nil || e.Receipt == nil {
		return nil, errors.New("indexed spool and receipt required")
	}
	openings := e.Source.Arrays["openings"]
	n := len(openings)
	if n == 0 || uint64(n) != uint64(e.Receipt.OutputTokens) {
		return nil, errors.New("opening count differs from receipt.output_tokens")
	}
	m := &PipelineSpotCommitment{Kind: "pipeline", RequestID: requestID, SpotStepCount: uint64(n), Records: make([]SpotRecordDigests, n)}
	for i := 0; i < e.InvocationCount(); i++ {
		inv, err := e.ReadInvocation(i)
		if err != nil {
			return nil, err
		}
		req := new(pb.ShardForwardRequest)
		if err = proto.Unmarshal(inv.Request, req); err != nil {
			return nil, err
		}
		if uint64(req.Step) >= uint64(n) {
			return nil, errors.New("request.step outside openings")
		}
		if req.RequestId != e.Receipt.RequestId {
			return nil, errors.New("request identity differs from receipt")
		}
		g := &m.Records[req.Step]
		a, b := sha256.Sum256(inv.Request), sha256.Sum256(inv.Response)
		g.Indices = append(g.Indices, i)
		g.RequestHashes = append(g.RequestHashes, hex.EncodeToString(a[:]))
		g.ResponseHashes = append(g.ResponseHashes, hex.EncodeToString(b[:]))
	}
	format, profile := sha256.Sum256([]byte(PipelineRecordFormat)), sha256.Sum256([]byte(profileID))
	leaves := make([]chain.SpotLeaf, n)
	for i := range openings {
		g := &m.Records[i]
		if len(g.Indices) == 0 {
			return nil, errors.New("opening missing stage invocations")
		}
		g.OpeningHash, err = recordDigest(e.Source.Reader(openings[i]))
		if err != nil {
			return nil, err
		}
		incoming, err := compositeDigest(g.RequestHashes)
		if err != nil {
			return nil, err
		}
		hashes := append(append([]string(nil), g.ResponseHashes...), g.OpeningHash)
		outgoing, err := compositeDigest(hashes)
		if err != nil {
			return nil, err
		}
		l := chain.SpotLeaf{Step: uint64(i + 1), InputHash: incoming, OutputHash: outgoing, FormatHash: format, ProfileIDHash: profile}
		copy(l.RequestID[:], rid)
		leaves[i] = l
		m.Leaves = append(m.Leaves, map[string]any{"step_index": i + 1, "input_hash": hex.EncodeToString(incoming[:]), "output_hash": hex.EncodeToString(outgoing[:]), "format_hash": hex.EncodeToString(format[:]), "profile_id_hash": hex.EncodeToString(profile[:]), "request_id": requestID})
	}
	root, err := SpotRoot(leaves)
	if err != nil {
		return nil, err
	}
	m.CommitmentRoot = hex.EncodeToString(root[:])
	return m, nil
}
