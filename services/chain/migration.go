package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// MigrateV2State is the deterministic, manifest-gated 2 -> 3 -> 4 bridge.
// It never executes transactions, reconstructs signed bytes, or installs membership.
// encoding/json preserves byte slices and sorts map keys when State.Bytes hashes
// the result. Defaults apply only to absent fields, including explicit zero policies.
func MigrateV2State(raw []byte, upgrade MembershipUpgrade) (*State, error) {
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if s.Version != 2 {
		return nil, errors.New("migration requires state version 2")
	}
	var fields struct {
		Parameters map[string]json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	p := fields.Parameters
	// v2 -> v3. Other absent additions are exactly their nil/zero representation;
	// in particular no historical offense, latency, publication, or split is inferred.
	if _, ok := p["slash_floor_bps"]; !ok {
		s.Parameters.SlashFloorBPS = 100
	}
	if _, ok := p["slash_repeat_window_blocks"]; !ok {
		s.Parameters.SlashRepeatWindow = 1000
	}
	s.Offenses = nil // supervisor ruling: no fabricated confirmed-offense backfill
	s.Version = 3
	// v3 -> v4, including the later v4 audit/evidence additions. Hold-local policies
	// remain zero; they were never frozen by the legacy flow.
	if _, ok := p["assignment"]; !ok {
		s.Parameters.Assignment = AssignmentParameters{MBlocks: 3, CooldownRequests: 8}
	}
	if _, ok := p["audit_work"]; !ok {
		s.Parameters.AuditWork = AuditWorkParameters{EpochBlocks: 1000, MaxPerExecutor: 64}
	}
	if _, ok := p["misjudge"]; !ok {
		s.Parameters.Misjudge.defaults()
	}
	s.Version = 4
	// Reuse existing validation and detach the manifest without altering activation.
	app := &Application{committed: &s}
	if err := app.ConfigureMembershipUpgrade(upgrade); err != nil {
		return nil, err
	}
	// Handshake bridge. The pre-migration binary's SaveState wrote
	// json.Marshal of the very struct its Hash() hashed, so the state
	// file's raw bytes are the preimage of the AppHash it committed at
	// s.Height and CometBFT pinned in its state store. Record that hash
	// so Info can present it at the boundary height (see State.AppHash);
	// the migrated state's own Hash() is unchanged.
	legacy := sha256.Sum256(raw)
	s.legacyAppHash = legacy[:]
	s.legacyHeight = s.Height
	return &s, nil
}

// A pre-S47 hold has no frozen request identity/transaction/policy. These persisted
// facts survive restart and activation; every new accepted freeze pins all three.
func (h *Hold) legacyV2() bool {
	return h.RequestID == "" && len(h.FreezeTransaction) == 0 && len(h.FreezePayload) == 0 && h.AssignmentPolicy == (AssignmentParameters{}) && h.Pipeline == nil
}

func (h *Hold) legacyUngraded() bool {
	if !h.legacyV2() || h.FirstGrade != nil {
		return false
	}
	v := &pb.AuditVerdict{}
	return proto.Unmarshal(h.VerdictBytes, v) == nil && v.Offense == nil
}

// The v2 domain and round byte are retained, using the original eligible pool.
// An already assigned auditor/proof is never redrawn by migration.
func (s *State) legacyAuditor(h *Hold, ids []string) (string, []byte, error) {
	if len(s.Entropy) != sha256.Size {
		return "", nil, errors.New("chain randomness unavailable")
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "", nil, errors.New("no independent auditor available")
	}
	digest := sha256.New()
	digest.Write([]byte("ember/audit-selection/v1\x00"))
	digest.Write(s.Entropy)
	digest.Write([]byte(h.ID))
	digest.Write([]byte(h.ReceiptRef))
	digest.Write([]byte{byte(h.AuditRound + 1)})
	proof := digest.Sum(nil)
	return ids[binary.BigEndian.Uint64(proof[:8])%uint64(len(ids))], proof, nil
}

// Legacy signed rejections have no offense grade. Preserve their original
// confirmed-rejection settlement, without minting graded/historical records.
func (s *State) settleLegacyRejection(h *Hold) error {
	penalty, err := multiply(h.Actual, s.Parameters.SlashMultiplier)
	if err != nil {
		return err
	}
	if penalty > s.Accounts[h.Node].Stake {
		penalty = s.Accounts[h.Node].Stake
	}
	if err := s.creditBalance(h.Payer, penalty); err != nil {
		return err
	}
	s.Accounts[h.Node].Stake -= penalty
	h.State = "slashed"
	return nil
}
