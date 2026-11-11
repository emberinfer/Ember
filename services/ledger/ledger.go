package ledger

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

type Ledger struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Ledger, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`PRAGMA foreign_keys=ON`,
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL)`,
		`INSERT INTO schema_meta(version) SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_meta)`,
		`CREATE TABLE IF NOT EXISTS accounts (id TEXT PRIMARY KEY, balance INTEGER NOT NULL DEFAULT 0 CHECK(balance>=0), stake INTEGER NOT NULL DEFAULT 0 CHECK(stake>=0))`,
		`CREATE TABLE IF NOT EXISTS tokens (digest BLOB PRIMARY KEY, account TEXT NOT NULL REFERENCES accounts(id))`,
		`CREATE TABLE IF NOT EXISTS holds (id TEXT PRIMARY KEY, payer TEXT NOT NULL REFERENCES accounts(id), node TEXT NOT NULL REFERENCES accounts(id), amount INTEGER NOT NULL CHECK(amount>0), actual INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL CHECK(state IN ('frozen','dispute','settled','slashed','cancelled')), receipt BLOB, deadline INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS receipts (seq INTEGER PRIMARY KEY AUTOINCREMENT, hold_id TEXT NOT NULL UNIQUE REFERENCES holds(id), receipt BLOB NOT NULL, previous BLOB NOT NULL, hash BLOB NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS audits (hold_id TEXT PRIMARY KEY REFERENCES receipts(hold_id), challenge BLOB NOT NULL, verdict BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS pipeline_holds (id TEXT PRIMARY KEY, payer TEXT NOT NULL REFERENCES accounts(id), amount INTEGER NOT NULL CHECK(amount>0), actual INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL CHECK(state IN ('frozen','dispute','settled','slashed','cancelled')), receipt BLOB, deadline INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS pipeline_authorizations (hold_id TEXT NOT NULL REFERENCES pipeline_holds(id), node TEXT NOT NULL REFERENCES accounts(id), start_layer INTEGER NOT NULL, end_layer INTEGER NOT NULL, reserved INTEGER NOT NULL CHECK(reserved>=0), PRIMARY KEY(hold_id,node,start_layer,end_layer))`,
		`CREATE TABLE IF NOT EXISTS pipeline_receipts (seq INTEGER PRIMARY KEY AUTOINCREMENT, hold_id TEXT NOT NULL UNIQUE REFERENCES pipeline_holds(id), receipt BLOB NOT NULL, previous BLOB NOT NULL, hash BLOB NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS pipeline_credits (hold_id TEXT NOT NULL REFERENCES pipeline_holds(id), node TEXT NOT NULL REFERENCES accounts(id), work INTEGER NOT NULL CHECK(work>0), amount INTEGER NOT NULL CHECK(amount>=0), PRIMARY KEY(hold_id,node))`,
		`CREATE TABLE IF NOT EXISTS pipeline_audits (hold_id TEXT PRIMARY KEY REFERENCES pipeline_receipts(hold_id), challenge BLOB NOT NULL, verdict BLOB NOT NULL)`,
		`CREATE TRIGGER IF NOT EXISTS receipts_no_update BEFORE UPDATE ON receipts BEGIN SELECT RAISE(ABORT,'append only'); END`,
		`CREATE TRIGGER IF NOT EXISTS receipts_no_delete BEFORE DELETE ON receipts BEGIN SELECT RAISE(ABORT,'append only'); END`,
		`CREATE TRIGGER IF NOT EXISTS audits_no_update BEFORE UPDATE ON audits BEGIN SELECT RAISE(ABORT,'append only'); END`,
		`CREATE TRIGGER IF NOT EXISTS audits_no_delete BEFORE DELETE ON audits BEGIN SELECT RAISE(ABORT,'append only'); END`,
		`CREATE TRIGGER IF NOT EXISTS pipeline_receipts_no_update BEFORE UPDATE ON pipeline_receipts BEGIN SELECT RAISE(ABORT,'append only'); END`,
		`CREATE TRIGGER IF NOT EXISTS pipeline_receipts_no_delete BEFORE DELETE ON pipeline_receipts BEGIN SELECT RAISE(ABORT,'append only'); END`,
		`CREATE TRIGGER IF NOT EXISTS pipeline_audits_no_update BEFORE UPDATE ON pipeline_audits BEGIN SELECT RAISE(ABORT,'append only'); END`,
		`CREATE TRIGGER IF NOT EXISTS pipeline_audits_no_delete BEFORE DELETE ON pipeline_audits BEGIN SELECT RAISE(ABORT,'append only'); END`,
	} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	var version int
	if err = db.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil || version != 1 {
		db.Close()
		return nil, fmt.Errorf("unsupported ledger schema version %d: %w", version, err)
	}
	return &Ledger{db: db}, nil
}
func (l *Ledger) Close() error { return l.db.Close() }
func (l *Ledger) DB() *sql.DB  { return l.db }

func (l *Ledger) transaction(fn func(*sql.Tx) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func move(tx *sql.Tx, account, column string, delta int64) error {
	if delta < 0 && delta == -delta {
		return errors.New("integer overflow")
	}
	res, err := tx.Exec(`UPDATE accounts SET `+column+`=`+column+`+? WHERE id=? AND `+column+`+?>=0`, delta, account, delta)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("insufficient funds or missing account %s", account)
	}
	return nil
}
func (l *Ledger) Create(id string, initial int64) error {
	if id == "" || initial < 0 {
		return errors.New("invalid account")
	}
	return l.transaction(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO accounts(id,balance) VALUES(?,?)`, id, initial)
		return err
	})
}
func (l *Ledger) Balance(id string) (int64, int64, error) {
	var b, s int64
	err := l.db.QueryRow(`SELECT balance,stake FROM accounts WHERE id=?`, id).Scan(&b, &s)
	return b, s, err
}
func (l *Ledger) IssueToken(id string, initial int64, token string) error {
	if id == "" || initial < 0 || len(token) < 32 {
		return errors.New("invalid account or token")
	}
	digest := sha256.Sum256([]byte(token))
	return l.transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO accounts(id,balance) VALUES(?,?)`, id, initial); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO tokens(digest,account) VALUES(?,?)`, digest[:], id)
		return err
	})
}
func (l *Ledger) TokenAccount(token string) (string, error) {
	if len(token) < 32 {
		return "", errors.New("invalid token")
	}
	digest := sha256.Sum256([]byte(token))
	var id string
	err := l.db.QueryRow(`SELECT account FROM tokens WHERE digest=?`, digest[:]).Scan(&id)
	return id, err
}
func (l *Ledger) Stake(id string, amount int64) error {
	if amount <= 0 {
		return errors.New("invalid stake")
	}
	return l.transaction(func(tx *sql.Tx) error {
		if err := move(tx, id, "balance", -amount); err != nil {
			return err
		}
		return move(tx, id, "stake", amount)
	})
}
func (l *Ledger) Freeze(id, payer, node string, amount int64) error {
	if id == "" || payer == node || amount <= 0 {
		return errors.New("invalid freeze")
	}
	return l.transaction(func(tx *sql.Tx) error {
		var stake, exposure int64
		if err := tx.QueryRow(`SELECT stake FROM accounts WHERE id=?`, node).Scan(&stake); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT coalesce(sum(amount),0) FROM holds WHERE node=? AND state IN ('frozen','dispute')`, node).Scan(&exposure); err != nil {
			return err
		}
		if amount > stake/5-exposure {
			return errors.New("stake exposure limit")
		}
		// Insert first: a duplicate request cannot debit the payer twice.
		if _, err := tx.Exec(`INSERT INTO holds(id,payer,node,amount,state) VALUES(?,?,?,?,'frozen')`, id, payer, node, amount); err != nil {
			return err
		}
		return move(tx, payer, "balance", -amount)
	})
}
func (l *Ledger) Submit(id string, receipt *pb.ExecutionReceipt, actual int64, window time.Duration) error {
	if actual < 0 || window < 0 {
		return errors.New("invalid charge or window")
	}
	if err := core.Verify(receipt); err != nil {
		return err
	}
	if receipt.RequestId != id {
		return errors.New("receipt request mismatch")
	}
	chunks := make([]proto.Message, len(receipt.ChunkRoots))
	for i, c := range receipt.ChunkRoots {
		chunks[i] = c
	}
	if err := core.VerifyChunks(chunks, id, receipt.OutputTokens, func(m proto.Message) (string, uint32, uint32, []byte, []byte) {
		c := m.(*pb.ChunkRoot)
		return c.RequestId, c.Seq, c.UptoStep, c.PrevRoot, c.MerkleRoot
	}); err != nil {
		return err
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	if err != nil {
		return err
	}
	return l.transaction(func(tx *sql.Tx) error {
		var node string
		var held int64
		var state string
		if err := tx.QueryRow(`SELECT node,amount,state FROM holds WHERE id=?`, id).Scan(&node, &held, &state); err != nil {
			return err
		}
		if state != "frozen" || node != receipt.NodeId || actual > held {
			return errors.New("invalid receipt transition")
		}
		previous := make([]byte, 32)
		err := tx.QueryRow(`SELECT hash FROM receipts ORDER BY seq DESC LIMIT 1`).Scan(&previous)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		h := sha256.New()
		h.Write(previous)
		h.Write([]byte(id))
		h.Write(raw)
		sum := h.Sum(nil)
		if _, err := tx.Exec(`INSERT INTO receipts(hold_id,receipt,previous,hash) VALUES(?,?,?,?)`, id, raw, previous, sum); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE holds SET actual=?,state='dispute',receipt=?,deadline=? WHERE id=?`, actual, sum, time.Now().Add(window).Unix(), id)
		return err
	})
}
func (l *Ledger) Finish(id, verdict string, now time.Time, slashMultiplier int64) error {
	if verdict != "accept" && verdict != "reject" && verdict != "service_failure" {
		return errors.New("invalid verdict")
	}
	return l.transaction(func(tx *sql.Tx) error {
		var payer, node, state string
		var held, actual, deadline int64
		if err := tx.QueryRow(`SELECT payer,node,amount,actual,state,deadline FROM holds WHERE id=?`, id).Scan(&payer, &node, &held, &actual, &state, &deadline); err != nil {
			return err
		}
		if state != "dispute" {
			return errors.New("not disputable")
		}
		if verdict == "accept" && now.Unix() < deadline {
			return errors.New("dispute window open")
		}
		if verdict == "reject" && slashMultiplier < 20 {
			return errors.New("unconfirmed or inadequate slash")
		}
		if err := move(tx, payer, "balance", held-actual); err != nil {
			return err
		}
		target := "settled"
		if verdict == "accept" {
			if err := move(tx, node, "balance", actual); err != nil {
				return err
			}
		} else {
			if err := move(tx, payer, "balance", actual); err != nil {
				return err
			}
			target = "cancelled"
			if verdict == "reject" {
				target = "slashed"
				var stake int64
				if err := tx.QueryRow(`SELECT stake FROM accounts WHERE id=?`, node).Scan(&stake); err != nil {
					return err
				}
				penalty := actual * slashMultiplier
				if actual > 0 && penalty/actual != slashMultiplier {
					return errors.New("penalty overflow")
				}
				if penalty > stake {
					penalty = stake
				}
				if err := move(tx, node, "stake", -penalty); err != nil {
					return err
				}
				if err := move(tx, payer, "balance", penalty); err != nil {
					return err
				}
			}
		}
		_, err := tx.Exec(`UPDATE holds SET state=? WHERE id=?`, target, id)
		return err
	})
}
func (l *Ledger) Cancel(id string) error {
	return l.transaction(func(tx *sql.Tx) error {
		var payer, state string
		var held int64
		if err := tx.QueryRow(`SELECT payer,amount,state FROM holds WHERE id=?`, id).Scan(&payer, &held, &state); err != nil {
			return err
		}
		if state != "frozen" {
			return errors.New("cannot cancel")
		}
		if err := move(tx, payer, "balance", held); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE holds SET state='cancelled' WHERE id=?`, id)
		return err
	})
}

// RecoverFrozen refunds requests that could not have produced a durable receipt.
func (l *Ledger) RecoverFrozen() (int64, error) {
	var recovered int64
	err := l.transaction(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id,payer,amount FROM holds WHERE state='frozen'`)
		if err != nil {
			return err
		}
		type hold struct {
			id, payer string
			amount    int64
		}
		var holds []hold
		for rows.Next() {
			var h hold
			if err := rows.Scan(&h.id, &h.payer, &h.amount); err != nil {
				rows.Close()
				return err
			}
			holds = append(holds, h)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, h := range holds {
			if err := move(tx, h.payer, "balance", h.amount); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE holds SET state='cancelled' WHERE id=? AND state='frozen'`, h.id); err != nil {
				return err
			}
			recovered++
		}
		rows, err = tx.Query(`SELECT id,payer,amount FROM pipeline_holds WHERE state='frozen'`)
		if err != nil {
			return err
		}
		holds = nil
		for rows.Next() {
			var h hold
			if err := rows.Scan(&h.id, &h.payer, &h.amount); err != nil {
				rows.Close()
				return err
			}
			holds = append(holds, h)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, h := range holds {
			if err := move(tx, h.payer, "balance", h.amount); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE pipeline_holds SET state='cancelled' WHERE id=? AND state='frozen'`, h.id); err != nil {
				return err
			}
			recovered++
		}
		return nil
	})
	return recovered, err
}

