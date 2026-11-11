package ledger

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

type StageAuthorization struct {
	NodeID     string
	StartLayer uint32
	EndLayer   uint32
}

func reserveShare(amount int64, layers, total uint32) (int64, error) {
	if amount <= 0 || layers == 0 || total == 0 || layers > total {
		return 0, errors.New("invalid pipeline reservation")
	}
	n := new(big.Int).Mul(big.NewInt(amount), new(big.Int).SetUint64(uint64(layers)))
	n.Add(n, new(big.Int).SetUint64(uint64(total-1)))
	n.Quo(n, new(big.Int).SetUint64(uint64(total)))
	if !n.IsInt64() {
		return 0, errors.New("pipeline reservation overflow")
	}
	return n.Int64(), nil
}

func (l *Ledger) FreezePipeline(id, payer string, amount int64, authorizations []StageAuthorization, totalLayers uint32) error {
	if id == "" || payer == "" || amount <= 0 || totalLayers == 0 || len(authorizations) == 0 {
		return errors.New("invalid pipeline freeze")
	}
	type reservedAuthorization struct {
		StageAuthorization
		amount int64
	}
	reserved := make([]reservedAuthorization, len(authorizations))
	seen := map[string]bool{}
	for i, authorization := range authorizations {
		key := fmt.Sprintf("%s/%d/%d", authorization.NodeID, authorization.StartLayer, authorization.EndLayer)
		if authorization.NodeID == "" || authorization.EndLayer < authorization.StartLayer || (authorization.EndLayer == authorization.StartLayer && authorization.StartLayer != 0 && authorization.StartLayer != totalLayers) || authorization.EndLayer > totalLayers || seen[key] {
			return errors.New("invalid or duplicate pipeline authorization")
		}
		seen[key] = true
		value, err := reserveShare(amount, core.StageWeight(authorization.StartLayer, authorization.EndLayer), totalLayers)
		if err != nil {
			return err
		}
		reserved[i] = reservedAuthorization{authorization, value}
	}
	return l.transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO pipeline_holds(id,payer,amount,state) VALUES(?,?,?,'frozen')`, id, payer, amount); err != nil {
			return err
		}
		for _, authorization := range reserved {
			var stake, exposure int64
			if err := tx.QueryRow(`SELECT stake FROM accounts WHERE id=?`, authorization.NodeID).Scan(&stake); err != nil {
				return err
			}
			if err := tx.QueryRow(`SELECT coalesce((SELECT sum(amount) FROM holds WHERE node=? AND state IN ('frozen','dispute')),0)+coalesce((SELECT sum(pa.reserved) FROM pipeline_authorizations pa JOIN pipeline_holds ph ON ph.id=pa.hold_id WHERE pa.node=? AND ph.state IN ('frozen','dispute')),0)`, authorization.NodeID, authorization.NodeID).Scan(&exposure); err != nil {
				return err
			}
			if authorization.amount > stake/5-exposure {
				return fmt.Errorf("stake exposure limit for stage %s", authorization.NodeID)
			}
			if _, err := tx.Exec(`INSERT INTO pipeline_authorizations(hold_id,node,start_layer,end_layer,reserved) VALUES(?,?,?,?,?)`,
				id, authorization.NodeID, authorization.StartLayer, authorization.EndLayer, authorization.amount); err != nil {
				return err
			}
		}
		return move(tx, payer, "balance", -amount)
	})
}

func validatePipelineReceipt(receipt *pb.PipelineExecutionReceipt, totalLayers uint32) ([]StageWork, error) {
	if receipt == nil || receipt.RequestId == "" || receipt.InputTokens == 0 || receipt.OutputTokens == 0 || receipt.ElapsedMs <= 0 || len(receipt.OutputCommitment) != 32 || totalLayers == 0 {
		return nil, errors.New("invalid pipeline receipt")
	}
	type span struct{ start, end uint32 }
	coverage := map[uint32][]span{}
	work := []StageWork{}
	for _, stage := range receipt.StageReceipts {
		if core.Verify(stage) != nil || stage.RequestId != receipt.RequestId || stage.Step >= receipt.OutputTokens || stage.NodeId != core.NodeID(stage.Signer) || len(stage.RequestHash) != 32 || len(stage.ResponseHash) != 32 || len(stage.ParametersHash) != 32 {
			return nil, errors.New("invalid signed stage receipt")
		}
		embedding, head := false, false
		switch stage.UnitKind {
		case "":
			embedding = stage.StartLayer == 0
			head = stage.EndLayer == totalLayers
		case "embedding":
			embedding = true
		case "lm_head":
			head = true
		case "layers":
		default:
			return nil, errors.New("unsupported stage unit")
		}
		start, end, err := core.StageSpan(stage.StartLayer, stage.EndLayer, totalLayers, embedding, head)
		if err != nil {
			return nil, err
		}
		coverage[stage.Step] = append(coverage[stage.Step], span{start, end})
		work = append(work, StageWork{NodeID: stage.NodeId, StartLayer: stage.StartLayer, EndLayer: stage.EndLayer, Tokens: 1})
	}
	for step := uint32(0); step < receipt.OutputTokens; step++ {
		spans := coverage[step]
		sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
		cursor := uint32(0)
		for _, v := range spans {
			if v.start != cursor {
				return nil, errors.New("pipeline gap or overlap")
			}
			cursor = v.end
		}
		if cursor != totalLayers+2 {
			return nil, errors.New("incomplete pipeline unit coverage")
		}
	}
	return work, nil
}

func (l *Ledger) SubmitPipeline(id string, receipt *pb.PipelineExecutionReceipt, actual int64, totalLayers uint32, window time.Duration) ([]StageShare, error) {
	if actual <= 0 || window < 0 || receipt == nil || receipt.RequestId != id {
		return nil, errors.New("invalid pipeline submission")
	}
	work, err := validatePipelineReceipt(receipt, totalLayers)
	if err != nil {
		return nil, err
	}
	weighted := append([]StageWork(nil), work...)
	for i := range weighted {
		if weighted[i].StartLayer == weighted[i].EndLayer {
			weighted[i].StartLayer = 0
			weighted[i].EndLayer = 1
		}
	}
	shares, err := SplitInvocationCharge(actual, weighted)
	if err != nil {
		return nil, err
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	err = l.transaction(func(tx *sql.Tx) error {
		var held int64
		var state string
		if err := tx.QueryRow(`SELECT amount,state FROM pipeline_holds WHERE id=?`, id).Scan(&held, &state); err != nil {
			return err
		}
		if state != "frozen" || actual > held {
			return errors.New("invalid pipeline receipt transition")
		}
		for _, invocation := range work {
			var count int
			if err := tx.QueryRow(`SELECT count(*) FROM pipeline_authorizations WHERE hold_id=? AND node=? AND start_layer=? AND end_layer=?`,
				id, invocation.NodeID, invocation.StartLayer, invocation.EndLayer).Scan(&count); err != nil || count != 1 {
				return errors.New("stage was not authorized for this hold")
			}
		}
		previous := make([]byte, 32)
		if err := tx.QueryRow(`SELECT hash FROM pipeline_receipts ORDER BY seq DESC LIMIT 1`).Scan(&previous); err != nil && err != sql.ErrNoRows {
			return err
		}
		h := sha256.New()
		h.Write(previous)
		h.Write([]byte(id))
		h.Write(raw)
		sum := h.Sum(nil)
		if _, err := tx.Exec(`INSERT INTO pipeline_receipts(hold_id,receipt,previous,hash) VALUES(?,?,?,?)`, id, raw, previous, sum); err != nil {
			return err
		}
		for _, share := range shares {
			if _, err := tx.Exec(`INSERT INTO pipeline_credits(hold_id,node,work,amount) VALUES(?,?,?,?)`, id, share.NodeID, share.Work, share.Amount); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE pipeline_holds SET actual=?,state='dispute',receipt=?,deadline=? WHERE id=?`,
			actual, sum, time.Now().Add(window).Unix(), id)
		return err
	})
	return shares, err
}

