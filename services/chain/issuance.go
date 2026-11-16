package chain

// Genesis issuance uses integer subunits throughout. No runtime operation can
// mint or replenish the pool; recycling restores only the original channel.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"time"
)

const PoolEquivalents int64 = 1714286
const PoolAccount = "ember:genesis-subsidy-pool"
const SubsidyDisclosure = "Subsidies are consumption vouchers, usable only for inference fees; they cannot be transferred, staked, or used as deposits."
const subsidyWeek int64 = 7 * 86400

// StandardPrice is the published 512-input/256-output reference request price
// in subunits (one Spark = 1,000,000 subunits). PriceRef identifies that table.
// OrganicForecast is the published conservative first-year request forecast.
type IssuanceRules struct {
	PoolEquivalents    int64            `json:"pool_equivalents"`
	ChannelEquivalents map[string]int64 `json:"channel_equivalents"`
	DailyBPS           map[string]int64 `json:"daily_basis_points"`
	DailyTotalBPS      int64            `json:"daily_total_basis_points"`
	SeedAccounts       int64            `json:"seed_accounts"`
	SingleGrant        int64            `json:"single_grant_equivalents"`
	LossNumerator      int64            `json:"loss_numerator"`
	LossDenominator    int64            `json:"loss_denominator"`
	BWeights           [3]int64         `json:"b_supply_audit_consume_weights"`
	BCapMultiplier     int64            `json:"b_account_cap_multiplier"`
	Mark               string           `json:"subsidy_restriction"`
	ActivationSeconds  int64            `json:"activation_seconds"`
	MinimumInputTokens uint32           `json:"activation_minimum_input_tokens"`
	CThreshold         int              `json:"c_approval_threshold"`
	CGrantBPS          int64            `json:"c_single_grant_basis_points"`
	NoticeSeconds      int64            `json:"c_notice_seconds"`
}

func fixedIssuanceRules() *IssuanceRules {
	return &IssuanceRules{PoolEquivalents: PoolEquivalents, ChannelEquivalents: map[string]int64{"A": 1200000, "B": 342857, "C": 171429}, DailyBPS: map[string]int64{"A": 60, "B": 25, "C": 15}, DailyTotalBPS: 100, SeedAccounts: 8000, SingleGrant: 100, LossNumerator: 3, LossDenominator: 2, BWeights: [3]int64{6, 3, 1}, BCapMultiplier: 10, Mark: "permanent_consume_only", ActivationSeconds: subsidyWeek, MinimumInputTokens: 128, CThreshold: 4, CGrantBPS: 25, NoticeSeconds: subsidyWeek}
}

type IssuanceParameters struct {
	Rules             *IssuanceRules       `json:"fixed_rules"`
	StandardPrice     int64                `json:"standard_request_subunits"`
	PriceRef          string               `json:"price_table_ref"`
	OrganicForecast   int64                `json:"organic_forecast_equivalents"`
	ForecastRef       string               `json:"forecast_ref"`
	GenesisTime       int64                `json:"genesis_time_unix"`
	GovernorThreshold int                  `json:"governor_threshold"`
	GovernorSigners   []string             `json:"governor_signers"`
	FoundingEntities  []string             `json:"founding_entities"`
	Entities          map[string]string    `json:"registered_entities"`
	Retrospective     []ContributionRecord `json:"testnet_records"`
}

