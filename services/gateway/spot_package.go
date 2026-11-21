package gateway

import (
	"crypto/sha256"
	pb "ember/proto"
	"ember/services/auditqueue"
	chain "ember/services/chain"
	"ember/services/jsonspool"
	"encoding/hex"
	"encoding/json"
	"errors"
	"google.golang.org/protobuf/proto"
	"io"
)

func recordLeaf(value map[string]any) (chain.SpotLeaf, error) {
	raw, _ := json.Marshal(value)
	var v struct {
		Step    uint64 `json:"step_index"`
		Input   string `json:"input_hash"`
		Output  string `json:"output_hash"`
		Format  string `json:"format_hash"`
		Profile string `json:"profile_id_hash"`
		Request string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return chain.SpotLeaf{}, err
	}
	l := chain.SpotLeaf{Step: v.Step}
	for _, pair := range []struct {
		s string
		p *[32]byte
	}{{v.Input, &l.InputHash}, {v.Output, &l.OutputHash}, {v.Format, &l.FormatHash}, {v.Profile, &l.ProfileIDHash}, {v.Request, &l.RequestID}} {
		b, e := hex.DecodeString(pair.s)
		if e != nil || len(b) != 32 {
			return l, errors.New("invalid leaf digest")
		}
		copy(pair.p[:], b)
	}
	return l, nil
}

// BuildPipelineSpotPackage uses exactly B's pipeline_package disclosure set.
func BuildPipelineSpotPackage(e *PipelineExecution, m *PipelineSpotCommitment, step uint64) (map[string]any, error) {
	if step < 1 || step > m.SpotStepCount {
		return nil, errors.New("invalid package step")
	}
	hashes := make([][32]byte, len(m.Leaves))
	for i, v := range m.Leaves {
		l, err := recordLeaf(v)
		if err != nil {
			return nil, err
		}
		hashes[i] = chain.SpotLeafHash(l)
	}
	root, err := chain.SpotMerkleRoot(hashes)
	if err != nil || hex.EncodeToString(root[:]) != m.CommitmentRoot {
		return nil, errors.New("manifest root changed")
	}
	proof := func(i int) any {
		p, _ := chain.SpotMerkleProof(hashes, i)
		out := []map[string]any{}
		for _, v := range p {
			out = append(out, map[string]any{"hash": hex.EncodeToString(v.Hash[:]), "left": v.Left})
		}
		return out
	}
	disclose := func(i int, full bool) (map[string]any, error) {
		g := m.Records[i]
		v := map[string]any{"request_hashes": g.RequestHashes}
		for pos, index := range g.Indices {
			inv, err := e.ReadInvocation(index)
			if err != nil {
				return nil, err
			}
			req := new(pb.ShardForwardRequest)
			if err = proto.Unmarshal(inv.Request, req); err != nil {
				return nil, err
			}
			a, b := sha256.Sum256(inv.Request), sha256.Sum256(inv.Response)
			if req.Step != uint32(i) || hex.EncodeToString(a[:]) != g.RequestHashes[pos] || full && hex.EncodeToString(b[:]) != g.ResponseHashes[pos] {
				return nil, errors.New("spool invocation changed")
			}
			if pos == 0 {
				v["first_request"] = hex.EncodeToString(inv.Request)
			}
			if full && pos == len(g.Indices)-1 {
				v["last_response"] = hex.EncodeToString(inv.Response)
			}
		}
		if full {
			raw, err := io.ReadAll(e.Source.Reader(e.Source.Arrays["openings"][i]))
			if err != nil {
				return nil, err
			}
			h := sha256.Sum256(raw)
			if hex.EncodeToString(h[:]) != g.OpeningHash {
				return nil, errors.New("spool opening changed")
			}
			v["opening"] = hex.EncodeToString(raw)
			v["response_hashes"] = g.ResponseHashes
		}
		return v, nil
	}
	i := int(step - 1)
	v, err := disclose(i, true)
	if err != nil {
		return nil, err
	}
	v["kind"] = "pipeline"
	v["step_index"] = step
	v["leaf_count"] = m.SpotStepCount
	v["leaf"] = m.Leaves[i]
	v["proof"] = proof(i)
	if step < m.SpotStepCount {
		next, err := disclose(i+1, false)
		if err != nil {
			return nil, err
		}
		v["next"] = next
		v["next_leaf"] = m.Leaves[i+1]
		v["next_proof"] = proof(i + 1)
	}
	return v, nil
}

// WritePipelineSpotAudit sends the challenged openings, including bounded
// escalation, without the full stage invocation tensor array. The explicit
// escalation_full mode retains the historical full-package transport.
func WritePipelineSpotAudit(w io.Writer, source *jsonspool.Object, r *SpotDelivery, mode string) error {
	job := map[string]any{}
	metadata, err := source.Metadata("openings", "invocations", "failovers")
	if err != nil {
		return err
	}
	if err = json.Unmarshal(metadata, &job); err != nil {
		return err
	}
	// The auditor derives tokens from the complete output-committed openings.
	// Only the first invocation request is needed for the hold identity check;
	// ordinary spot audits still omit stage tensors and the full invocation array.
	execution := PipelineExecution{Source: source}
	if execution.InvocationCount() == 0 {
		return errors.New("context derivation requires first invocation")
	}
	first, err := execution.ReadInvocation(0)
	if err != nil {
		return err
	}
	job["first_invocation_request"] = hex.EncodeToString(first.Request)
	if mode == "escalation_full" {
		job["pipeline_full"] = true
	} else {
		delete(job, "invocations")
		delete(job, "failovers")
	}
	var genesis string
	if err := source.Decode("genesis", &genesis); err != nil {
		return err
	}
	job["kind"] = "spotcheck"
	job["mode"] = mode
	if mode == "escalation_full" {
		job["mode"] = "escalation"
	}
	job["genesis"] = genesis
	job["spot_delivery"] = r
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if mode == "escalation_full" {
		return WritePipelineAudit(w, raw, source)
	}
	return jsonspool.WriteMergeTransform(w, raw, source, nil, "openings")
}

// BuildDurablePipelineSpotCommitment owns reopening the final spool. The caller
// must have completed PutStream before invoking it; no transient Source is used.
func BuildDurablePipelineSpotCommitment(q *auditqueue.Queue, id, profile string, receipt *pb.PipelineExecutionReceipt) (*PipelineSpotCommitment, error) {
	f, err := q.OpenJob(id)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	source, err := jsonspool.Index(f)
	if err != nil {
		return nil, err
	}
	return BuildPipelineSpotCommitment(&PipelineExecution{Source: source, Receipt: receipt}, id, profile)
}