func (l *Ledger) FinishPipeline(id, verdict string, now time.Time, slashMultiplier int64) error {
	if verdict != "accept" && verdict != "reject" && verdict != "service_failure" {
		return errors.New("invalid pipeline verdict")
	}
	return l.transaction(func(tx *sql.Tx) error {
		faults := map[string]bool{}
		if verdict == "reject" {
			var raw []byte
			if err := tx.QueryRow(`SELECT verdict FROM pipeline_audits WHERE hold_id=?`, id).Scan(&raw); err != nil {
				return err
			}
			recorded := &pb.AuditVerdict{}
			if proto.Unmarshal(raw, recorded) != nil || core.Verify(recorded) != nil || recorded.Verdict != "reject" || len(recorded.FaultNodeIds) == 0 {
				return errors.New("reject requires signed fault attribution")
			}
			for _, node := range recorded.FaultNodeIds {
				if node == "" || faults[node] {
					return errors.New("invalid duplicate fault attribution")
				}
				faults[node] = true
			}
		}
		var payer, state string
		var held, actual, deadline int64
		if err := tx.QueryRow(`SELECT payer,amount,actual,state,deadline FROM pipeline_holds WHERE id=?`, id).Scan(&payer, &held, &actual, &state, &deadline); err != nil {
			return err
		}
		if state != "dispute" || (verdict == "accept" && now.Unix() < deadline) || (verdict == "reject" && slashMultiplier < 20) {
			return errors.New("invalid pipeline settlement transition")
		}
		if err := move(tx, payer, "balance", held-actual); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT node,amount FROM pipeline_credits WHERE hold_id=?`, id)
		if err != nil {
			return err
		}
		type credit struct {
			node   string
			amount int64
		}
		var credits []credit
		for rows.Next() {
			var value credit
			if err := rows.Scan(&value.node, &value.amount); err != nil {
				rows.Close()
				return err
			}
			credits = append(credits, value)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		target := "settled"
		if verdict == "accept" {
			for _, credit := range credits {
				if err := move(tx, credit.node, "balance", credit.amount); err != nil {
					return err
				}
			}
		} else {
			if err := move(tx, payer, "balance", actual); err != nil {
				return err
			}
			target = "cancelled"
			if verdict == "reject" {
				target = "slashed"
				for _, credit := range credits {
					if !faults[credit.node] {
						continue
					}
					delete(faults, credit.node)
					penalty := credit.amount * slashMultiplier
					if credit.amount > 0 && penalty/credit.amount != slashMultiplier {
						return errors.New("pipeline penalty overflow")
					}
					var stake int64
					if err := tx.QueryRow(`SELECT stake FROM accounts WHERE id=?`, credit.node).Scan(&stake); err != nil {
						return err
					}
					if penalty > stake {
						penalty = stake
					}
					if err := move(tx, credit.node, "stake", -penalty); err != nil {
						return err
					}
					if err := move(tx, payer, "balance", penalty); err != nil {
						return err
					}
				}
				if len(faults) != 0 {
					return errors.New("fault attribution references uncredited stage")
				}
			}
		}
		_, err = tx.Exec(`UPDATE pipeline_holds SET state=? WHERE id=?`, target, id)
		return err
	})
}

func (l *Ledger) CancelPipeline(id string) error {
	return l.transaction(func(tx *sql.Tx) error {
		var payer, state string
		var held int64
		if err := tx.QueryRow(`SELECT payer,amount,state FROM pipeline_holds WHERE id=?`, id).Scan(&payer, &held, &state); err != nil {
			return err
		}
		if state != "frozen" {
			return errors.New("cannot cancel pipeline hold")
		}
		if err := move(tx, payer, "balance", held); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE pipeline_holds SET state='cancelled' WHERE id=?`, id)
		return err
	})
}