type ContributionRecord struct {
	RequestID string `json:"request_id"`
	Payer     string `json:"payer"`
	Executor  string `json:"executor"`
	Auditor   string `json:"auditor,omitempty"`
	Settled   bool   `json:"settled"`
	Audited   bool   `json:"audited"`
}
type RetrospectiveAward struct {
	Supply  int64 `json:"supply_points"`
	Audit   int64 `json:"audit_points"`
	Consume int64 `json:"consume_points"`
	Score   int64 `json:"weighted_score"`
	Amount  int64 `json:"amount"`
}
type SubsidyAction struct {
	Reason           string  `json:"reason,omitempty"`
	Op               string  `json:"op"`
	Confirm          bool    `json:"confirm_consume_only,omitempty"`
	Batch            string  `json:"batch_id,omitempty"`
	ID               string  `json:"id,omitempty"`
	To               string  `json:"to,omitempty"`
	Amount           int64   `json:"amount,omitempty"`
	Notice           string  `json:"public_notice,omitempty"`
	GovernanceAction *Action `json:"governance_action,omitempty"`
}
type IssuanceGrant struct {
	DeveloperID    string `json:"developer_proposal,omitempty"`
	Inventory      int64  `json:"undistributed_inventory,omitempty"`
	ID             string `json:"id"`
	Channel        string `json:"channel"`
	Batch          string `json:"batch_id"`
	Account        string `json:"account"`
	Amount         int64  `json:"amount"`
	IssuedAt       int64  `json:"issued_at"`
	ExpiresAt      int64  `json:"expires_at,omitempty"`
	ActivatedAt    int64  `json:"activated_at,omitempty"`
	ActivationHold string `json:"activation_hold,omitempty"`
	Recycled       int64  `json:"recycled"`
	Parent         string `json:"parent,omitempty"`
}
type DeveloperProposal struct {
	Funded           int64           `json:"funded"`
	ID               string          `json:"id"`
	Account          string          `json:"account"`
	Amount           int64           `json:"amount"`
	Notice           string          `json:"public_notice"`
	NoticeAt         int64           `json:"notice_at"`
	Approvals        map[string]bool `json:"approvals"`
	Issued           bool            `json:"issued"`
	Remaining        int64           `json:"undistributed"`
	ReconcileAt      int64           `json:"reconcile_at"`
	Revoked          bool            `json:"revoked"`
	RevocationNotice string          `json:"revocation_notice,omitempty"`
	RevokedAt        int64           `json:"revoked_at,omitempty"`
}
type IssuanceDay struct {
	Total    int64            `json:"total"`
	Channels map[string]int64 `json:"channels"`
}
type SettlementDay struct {
	Total    int64 `json:"total"`
	External int64 `json:"non_founding"`
}
type GovernorReminder struct {
	Trigger           string `json:"trigger"`
	At                int64  `json:"at"`
	Due               int64  `json:"due"`
	RequiredSigners   int    `json:"required_signers"`
	CrossOrganization int    `json:"cross_organization_signers"`
	Threshold         int    `json:"threshold"`
}
type SubsidyRecycle struct {
	GrantID string `json:"grant_id"`
	Channel string `json:"channel"`
	Amount  int64  `json:"amount"`
	At      int64  `json:"at"`
}
type IssuanceState struct {
	Recycles            []SubsidyRecycle              `json:"recycle_records"`
	Initial             int64                         `json:"initial"`
	Issued              int64                         `json:"issued"`
	Recycled            int64                         `json:"recycled"`
	Remaining           map[string]int64              `json:"remaining_by_channel"`
	Grants              []IssuanceGrant               `json:"grants"`
	Claims              map[string]string             `json:"lifetime_ab_claims"`
	Confirmed           map[string]bool               `json:"disclosure_confirmed"`
	Awards              map[string]RetrospectiveAward `json:"retrospective_awards"`
	Proposals           map[string]*DeveloperProposal `json:"developer_proposals"`
	Days                map[int64]*IssuanceDay        `json:"daily_issuance"`
	Settlements         map[int64]*SettlementDay      `json:"settlement_days"`
	Reminders           []GovernorReminder            `json:"governor_reminders"`
	GovernanceApprovals map[string]map[string]bool    `json:"governance_approvals"`
	GovernanceExecuted  map[string]bool               `json:"governance_executed"`
}

func scaled(a, b, d int64) int64 {
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return n.Quo(n, big.NewInt(d)).Int64()
}

