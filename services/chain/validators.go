package chain

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/api/cometbft/types/v1"
	cmted "github.com/cometbft/cometbft/crypto/ed25519"
)

// EqualValidatorPower deliberately matches the existing genesis CLI's power.
const EqualValidatorPower int64 = 10

type Validator struct {
	PubKey   []byte `json:"pubkey"`
	Operator string `json:"operator"`
	OrgID    string `json:"org_id"`
	Power    int64  `json:"power"`
	Status   string `json:"status"`
}
type ValidatorReady struct {
	PubKey    []byte `json:"pubkey"`
	Height    int64  `json:"height"`
	Signature []byte `json:"signature"`
}
type ValidatorSetChange struct {
	Add             []Validator `json:"add"`
	Remove          []string    `json:"remove"` // canonical lower-case public-key hex
	EffectiveHeight int64       `json:"effective_height"`
	ProposedHeight  int64       `json:"proposed_height"`
	Status          string      `json:"status"`
	Reason          string      `json:"reason,omitempty"`
}
type Membership struct {
	ChangeDelayBlocks  int64                 `json:"change_delay_blocks"`
	ChangeWindowBlocks int64                 `json:"change_window_blocks"`
	ReadinessBlocks    int64                 `json:"readiness_blocks"`
	MaxValidators      int                   `json:"max_validators"`
	FloorReached       bool                  `json:"floor_reached"`
	Registry           map[string]*Validator `json:"registry"`
	Ready              map[string]int64      `json:"ready,omitempty"`
	Reachable          map[string]bool       `json:"reachable,omitempty"`
	Pending            *ValidatorSetChange   `json:"pending,omitempty"`
	History            []ValidatorSetChange  `json:"history,omitempty"`
	LastProposalHeight int64                 `json:"last_proposal_height,omitempty"`
}

func validatorID(key []byte) string { return hex.EncodeToString(key) }
func (m *Membership) active() map[string]bool {
	out := map[string]bool{}
	for id, v := range m.Registry {
		if v.Status == "active" || v.Status == "leaving" {
			out[id] = true
		}
	}
	return out
}
func (m *Membership) validate(s *State) error {
	if m.ChangeDelayBlocks < 3 || m.ChangeDelayBlocks > math.MaxInt64-2 || m.ChangeWindowBlocks < m.ChangeDelayBlocks+2 || m.ReadinessBlocks < 1 || m.ReadinessBlocks > 1000 || m.MaxValidators < 3 || m.MaxValidators > 1000 || len(m.Registry) < 2 || len(m.Registry) > m.MaxValidators || m.Pending != nil || len(m.History) != 0 {
		return errors.New("invalid initial membership parameters")
	}
	seen := map[string]bool{}
	for id, v := range m.Registry {
		if v == nil || len(v.PubKey) != ed25519.PublicKeySize || id != validatorID(v.PubKey) || v.Power != EqualValidatorPower || v.OrgID == "" || v.Status != "active" || seen[v.Operator] || s.Accounts[v.Operator] == nil {
			return errors.New("invalid initial validator identity")
		}
		seen[v.Operator] = true
	}
	m.FloorReached = len(m.Registry) >= 3
	m.Ready = nil
	m.Reachable = nil
	m.LastProposalHeight = 0
	return nil
}
func (m *Membership) matchGenesis(vs []abci.ValidatorUpdate) error {
	if len(vs) != len(m.Registry) {
		return errors.New("registry differs from consensus genesis")
	}
	seen := map[string]bool{}
	for _, v := range vs {
		id := validatorID(v.PubKeyBytes)
		if v.PubKeyType != "ed25519" || m.Registry[id] == nil || seen[id] || v.Power != EqualValidatorPower {
			return errors.New("registry differs from consensus genesis")
		}
		seen[id] = true
	}
	return nil
}

