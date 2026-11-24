package chain

import (
	"errors"
	"math/big"
)

// Genesis decision 2026-11-12. These are consensus constants, never governance
// parameters. The equivalents-to-subunits conversion is NOT defined here: it
// is generated from the calibrated genesis price table (genesis_prices.json,
// see genesis_prices_gen.go) since the 2026-11-15 price calibration replaced
// the 1/1/0 testnet placeholder. Do not reintroduce price-derived constants
// by hand; edit the price table and re-run gen_prices.py.
const (
	SubsidyPoolAccount                 = "ember/genesis-consumption-pool"
	SubsidyTotalEquivalents     int64  = 1714286
	SubsidyAEquivalents         int64  = 1200000
	SubsidyBEquivalents         int64  = 342857
	SubsidyCEquivalents         int64  = 171429
	SubsidyHouseholdEquivalents int64  = 100
	SubsidyBCapMultiplier       int64  = 10
	SubsidySupplyWeight         int64  = 6
	SubsidyAuditWeight          int64  = 3
	SubsidyConsumeWeight        int64  = 1
	SubsidyGlobalDailyBPS       int64  = 100
	SubsidyADailyBPS            int64  = 60
	SubsidyBDailyBPS            int64  = 25
	SubsidyCDailyBPS            int64  = 15
	SubsidyCGrantBPS            int64  = 25
	SubsidyCApprovals                  = 4
	SubsidyGovernors                   = 5
	SubsidyActivationInput      uint32 = 128
	SubsidyDaySeconds           int64  = 86400
	SubsidyActivationSeconds           = 7 * SubsidyDaySeconds
	SubsidyNotice                      = "The subsidy is a consumption voucher. It can only be used to pay inference fees and cannot be transferred to anyone else."
)

// The conservative forecast is supplied once in genesis, not revised at runtime.
// It is published alongside the immutable conversion and scaled channel budgets.
type GenesisSubsidy struct {
	TestnetRecords         []TestnetContribution `json:"testnet_records,omitempty"`
	Entities               map[string]string     `json:"registered_entities,omitempty"`
	OrganicYearEquivalents int64                 `json:"organic_year_equivalents"`
}
type SubsidyChannel struct {
	Initial   int64 `json:"initial"`
	Balance   int64 `json:"balance"`
	Issued    int64 `json:"issued"`
	Recovered int64 `json:"recovered"`
}
type SubsidyEvent struct {
	Channel  string `json:"channel"`
	Account  string `json:"account"`
	Batch    string `json:"batch"`
	Amount   int64  `json:"amount"`
	Time     int64  `json:"time"`
	Recovery bool   `json:"recovery"`
}
type SubsidyPool struct {
	Notice                 string                     `json:"notice"`
	Developers             map[string]*DeveloperGrant `json:"developer_grants"`
	DeveloperBanned        map[string]bool            `json:"developer_banned"`
	Daily                  map[int64]map[string]int64 `json:"daily_issued"`
	Retrospective          *SubsidyRetrospective      `json:"retrospective"`
	Notices                map[string]bool            `json:"notices"`
	Grants                 map[string]*SubsidyGrant   `json:"grants"`
	Account                string                     `json:"account"`
	OrganicYearEquivalents int64                      `json:"organic_year_equivalents"`
	OriginalEquivalents    int64                      `json:"original_equivalents"`
	Total                  int64                      `json:"total_subunits"`
	EquivalentSubunits     int64                      `json:"equivalent_subunits"`
	GuardrailPassed        bool                       `json:"guardrail_passed"`
	Channels               map[string]*SubsidyChannel `json:"channels"`
	Events                 []SubsidyEvent             `json:"events"`
}

func ratio(n, a, b int64) int64 {
	x := new(big.Int).Mul(big.NewInt(n), big.NewInt(a))
	return x.Quo(x, big.NewInt(b)).Int64()
}
func (s *State) initGenesisSubsidy(g *GenesisSubsidy) error {
	if g == nil {
		return nil
	}
	if g.OrganicYearEquivalents <= 0 || s.Accounts[SubsidyPoolAccount] != nil || s.Parameters.SubsidyCap != 0 {
		return errors.New("invalid genesis subsidy forecast, pool account or legacy mint")
	}
	capEquiv := g.OrganicYearEquivalents / 2
	if capEquiv > SubsidyTotalEquivalents {
		capEquiv = SubsidyTotalEquivalents
	}
	total, err := multiply(capEquiv, SubsidyEquivalentSubunits)
	if err != nil {
		return err
	}
	p := &SubsidyPool{Account: SubsidyPoolAccount, OrganicYearEquivalents: g.OrganicYearEquivalents, OriginalEquivalents: SubsidyTotalEquivalents, Total: total, EquivalentSubunits: SubsidyEquivalentSubunits, GuardrailPassed: true, Channels: map[string]*SubsidyChannel{}}
	// Floors leave any indivisible residue unissued, never increasing a channel.
	for ch, equiv := range map[string]int64{"A": SubsidyAEquivalents, "B": SubsidyBEquivalents, "C": SubsidyCEquivalents} {
		budget := ratio(total, equiv, SubsidyTotalEquivalents)
		p.Channels[ch] = &SubsidyChannel{Initial: budget, Balance: budget}
	}
	p.Notice = SubsidyNotice
	p.Notices = map[string]bool{}
	p.Developers = map[string]*DeveloperGrant{}
	p.DeveloperBanned = map[string]bool{}
	p.Daily = map[int64]map[string]int64{}
	p.Grants = map[string]*SubsidyGrant{}
	s.GenesisSubsidy = p
	s.Accounts[SubsidyPoolAccount] = &Account{Balance: total}
	return s.scoreRetrospective(g)
}

// Ordinary debits never inspect ConsumptionCredit: transfers, stake and bonds
// can only spend clean Spark. Only inference freezes call this function.
// Both voucher systems retain their restrictions. Consumption credits pay first,
// then marked issuance vouchers, then clean balance; neither can fund transfers.
func (s *State) debitInference(id string, amount int64) error {
	_, err := s.debitAllInference(id, amount)
	return err
}
func (s *State) debitAllInference(id string, amount int64) (int64, error) {
	a := s.Accounts[id]
	if a == nil || amount <= 0 {
		return 0, errors.New("invalid inference debit")
	}
	use := a.ConsumptionCredit
	if use > amount {
		use = amount
	}
	marked := int64(0)
	if amount > use {
		var err error
		marked, err = s.debitMarkedInference(id, amount-use)
		if err != nil {
			return 0, err
		}
	}
	a.ConsumptionCredit -= use
	s.inferenceDebit = use
	return marked, nil
}
func (s *State) takeInferenceDebit() int64                   { n := s.inferenceDebit; s.inferenceDebit = 0; return n }
func (s *State) rebateInference(h *Hold, amount int64) error { return s.consumerRebate(h, amount) }

type SubsidyGrant struct {
	Account   string `json:"account,omitempty"`
	Channel   string `json:"channel"`
	Amount    int64  `json:"amount"`
	ClaimedAt int64  `json:"claimed_at"`
	Deadline  int64  `json:"deadline"`
	Activated bool   `json:"activated"`
	Reclaimed bool   `json:"reclaimed"`
}