// RecomputeRetrospective is shared by genesis validation and public tooling.
// One weighted point earns one standard-request equivalent before the 1000
// equivalent account cap and proportional budget reduction. Sorted accounts
// and flooring make any indivisible remainder stay in B, never another channel.
func RecomputeRetrospective(p *IssuanceParameters) (map[string]RetrospectiveAward, error) {
	if p == nil || p.StandardPrice <= 0 || p.StandardPrice > math.MaxInt64/PoolEquivalents {
		return nil, errors.New("invalid retrospective reference price")
	}
	out := map[string]RetrospectiveAward{}
	seen := map[string]bool{}
	for _, r := range p.Retrospective {
		if r.RequestID == "" || seen[r.RequestID] {
			return nil, errors.New("missing/duplicate retrospective request")
		}
		seen[r.RequestID] = true
		if !r.Settled {
			continue
		}
		if p.Entities[r.Payer] == "" || p.Entities[r.Executor] == "" || (r.Audited && p.Entities[r.Auditor] == "") {
			return nil, errors.New("unregistered retrospective entity")
		}
		if p.Entities[r.Payer] == p.Entities[r.Executor] {
			continue
		}
		e := out[r.Executor]
		e.Supply++
		out[r.Executor] = e
		c := out[r.Payer]
		c.Consume++
		out[r.Payer] = c
		if r.Audited {
			a := out[r.Auditor]
			a.Audit++
			out[r.Auditor] = a
		}
	}
	var total int64
	for id, a := range out {
		a.Score = 6*a.Supply + 3*a.Audit + a.Consume
		points := a.Score
		if points > 1000 {
			points = 1000
		}
		amount, err := multiply(points, p.StandardPrice)
		if err != nil {
			return nil, err
		}
		a.Amount = amount
		if err = add(&total, amount); err != nil {
			return nil, err
		}
		out[id] = a
	}
	budget, err := multiply(342857, p.StandardPrice)
	if err != nil {
		return nil, err
	}
	if total > budget {
		for id, a := range out {
			a.Amount = scaled(a.Amount, budget, total)
			out[id] = a
		}
	}
	return out, nil
}

func (s *State) initIssuance() error {
	p := s.Parameters.GenesisIssuance
	if p == nil {
		return nil
	}
	if p.Rules == nil {
		p.Rules = fixedIssuanceRules()
	} else if !reflect.DeepEqual(p.Rules, fixedIssuanceRules()) {
		return errors.New("fixed genesis numbers cannot be overridden")
	}
	if p.StandardPrice <= 0 || p.StandardPrice > math.MaxInt64/PoolEquivalents || p.PriceRef == "" || p.ForecastRef == "" || p.OrganicForecast < 2*PoolEquivalents || p.GenesisTime <= 0 || p.GovernorThreshold != 3 || len(p.GovernorSigners) != 5 || len(s.Governors) != 5 || s.Parameters.SubsidyCap != 0 || s.Accounts[PoolAccount] != nil {
		return errors.New("invalid fixed genesis issuance configuration")
	}
	seen := map[string]bool{}
	for _, id := range p.GovernorSigners {
		if id == "" || seen[id] || !s.Governors[id] {
			return errors.New("genesis requires five distinct governor signers")
		}
		seen[id] = true
	}
	if len(p.FoundingEntities) == 0 {
		return errors.New("founding entities required")
	}
	awards, err := RecomputeRetrospective(p)
	if err != nil {
		return err
	}
	s.Time = p.GenesisTime
	initial := PoolEquivalents * p.StandardPrice
	s.Accounts[PoolAccount] = &Account{Balance: initial}
	s.Issuance = &IssuanceState{Initial: initial, Remaining: map[string]int64{"A": 1200000 * p.StandardPrice, "B": 342857 * p.StandardPrice, "C": 171429 * p.StandardPrice}, Claims: map[string]string{}, Confirmed: map[string]bool{}, Awards: awards, Proposals: map[string]*DeveloperProposal{}, Days: map[int64]*IssuanceDay{}, Settlements: map[int64]*SettlementDay{}, GovernanceApprovals: map[string]map[string]bool{}, GovernanceExecuted: map[string]bool{}}
	return nil
}