// ReadySignBytes binds a consensus-key proof of possession and recent chain
// observation to an operator. It is evidence of readiness, not an uptime promise.
func ReadySignBytes(chainID, operator string, pub []byte, height int64, hash []byte) []byte {
	raw, _ := json.Marshal(struct {
		ChainID, Operator string
		PubKey            []byte
		Height            int64
		Hash              []byte
	}{chainID, operator, pub, height, hash})
	return append([]byte("ember/validator-ready/v1\x00"), raw...)
}
func (s *State) validatorAdmitted(operator string, height int64) bool {
	a, r := s.Accounts[operator], s.Nodes[operator]
	return a != nil && a.Stake >= s.Parameters.MinNodeStake && r != nil && r.ExpiresHeight > height
}
func (s *State) validValidatorReady(v *Validator, r *ValidatorReady, h int64) bool {
	return r != nil && bytes.Equal(r.PubKey, v.PubKey) && r.Height > 0 && r.Height <= s.Height && h-r.Height <= s.Membership.ReadinessBlocks && len(s.BlockHashes[r.Height]) == 32 && ed25519.Verify(v.PubKey, ReadySignBytes(s.Parameters.ChainID, v.Operator, v.PubKey, r.Height, s.BlockHashes[r.Height]), r.Signature)
}
func (s *State) validatorAction(signer string, a Action, h int64) (bool, error) {
	m := s.Membership
	switch a.Type {
	case "validator_register", "validator_ready", "validator_set_change", "validator_cancel":
		if m == nil {
			return true, errors.New("validator dynamics not activated")
		}
	case "unbond":
		if m != nil {
			for _, v := range m.Registry {
				if v.Operator == signer && v.Status != "exited" {
					return true, errors.New("validator membership locks stake until exit")
				}
			}
		}
		return false, nil
	default:
		return false, nil
	}
	fail := func(msg string) (bool, error) { return true, errors.New(msg) }
	switch a.Type {
	case "validator_cancel":
		if a.Validator == nil {
			return fail("missing validator identity")
		}
		id := validatorID(a.Validator.PubKey)
		v := m.Registry[id]
		if v == nil || v.Operator != signer || v.Status != "pending" {
			return fail("only pending candidate may cancel admission")
		}
		if m.Pending != nil {
			for _, added := range m.Pending.Add {
				if bytes.Equal(added.PubKey, v.PubKey) {
					return fail("cannot cancel a proposed join")
				}
			}
		}
		v.Status = "exited"
		delete(m.Ready, id)
	case "validator_register":
		v := a.Validator
		if v == nil || len(v.PubKey) != 32 || v.Operator != signer || v.Power != EqualValidatorPower || v.OrgID == "" || v.Status != "pending" || !s.validatorAdmitted(signer, h) {
			return fail("invalid or under-staked validator registration")
		}
		id := validatorID(v.PubKey)
		if m.Registry[id] != nil {
			return fail("validator key already registered")
		}
		for _, old := range m.Registry {
			if old.Operator == signer && old.Status != "exited" {
				return fail("operator already has a validator")
			}
		}
		if len(m.Registry) >= 10000 {
			return fail("validator registry capacity reached")
		}
		if !s.validValidatorReady(v, a.Ready, h) {
			return fail("registration requires consensus-key readiness proof")
		}
		cp := *v
		cp.PubKey = append([]byte(nil), v.PubKey...)
		m.Registry[id] = &cp
		if m.Ready == nil {
			m.Ready = map[string]int64{}
		}
		m.Ready[id] = a.Ready.Height
	case "validator_ready":
		r := a.Ready
		if r == nil {
			return fail("missing readiness proof")
		}
		v := m.Registry[validatorID(r.PubKey)]
		if v == nil || v.Operator != signer || v.Status == "exited" || !s.validValidatorReady(v, r, h) {
			return fail("invalid or stale validator readiness")
		}
		if m.Ready == nil {
			m.Ready = map[string]int64{}
		}
		m.Ready[validatorID(r.PubKey)] = r.Height
	case "validator_set_change":
		c := a.SetChange
		authorized := false
		for _, v := range m.Registry {
			if v.Operator == signer && v.Status == "active" {
				authorized = true
			}
		}
		if !authorized {
			return fail("set change requires active validator operator")
		}
		if c == nil || len(c.Add)+len(c.Remove) != 1 || c.Status != "" || c.ProposedHeight != 0 || c.Reason != "" || h > math.MaxInt64-m.ChangeDelayBlocks || c.EffectiveHeight < h+m.ChangeDelayBlocks {
			return fail("one-member change and explicit timelock required")
		}
		if m.Pending != nil || (m.LastProposalHeight != 0 && h-m.LastProposalHeight < m.ChangeWindowBlocks) {
			return fail("validator change rate limited")
		}
		if c.EffectiveHeight > math.MaxInt64-2 {
			return fail("invalid effective height")
		}
		if len(c.Add) == 1 {
			v := c.Add[0]
			registered := m.Registry[validatorID(v.PubKey)]
			if registered == nil || registered.Status != "pending" || v.Status != "pending" || v.Operator != registered.Operator || v.OrgID != registered.OrgID || v.Power != EqualValidatorPower || !s.validatorAdmitted(v.Operator, h) {
				return fail("join requires candidate's signed admission")
			}
		} else {
			v := m.Registry[c.Remove[0]]
			if v == nil || v.Status != "active" || v.Operator != signer {
				return fail("voluntary exit must be signed by leaving operator")
			}
		}
		cp := *c
		cp.Add = append([]Validator(nil), c.Add...)
		cp.Remove = append([]string(nil), c.Remove...)
		cp.ProposedHeight = h
		cp.Status = "pending"
		if reason := s.checkContinuity(&cp, h); reason != "" {
			return fail(reason)
		}
		m.Pending = &cp
		m.LastProposalHeight = h
		for _, id := range cp.Remove {
			m.Registry[id].Status = "leaving"
		}
	}
	return true, nil
}
func (s *State) checkContinuity(c *ValidatorSetChange, h int64) string {
	m := s.Membership
	old := m.active()
	next := m.active()
	for _, v := range c.Add {
		next[validatorID(v.PubKey)] = true
	}
	for _, id := range c.Remove {
		delete(next, id)
	}
	if len(next) > m.MaxValidators || len(next) < 2 || (m.FloorReached && len(next) < 3) {
		return "validator set size floor/cap violated"
	}
	// Strict >2/3 on BOTH sets; last decided commit supplies old-set reachability.
	// A candidate must additionally demonstrate recent consensus-key possession.
	oldLive, newLive := 0, 0
	for id := range old {
		if m.Reachable[id] {
			oldLive++
		}
	}
	for id := range next {
		if old[id] {
			if m.Reachable[id] {
				newLive++
			}
		} else {
			v := m.Registry[id]
			at := m.Ready[id]
			if at <= 0 || h-at > m.ReadinessBlocks {
				return "candidate readiness missing or stale"
			}
			admissionHeight := h
			if h >= c.EffectiveHeight-2 {
				admissionHeight = c.EffectiveHeight
			}
			if v == nil || !s.validatorAdmitted(v.Operator, admissionHeight) {
				return "candidate admission expired before activation"
			}
			newLive++
		}
	}
	if oldLive*3 <= len(old)*2 || newLive*3 <= len(next)*2 {
		return fmt.Sprintf("quorum continuity rejected: old=%d/%d new=%d/%d require strict >2/3", oldLive, len(old), newLive, len(next))
	}
	return ""
}
func (s *State) observeValidatorVotes(req *abci.FinalizeBlockRequest) {
	if s.Membership == nil {
		return
	}
	live := map[string]bool{}
	for _, vote := range req.DecidedLastCommit.Votes {
		if vote.BlockIdFlag != cmtproto.BlockIDFlagCommit || vote.Validator.Power != EqualValidatorPower {
			continue
		}
		for id, v := range s.Membership.Registry {
			if bytes.Equal(cmted.PubKey(v.PubKey).Address(), vote.Validator.Address) {
				live[id] = true
			}
		}
	}
	s.Membership.Reachable = live
}
func (s *State) finalizeMembership(h int64) []abci.ValidatorUpdate {
	m := s.Membership
	if m == nil || m.Pending == nil {
		return nil
	}
	c := m.Pending
	if h == c.EffectiveHeight-2 {
		if reason := s.checkContinuity(c, h); reason != "" {
			c.Status = "rejected"
			c.Reason = reason
			for _, id := range c.Remove {
				m.Registry[id].Status = "active"
			}
			m.History = append(m.History, *c)
			m.Pending = nil
			return nil
		}
		c.Status = "scheduled"
		updates := []abci.ValidatorUpdate{}
		for _, v := range c.Add {
			updates = append(updates, abci.NewValidatorUpdate(cmted.PubKey(v.PubKey), EqualValidatorPower))
		}
		for _, id := range c.Remove {
			updates = append(updates, abci.NewValidatorUpdate(cmted.PubKey(m.Registry[id].PubKey), 0))
		}
		return updates
	}
	if h == c.EffectiveHeight && c.Status == "scheduled" {
		for _, v := range c.Add {
			m.Registry[validatorID(v.PubKey)].Status = "active"
		}
		for _, id := range c.Remove {
			m.Registry[id].Status = "exited"
		}
		if len(m.active()) >= 3 {
			m.FloorReached = true
		}
		c.Status = "applied"
		m.History = append(m.History, *c)
		m.Pending = nil
	}
	return nil
}

