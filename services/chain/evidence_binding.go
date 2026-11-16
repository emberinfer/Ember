package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// verdictBindings isolates the wire representation from adjudication callers.
func verdictBindings(rawVerdict []byte, v *pb.AuditVerdict) (*int64, string, []byte, error) {
	if err := proto.Unmarshal(rawVerdict, v); err != nil {
		return nil, "", nil, err
	}
	return v.FirstDivergence, v.Tolerance, v.RecomputeRef, nil
}

// recomputeReference binds the auditor's own signed recompute record, including
// the applied tolerance and the absence of a positional divergence.
func recomputeReference(v *pb.AuditVerdict) ([]byte, error) {
	if len(v.ChallengeRef) != 32 || len(v.EvidenceRef) != 32 || uint64(len(v.Distances)) > math.MaxUint32 {
		return nil, errors.New("invalid recompute fields")
	}
	b := bytes.NewBufferString("ember/recompute/v1\x00")
	b.WriteString(v.AuditorId)
	b.WriteByte(0)
	b.Write(v.ChallengeRef)
	b.Write(v.EvidenceRef)
	b.WriteString(v.Verdict)
	b.WriteByte(0)
	pos := int64(-1)
	if v.FirstDivergence != nil {
		pos = *v.FirstDivergence
	}
	binary.Write(b, binary.BigEndian, pos)
	b.WriteString(v.Tolerance)
	b.WriteByte(0)
	binary.Write(b, binary.BigEndian, v.NOutBilled)
	binary.Write(b, binary.BigEndian, uint32(len(v.Distances)))
	for _, d := range v.Distances {
		if math.IsNaN(d) || math.IsInf(d, 0) {
			return nil, errors.New("non-finite recompute distance")
		}
		binary.Write(b, binary.BigEndian, math.Float64bits(d))
	}
	sum := sha256.Sum256(b.Bytes())
	return sum[:], nil
}

func (s *State) adjudicationEvidence(h *Hold, signer, outcome, evidenceRef string) RejectEvidence {
	e := RejectEvidence{Auditor: signer, ResultCommitment: h.ReceiptRef, ReplayRecord: evidenceRef}
	if h.FirstGrade != nil {
		e.Cause = h.FirstGrade.Class + ":" + h.FirstGrade.Fault + ":" + h.FirstGrade.Band
	}
	v := &pb.AuditVerdict{}
	pos, tolerance, ref, err := verdictBindings(h.VerdictBytes, v)
	if err != nil {
		e.ReplayRecord = ""
		return e
	}
	if len(ref) == 0 {
		return e
	} // Legacy/privacy evidence stays unbound.
	// A present but invalid binding must never borrow the legacy fallback.
	e.ReplayRecord = ""
	if len(ref) != 32 || v.AuditorId != signer || v.Verdict != outcome {
		return e
	}
	if v.Offense != nil {
		switch v.Offense.Class {
		case "token", "numerical":
			if v.Offense.Band == "" || tolerance != v.Offense.Band {
				return e
			}
		case "objective":
			if tolerance != "exact" {
				return e
			}
		default:
			return e
		}
	} else if outcome == "reject" {
		return e
	}
	if outcome == "reject" && pos != nil && (*pos < 0 || (h.AuditOutputTokens > 0 && *pos >= int64(h.AuditOutputTokens))) {
		return e
	}
	want, err := recomputeReference(v)
	if err != nil || !bytes.Equal(want, ref) {
		return e
	}
	e.ReplayRecord, e.FirstDivergence, e.Tolerance = hex.EncodeToString(ref), pos, tolerance
	return e
}