// AdvanceTime runs even for empty blocks, before transactions, so expiry cannot
// depend on a claimant or on map iteration order. UTC days are non-fungible.
func (s *State) AdvanceTime(now int64) error {
	if s.Issuance == nil {
		return nil
	}
	if now < s.Time {
		return errors.New("consensus time regression")
	}
	s.Time = now
	for i := range s.Issuance.Grants {
		g := &s.Issuance.Grants[i]
		if g.ExpiresAt > 0 && g.ActivatedAt == 0 && g.Recycled == 0 && now >= g.ExpiresAt {
			s.recycle(g, g.Amount)
		}
	}
	ids := make([]string, 0, len(s.Issuance.Proposals))
	for id := range s.Issuance.Proposals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := s.Issuance.Proposals[id]
		if p.Issued && p.Remaining > 0 && now >= p.ReconcileAt {
			s.recycleDeveloper(p)
		}
	}
	s.updateGovernorReminders()
	return nil
}
func (s *State) recycle(g *IssuanceGrant, amount int64) {
	s.Issuance.Recycles = append(s.Issuance.Recycles, SubsidyRecycle{GrantID: g.ID, Channel: g.Channel, Amount: amount, At: s.Time})
	g.Recycled += amount
	s.Issuance.Recycled += amount
	s.Issuance.Remaining[g.Channel] += amount
	s.Accounts[PoolAccount].Balance += amount
}
func (s *State) recycleDeveloper(p *DeveloperProposal) {
	for n := range s.Issuance.Grants {
		g := &s.Issuance.Grants[n]
		if g.DeveloperID == p.ID && g.Inventory > 0 {
			s.recycle(g, g.Inventory)
			p.Remaining -= g.Inventory
			g.Inventory = 0
		}
	}
}
func (s *State) issue(channel, batch, id, to string, amount int64, pending bool) error {
	i := s.Issuance
	if batch == "" || id == "" || to == "" || amount <= 0 || !i.Confirmed[to] {
		return errors.New("grant requires recipient disclosure confirmation, batch, id and amount")
	}
	for _, g := range i.Grants {
		if g.ID == id {
			return errors.New("duplicate grant id")
		}
	}
	if amount > i.Remaining[channel] {
		return errors.New("channel pool exhausted")
	}
	d := i.Days[s.Time/86400]
	if d == nil {
		d = &IssuanceDay{Channels: map[string]int64{}}
		i.Days[s.Time/86400] = d
	}
	bps := map[string]int64{"A": 60, "B": 25, "C": 15}[channel]
	if amount > scaled(i.Initial, 100, 10000)-d.Total || amount > scaled(i.Initial, bps, 10000)-d.Channels[channel] {
		return errors.New("non-fungible daily issuance cap exceeded")
	}
	g := IssuanceGrant{ID: id, Channel: channel, Batch: batch, Account: to, Amount: amount, IssuedAt: s.Time}
	if pending {
		g.ExpiresAt = s.Time + subsidyWeek
	} else {
		g.ActivatedAt = s.Time
	}
	if s.Accounts[to] == nil {
		s.Accounts[to] = &Account{}
	}
	if !pending && channel != "C" {
		if err := add(&s.Accounts[to].Marked, amount); err != nil {
			return err
		}
	}
	if err := add(&i.Issued, amount); err != nil {
		return err
	}
	i.Grants = append(i.Grants, g)
	i.Remaining[channel] -= amount
	d.Total += amount
	d.Channels[channel] += amount
	s.Accounts[PoolAccount].Balance -= amount
	s.updateGovernorReminders()
	return nil
}