func pipelineReceiptRef(receipt *pb.PipelineExecutionReceipt) ([]byte, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte("ember/pipeline-receipt/v1\x00"))
	h.Write(raw)
	return h.Sum(nil), nil
}

func (l *Ledger) RecordPipelineAudit(id string, challenge *pb.AuditChallenge, verdict *pb.AuditVerdict) error {
	if core.Verify(challenge) != nil || core.Verify(verdict) != nil || (verdict.Verdict != "accept" && verdict.Verdict != "reject" && verdict.Verdict != "inconclusive") {
		return errors.New("invalid pipeline audit")
	}
	challengeRef, err := core.Digest(challenge)
	if err != nil || !equal(verdict.ChallengeRef, challengeRef) {
		return errors.New("pipeline challenge reference mismatch")
	}
	challengeBytes, _ := proto.MarshalOptions{Deterministic: true}.Marshal(challenge)
	verdictBytes, _ := proto.MarshalOptions{Deterministic: true}.Marshal(verdict)
	return l.transaction(func(tx *sql.Tx) error {
		var receiptBytes []byte
		var state string
		if err := tx.QueryRow(`SELECT pr.receipt,ph.state FROM pipeline_receipts pr JOIN pipeline_holds ph ON ph.id=pr.hold_id WHERE ph.id=?`, id).Scan(&receiptBytes, &state); err != nil {
			return err
		}
		if state != "dispute" {
			return errors.New("pipeline hold not in dispute")
		}
		if verdict.Verdict == "reject" {
			if len(verdict.FaultNodeIds) == 0 {
				return errors.New("pipeline reject lacks fault attribution")
			}
			seen := map[string]bool{}
			for _, node := range verdict.FaultNodeIds {
				var count int
				if node == "" || seen[node] {
					return errors.New("invalid pipeline fault attribution")
				}
				seen[node] = true
				if err := tx.QueryRow(`SELECT count(*) FROM pipeline_credits WHERE hold_id=? AND node=?`, id, node).Scan(&count); err != nil || count != 1 {
					return errors.New("pipeline fault is not a credited stage")
				}
			}
		} else if len(verdict.FaultNodeIds) != 0 {
			return errors.New("non-reject verdict names fault nodes")
		}
		receipt := &pb.PipelineExecutionReceipt{}
		if err := proto.Unmarshal(receiptBytes, receipt); err != nil {
			return err
		}
		ref, err := pipelineReceiptRef(receipt)
		if err != nil || !equal(ref, challenge.ReceiptRef) {
			return errors.New("pipeline receipt reference mismatch")
		}
		_, err = tx.Exec(`INSERT INTO pipeline_audits(hold_id,challenge,verdict) VALUES(?,?,?)`, id, challengeBytes, verdictBytes)
		return err
	})
}

func (l *Ledger) RecordedPipelineVerdict(id string) (*pb.AuditVerdict, error) {
	var raw []byte
	if err := l.db.QueryRow(`SELECT verdict FROM pipeline_audits WHERE hold_id=?`, id).Scan(&raw); err != nil {
		return nil, err
	}
	verdict := &pb.AuditVerdict{}
	if err := proto.Unmarshal(raw, verdict); err != nil || core.Verify(verdict) != nil {
		return nil, errors.New("invalid recorded pipeline verdict")
	}
	return verdict, nil
}

func (l *Ledger) PipelineCredits(id string) ([]StageShare, error) {
	rows, err := l.db.Query(`SELECT node,work,amount FROM pipeline_credits WHERE hold_id=? ORDER BY node`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shares []StageShare
	for rows.Next() {
		var share StageShare
		if err := rows.Scan(&share.NodeID, &share.Work, &share.Amount); err != nil {
			return nil, err
		}
		shares = append(shares, share)
	}
	return shares, rows.Err()
}