// RecordAudit binds immutable signed challenge/verdict bytes to the receipt
// before settlement. The demo coordinator signs the verdict on auditor output.
func (l *Ledger) RecordAudit(id string, challenge *pb.AuditChallenge, verdict *pb.AuditVerdict) error {
	if err := core.Verify(challenge); err != nil {
		return err
	}
	if err := core.Verify(verdict); err != nil {
		return err
	}
	if verdict.Verdict != "accept" && verdict.Verdict != "reject" && verdict.Verdict != "inconclusive" {
		return errors.New("invalid verdict")
	}
	challengeRef, err := core.Digest(challenge)
	if err != nil {
		return err
	}
	if !equal(verdict.ChallengeRef, challengeRef) {
		return errors.New("challenge reference mismatch")
	}
	challengeBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(challenge)
	if err != nil {
		return err
	}
	verdictBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(verdict)
	if err != nil {
		return err
	}
	return l.transaction(func(tx *sql.Tx) error {
		var receiptBytes []byte
		var state string
		if err := tx.QueryRow(`SELECT receipts.receipt, holds.state FROM receipts JOIN holds ON receipts.hold_id=holds.id WHERE holds.id=?`, id).Scan(&receiptBytes, &state); err != nil {
			return err
		}
		if state != "dispute" {
			return errors.New("not in dispute")
		}
		receipt := &pb.ExecutionReceipt{}
		if err := proto.Unmarshal(receiptBytes, receipt); err != nil {
			return err
		}
		ref, err := core.Digest(receipt)
		if err != nil {
			return err
		}
		if !equal(ref, challenge.ReceiptRef) {
			return errors.New("receipt reference mismatch")
		}
		_, err = tx.Exec(`INSERT INTO audits(hold_id,challenge,verdict) VALUES(?,?,?)`, id, challengeBytes, verdictBytes)
		return err
	})
}
func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
func (l *Ledger) State(id string) (string, error) {
	var state string
	err := l.db.QueryRow(`SELECT state FROM holds WHERE id=?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		err = l.db.QueryRow(`SELECT state FROM pipeline_holds WHERE id=?`, id).Scan(&state)
	}
	return state, err
}

func (l *Ledger) RecordedVerdict(id string) (*pb.AuditVerdict, error) {
	var raw []byte
	if err := l.db.QueryRow(`SELECT verdict FROM audits WHERE hold_id=?`, id).Scan(&raw); err != nil {
		return nil, err
	}
	verdict := &pb.AuditVerdict{}
	if err := proto.Unmarshal(raw, verdict); err != nil {
		return nil, err
	}
	if err := core.Verify(verdict); err != nil {
		return nil, err
	}
	return verdict, nil
}
func (l *Ledger) VerifyLog() error {
	rows, err := l.db.Query(`SELECT hold_id,receipt,previous,hash FROM receipts ORDER BY seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	previous := make([]byte, 32)
	for rows.Next() {
		var id string
		var receipt, prev, sum []byte
		if err := rows.Scan(&id, &receipt, &prev, &sum); err != nil {
			return err
		}
		h := sha256.New()
		h.Write(previous)
		h.Write([]byte(id))
		h.Write(receipt)
		got := h.Sum(nil)
		if hex.EncodeToString(prev) != hex.EncodeToString(previous) || hex.EncodeToString(sum) != hex.EncodeToString(got) {
			return errors.New("receipt hash chain corrupted")
		}
		previous = sum
	}
	return rows.Err()
}