func (s *State) applySubsidy(signer string, a *SubsidyAction) error {
	if s.Issuance == nil || a == nil {
		return errors.New("genesis issuance unavailable")
	}
	i := s.Issuance
	p := s.Parameters.GenesisIssuance
	switch a.Op {
	case "confirm":
		if !a.Confirm {
			return errors.New(SubsidyDisclosure)
		}
		i.Confirmed[signer] = true
	case "claim":
		if !a.Confirm {
			return errors.New(SubsidyDisclosure)
		}
		if i.Claims[signer] != "" {
			return errors.New("lifetime A/B claim already used")
		}
		i.Confirmed[signer] = true
		amount := 100 * p.StandardPrice
		channel := "A"
		pending := true
		if b := i.Awards[signer].Amount; b > amount {
			amount = b
			channel = "B"
			pending = false
		}
		if err := s.issue(channel, a.Batch, a.ID, signer, amount, pending); err != nil {
			return err
		}
		i.Claims[signer] = channel
	case "propose_c":
		if !s.Governors[signer] || a.ID == "" || i.Proposals[a.ID] != nil || a.To == "" || a.Notice == "" || a.Amount <= 0 || a.Amount > scaled(i.Initial, 25, 10000) {
			return errors.New("invalid developer proposal or 0.25% grant cap exceeded")
		}
		for _, q := range i.Proposals {
			if q.Account == a.To && q.Revoked {
				return errors.New("developer permanently disqualified")
			}
		}
		i.Proposals[a.ID] = &DeveloperProposal{ID: a.ID, Account: a.To, Amount: a.Amount, Notice: a.Notice, NoticeAt: s.Time, Approvals: map[string]bool{}}
	case "approve_c":
		q := i.Proposals[a.ID]
		if !s.Governors[signer] || q == nil || q.Issued || q.Revoked {
			return errors.New("invalid developer approval")
		}
		q.Approvals[signer] = true
	case "issue_c":
		q := i.Proposals[a.ID]
		if q == nil || q.Funded == q.Amount || q.Revoked || len(q.Approvals) < 4 || s.Time < q.NoticeAt+subsidyWeek || (q.Issued && s.Time >= q.ReconcileAt) {
			return errors.New("developer grant requires four governors, seven days notice and an open inventory window")
		}
		amount := a.Amount
		if amount == 0 {
			amount = q.Amount - q.Funded
		}
		if amount <= 0 || amount > q.Amount-q.Funded {
			return errors.New("developer tranche exceeds approved grant")
		}
		id := q.ID
		if q.Issued {
			id = fmt.Sprintf("%s/tranche/%d", q.ID, len(i.Grants))
		}
		if err := s.issue("C", a.Batch, id, q.Account, amount, false); err != nil {
			return err
		}
		grant := &i.Grants[len(i.Grants)-1]
		grant.DeveloperID = q.ID
		grant.Inventory = amount
		if !q.Issued {
			q.ReconcileAt = time.Unix(s.Time, 0).UTC().AddDate(0, 3, 0).Unix()
		}
		q.Issued = true
		q.Remaining += amount
		q.Funded += amount
	case "distribute_c":
		q := i.Proposals[a.ID]
		if q == nil || !q.Issued || q.Revoked || signer != q.Account || a.Amount <= 0 || a.Amount > q.Remaining || !i.Confirmed[a.To] || a.Batch == "" {
			return errors.New("invalid developer distribution or missing recipient confirmation")
		}
		left := a.Amount
		count := len(i.Grants)
		for n := 0; n < count && left > 0; n++ {
			g := &i.Grants[n]
			if g.DeveloperID != q.ID || g.Inventory == 0 {
				continue
			}
			amount := g.Inventory
			if amount > left {
				amount = left
			}
			g.Inventory -= amount
			left -= amount
			parent := g.ID
			id := fmt.Sprintf("%s/user/%d", q.ID, len(i.Grants))
			i.Grants = append(i.Grants, IssuanceGrant{ID: id, Channel: "C", Batch: a.Batch, Account: a.To, Amount: amount, IssuedAt: s.Time, ExpiresAt: s.Time + subsidyWeek, Parent: parent})
		}
		if left != 0 {
			return errors.New("developer inventory mismatch")
		}
		q.Remaining -= a.Amount
	case "revoke_c":
		q := i.Proposals[a.ID]
		if q == nil || !s.Governors[signer] || a.Notice == "" {
			return errors.New("invalid developer revocation")
		}
		q.Revoked = true
		q.RevocationNotice = a.Notice
		q.RevokedAt = s.Time
		s.recycleDeveloper(q)
	case "register_entity":
		if !s.Governors[signer] || a.To == "" || a.Notice == "" || p.Entities[a.To] != "" {
			return errors.New("invalid entity registration")
		}
		p.Entities[a.To] = a.Notice
	case "approve_governance":
		if !s.Governors[signer] || a.GovernanceAction == nil || !governedAction(*a.GovernanceAction) {
			return errors.New("invalid governor approval")
		}
		key := governanceKey(*a.GovernanceAction)
		if i.GovernanceExecuted[key] {
			return errors.New("governance action already executed")
		}
		if i.GovernanceApprovals[key] == nil {
			i.GovernanceApprovals[key] = map[string]bool{}
		}
		i.GovernanceApprovals[key][signer] = true
	default:
		return errors.New("unknown subsidy operation")
	}
	return nil
}