// MembershipUpgrade is a coordinated hard-fork manifest, identical on every
// validator. Version-4 bytes/hashes remain untouched until ActivationHeight.
type MembershipUpgrade struct {
	AuditSampling    *AuditSamplingParameters `json:"audit_sampling,omitempty"`
	MoeEnabled       *bool                    `json:"moe_enabled,omitempty"`
	ChainID          string                   `json:"chain_id"`
	ActivationHeight int64                    `json:"activation_height"`
	Membership       Membership               `json:"membership"`
}

func (app *Application) ConfigureMembershipUpgrade(u MembershipUpgrade) error {
	if u.AuditSampling != nil && !u.AuditSampling.valid() {
		return errors.New("invalid upgrade audit sampling parameters")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.committed == nil {
		if u.ChainID == "" || u.ActivationHeight < 2 {
			return errors.New("invalid historical replay activation manifest")
		}
		raw, _ := json.Marshal(u)
		var cp MembershipUpgrade
		_ = json.Unmarshal(raw, &cp)
		app.upgrade = &cp
		return nil
	}
	if app.committed != nil && app.committed.Version >= 6 && app.committed.Membership != nil && u.ChainID == app.committed.Parameters.ChainID && u.ActivationHeight <= app.committed.Height {
		return nil
	}
	if app.committed == nil || u.ChainID != app.committed.Parameters.ChainID || u.ActivationHeight <= app.committed.Height {
		return errors.New("upgrade requires matching initialized chain and future activation height")
	}
	if app.committed.Version == 5 {
		raw, _ := json.Marshal(u)
		var cp MembershipUpgrade
		_ = json.Unmarshal(raw, &cp)
		app.upgrade = &cp
		return nil
	}
	if app.committed.Version == 6 || app.committed.Version == 7 || app.committed.Version == 8 {
		raw, _ := json.Marshal(u)
		var cp MembershipUpgrade
		_ = json.Unmarshal(raw, &cp)
		app.upgrade = &cp
		return nil
	}
	if app.committed.Version != 4 || app.committed.Membership != nil {
		return errors.New("membership upgrade only accepts legacy version 4")
	}
	if err := u.Membership.validate(app.committed); err != nil {
		return err
	}
	// Detach caller-owned maps.
	raw, _ := json.Marshal(u)
	var cp MembershipUpgrade
	_ = json.Unmarshal(raw, &cp)
	app.upgrade = &cp
	return nil
}
func (app *Application) activateMembership(s *State, req *abci.FinalizeBlockRequest) error {
	if s.Version == 8 {
		if app.upgrade == nil || req.Height < app.upgrade.ActivationHeight {
			return nil
		}
		if req.Height != app.upgrade.ActivationHeight {
			return errors.New("moe enablement activation height missed")
		}
		s.migrateMoeEnablement(*app.upgrade)
		return nil
	}
	if s.Version == 7 {
		if app.upgrade == nil || req.Height < app.upgrade.ActivationHeight {
			return nil
		}
		if req.Height != app.upgrade.ActivationHeight {
			return errors.New("audit sampling activation height missed")
		}
		s.migrateAuditSampling()
		if app.upgrade.AuditSampling != nil {
			s.Parameters.AuditSampling = *app.upgrade.AuditSampling
		}
		if app.upgrade.MoeEnabled != nil {
			s.Parameters.MoeEnabled = *app.upgrade.MoeEnabled
		}
		return nil
	}
	if s.Version == 6 {
		if app.upgrade == nil || req.Height < app.upgrade.ActivationHeight {
			return nil
		}
		if req.Height != app.upgrade.ActivationHeight {
			return errors.New("audit eligibility activation height missed")
		}
		s.migrateAuditEligibility(req.Height)
		return nil
	}
	if s.Version == 5 {
		if app.upgrade == nil || req.Height < app.upgrade.ActivationHeight {
			return nil
		}
		if req.Height != app.upgrade.ActivationHeight {
			return errors.New("liveness activation height missed")
		}
		s.migrateLiveness(req.Height)
		return nil
	}
	if s.Version != 4 {
		return nil
	}
	if app.upgrade == nil {
		return nil
	}
	u := app.upgrade
	if req.Height < u.ActivationHeight {
		return nil
	}
	if req.Height != u.ActivationHeight {
		return errors.New("membership activation height missed")
	}
	// Match the actual consensus set, not a possibly stale genesis document.
	updates := []abci.ValidatorUpdate{}
	for _, vote := range req.DecidedLastCommit.Votes {
		found := false
		for _, v := range u.Membership.Registry {
			if bytes.Equal(cmted.PubKey(v.PubKey).Address(), vote.Validator.Address) && vote.Validator.Power == EqualValidatorPower {
				updates = append(updates, abci.NewValidatorUpdate(cmted.PubKey(v.PubKey), v.Power))
				found = true
				break
			}
		}
		if !found {
			return errors.New("upgrade manifest differs from live consensus set")
		}
	}
	if err := u.Membership.matchGenesis(updates); err != nil {
		return err
	}
	raw, _ := json.Marshal(u.Membership)
	var m Membership
	_ = json.Unmarshal(raw, &m)
	s.Membership = &m
	s.migrateLiveness(req.Height)
	return nil
}

// SortedValidatorIDs provides stable operator output without map iteration order.
func SortedValidatorIDs(m *Membership) []string {
	ids := []string{}
	for id := range m.Registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