// Marked escrow is available only through inference freezes. Every other
// debit (transfers, stake, publication bonds, privacy deposits) stays clean.
func (s *State) debitMarkedInference(id string, amount int64) (int64, error) {
	a := s.Accounts[id]
	if a == nil || amount <= 0 {
		return 0, errors.New("invalid inference debit")
	}
	marked := a.Marked
	if marked > amount {
		marked = amount
	}
	if a.Balance < amount-marked {
		return 0, errors.New("insufficient inference funds")
	}
	a.Marked -= marked
	a.Balance -= amount - marked
	return marked, nil
}
func (s *State) refundInference(h *Hold, amount int64) error {
	if amount < 0 || amount > h.Amount {
		return errors.New("invalid inference refund")
	}
	clean := h.Amount - h.Marked - h.SubsidyDebit
	restricted := amount - (h.Amount - h.SubsidyDebit)
	if restricted < 0 {
		restricted = 0
	}
	if err := add(&s.Accounts[h.Payer].ConsumptionCredit, restricted); err != nil {
		return err
	}
	h.SubsidyDebit -= restricted
	amount -= restricted

	marked := amount - clean
	if marked < 0 {
		marked = 0
	}
	h.Marked -= marked
	if err := add(&s.Accounts[h.Payer].Marked, marked); err != nil {
		return err
	}
	return s.creditBalance(h.Payer, amount-marked)
}
func (s *State) consumerRebate(h *Hold, amount int64) error {
	if h.SubsidyDebit > 0 {
		return add(&s.Accounts[h.Payer].ConsumptionCredit, amount)
	}

	// A payer's rebate is not executor/auditor work. Preserve the voucher mark
	// up to the subsidy-funded part to prevent self-conversion through rebates.
	marked := h.Marked
	if marked > amount {
		marked = amount
	}
	h.Marked -= marked
	if err := add(&s.Accounts[h.Payer].Marked, marked); err != nil {
		return err
	}
	return s.creditBalance(h.Payer, amount-marked)
}
func (s *State) subsidySettlement(h *Hold) {
	if s.Issuance == nil {
		return
	}
	p := s.Parameters.GenesisIssuance
	if h.Actual > 0 && h.AuditInputTokens >= 128 && h.Payer != h.Node && (p.Entities[h.Payer] == "" || p.Entities[h.Payer] != p.Entities[h.Node]) {
		for n := range s.Issuance.Grants {
			g := &s.Issuance.Grants[n]
			if g.Account == h.Payer && g.ExpiresAt > s.Time && g.ActivatedAt == 0 && g.Recycled == 0 {
				g.ActivatedAt = s.Time
				g.ActivationHold = h.ID
				s.Accounts[h.Payer].Marked += g.Amount
			}
		}
	}
	d := s.Issuance.Settlements[s.Time/86400]
	if d == nil {
		d = &SettlementDay{}
		s.Issuance.Settlements[s.Time/86400] = d
	}
	external := func(id string) bool {
		entity := p.Entities[id]
		if entity == "" {
			return false
		}
		for _, e := range p.FoundingEntities {
			if entity == e {
				return false
			}
		}
		return true
	}
	if h.Pipeline == nil {
		amount := h.Actual
		if h.Shares != nil {
			amount = h.Shares.Executor
		}
		d.Total += amount
		if external(h.Node) {
			d.External += amount
		}
	} else {
		for _, c := range h.Pipeline.Credits {
			d.Total += c.Amount
			if external(c.NodeID) {
				d.External += c.Amount
			}
		}
	}
	s.updateGovernorReminders()
}

type GovernorMetrics struct {
	TrailingExternalBPS int64              `json:"trailing_30_day_non_founding_basis_points"`
	IssuanceBPS         int64              `json:"cumulative_issuance_basis_points"`
	TrailingTotal       int64              `json:"trailing_30_day_executor_subunits"`
	TrailingExternal    int64              `json:"trailing_30_day_non_founding_subunits"`
	ConsecutiveDays     int                `json:"consecutive_days_at_20_percent"`
	Issued              int64              `json:"cumulative_issued"`
	Initial             int64              `json:"initial_pool"`
	Months              int                `json:"months_since_genesis"`
	Reminders           []GovernorReminder `json:"reminders"`
}

func (s *State) governorMetrics() GovernorMetrics {
	m := GovernorMetrics{}
	if s.Issuance == nil {
		return m
	}
	i := s.Issuance
	m.Issued = i.Issued
	m.Initial = i.Initial
	m.Reminders = i.Reminders
	day := s.Time / 86400
	for j := int64(1); j <= 30; j++ {
		d := i.Settlements[day-j]
		if d != nil {
			m.TrailingTotal += d.Total
			m.TrailingExternal += d.External
		}
	}
	for j := int64(1); j <= 30; j++ {
		d := i.Settlements[day-j]
		if d == nil || d.Total == 0 || scaled(d.External, 10000, d.Total) < 2000 {
			break
		}
		m.ConsecutiveDays++
	}
	if m.TrailingTotal > 0 {
		m.TrailingExternalBPS = scaled(m.TrailingExternal, 10000, m.TrailingTotal)
	}
	m.IssuanceBPS = scaled(m.Issued, 10000, m.Initial)
	start := time.Unix(s.Parameters.GenesisIssuance.GenesisTime, 0).UTC()
	now := time.Unix(s.Time, 0).UTC()
	m.Months = (now.Year()-start.Year())*12 + int(now.Month()-start.Month())
	if now.Before(start.AddDate(0, m.Months, 0)) {
		m.Months--
	}
	return m
}
func (s *State) updateGovernorReminders() {
	m := s.governorMetrics()
	triggers := []string{}
	if m.ConsecutiveDays >= 30 {
		triggers = append(triggers, "non_founding_30_days")
	}
	if m.Issued >= (m.Initial+1)/2 {
		triggers = append(triggers, "pool_50_percent")
	}
	if m.Months >= 12 {
		triggers = append(triggers, "twelve_months")
	}
	for _, trigger := range triggers {
		found := false
		for _, r := range s.Issuance.Reminders {
			if r.Trigger == trigger {
				found = true
			}
		}
		if !found {
			at := s.Time
			if trigger == "twelve_months" {
				at = time.Unix(s.Parameters.GenesisIssuance.GenesisTime, 0).UTC().AddDate(1, 0, 0).Unix()
			}
			s.Issuance.Reminders = append(s.Issuance.Reminders, GovernorReminder{Trigger: trigger, At: at, Due: at + 90*86400, RequiredSigners: 7, CrossOrganization: 2, Threshold: 4})
		}
	}
}
func governedAction(a Action) bool {
	return a.Type == "freeze_model_registration" || a.Type == "set_audit_sampling" || a.Type == "set_pricing" || a.Type == "set_supply_demand" || a.Type == "set_audit_comp" || a.Type == "set_audit_work" || a.Type == "set_misjudge" || (a.Type == "privacy" && a.Privacy != nil && (a.Privacy.Op == "epoch" || a.Privacy.Op == "end_epoch")) || (a.Type == "subsidy" && a.Subsidy != nil && (a.Subsidy.Op == "revoke_c" || a.Subsidy.Op == "register_entity"))
}
func governanceKey(a Action) string {
	raw, _ := json.Marshal(a)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func (s *State) authorizeGovernor(signer string, a Action) error {
	if s.Issuance == nil || !governedAction(a) {
		return nil
	}
	key := governanceKey(a)
	if !s.Governors[signer] || len(s.Issuance.GovernanceApprovals[key]) < 3 || s.Issuance.GovernanceExecuted[key] {
		return errors.New("governance requires three of five signers")
	}
	// Upgrades out of refund_only require the separately specified calibration
	// and appeal evidence. Until that evidence workflow exists, fail closed.
	if a.Type == "set_misjudge" && a.Misjudge != nil && a.Misjudge.SlashMode != s.Parameters.Misjudge.SlashMode {
		return errors.New("slash mode upgrade evidence not established")
	}
	s.Issuance.GovernanceExecuted[key] = true
	return nil
}

// Monthly summaries are derived from immutable grant/recycle timestamps. Child
// distributions do not issue twice; their expirations do count as recycling.
type SubsidyMonth struct {
	Month    string           `json:"month"`
	Opening  int64            `json:"opening"`
	Issued   map[string]int64 `json:"issued_by_channel"`
	Recycled int64            `json:"recycled"`
	Closing  int64            `json:"closing"`
}

func (s *State) subsidyMonthly() []SubsidyMonth {
	if s.Issuance == nil {
		return nil
	}
	i := s.Issuance
	by := map[string]*SubsidyMonth{}
	month := func(at int64) *SubsidyMonth {
		key := time.Unix(at, 0).UTC().Format("2006-01")
		if by[key] == nil {
			by[key] = &SubsidyMonth{Month: key, Issued: map[string]int64{"A": 0, "B": 0, "C": 0}}
		}
		return by[key]
	}
	month(s.Parameters.GenesisIssuance.GenesisTime)
	month(s.Time)
	for _, g := range i.Grants {
		if g.Parent == "" {
			month(g.IssuedAt).Issued[g.Channel] += g.Amount
		}
	}
	for _, r := range i.Recycles {
		month(r.At).Recycled += r.Amount
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	balance := i.Initial
	out := []SubsidyMonth{}
	for _, k := range keys {
		m := by[k]
		m.Opening = balance
		for _, v := range m.Issued {
			balance -= v
		}
		balance += m.Recycled
		m.Closing = balance
		out = append(out, *m)
	}
	return out
}
