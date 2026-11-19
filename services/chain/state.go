package chain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/evidence"
	"google.golang.org/protobuf/proto"
)

const AppVersion uint64 = 11

type Parameters struct {
	Adjudication           AdjudicationParameters  `json:"adjudication"`
	AuditSampling          AuditSamplingParameters `json:"audit_sampling"`
	MoeEnabled             bool                    `json:"moe_enabled,omitempty"`
	GenesisIssuance        *IssuanceParameters     `json:"genesis_issuance,omitempty"`
	ProbationShareRedirect bool                    `json:"probation_share_redirect,omitempty"`
	SupplyDemand           PriceParameters         `json:"supply_demand"`
	AuditEquality          AuditEqualityParameters `json:"audit_equality"`
	Liveness               LivenessParameters      `json:"liveness"`
	AuditWork              AuditWorkParameters     `json:"audit_work"`
	Misjudge               MisjudgeParameters      `json:"misjudge"`
	Assignment             AssignmentParameters    `json:"assignment"`
	Evidence               evidence.Parameters     `json:"evidence"`
	SlashFloorBPS          uint32                  `json:"slash_floor_bps"`
	SlashRepeatWindow      int64                   `json:"slash_repeat_window_blocks"`
	Scheduling             *core.SchedulingPolicy  `json:"scheduling,omitempty"`
	Publishing             *PublishingParameters   `json:"publishing,omitempty"`
	RegisteredAuditors     bool                    `json:"registered_auditors,omitempty"`
	ChainID                string                  `json:"chain_id"`
	SubsidyCap             int64                   `json:"subsidy_cap"`
	UnbondingBlocks        int64                   `json:"unbonding_blocks"`
	SlashMultiplier        int64                   `json:"slash_multiplier"`
	DisputeBlocks          int64                   `json:"dispute_blocks"`
	MinNodeStake           int64                   `json:"min_node_stake"`
	HoldTimeout            int64                   `json:"hold_timeout_blocks"`
	RegistrationTTL        int64                   `json:"registration_ttl_blocks"`
}

type GenesisAccount struct {
	ID         string   `json:"id"`
	Balance    int64    `json:"balance"`
	Stake      int64    `json:"stake"`
	Nonce      uint64   `json:"nonce,omitempty"`
	Unbondings []Unbond `json:"unbondings,omitempty"`
}

type Model struct {
	VerifiedCompute    *VerifiedCompute     `json:"verified_compute,omitempty"`
	ComputeCorrections []ComputeCorrection  `json:"compute_corrections,omitempty"`
	Identity           *ModelIdentity       `json:"identity,omitempty"`
	RegistrationFrozen bool                 `json:"registration_frozen,omitempty"`
	WeightRoot         string               `json:"weight_root,omitempty"`
	AuditComp          *AuditCompParameters `json:"audit_comp,omitempty"`
	Manifest           []byte               `json:"signed_manifest,omitempty"`
	Genesis            []byte               `json:"signed_profile,omitempty"`
	Publication        *Publication         `json:"publication,omitempty"`
	TokenPrices        *core.TokenPrices    `json:"token_prices,omitempty"`
	Pricing            *RolePricing         `json:"pricing,omitempty"`
	ID                 string               `json:"id"`
	GenesisHash        string               `json:"genesis_hash"`
	ProfileHash        string               `json:"profile_hash"`
}

type GenesisState struct {
	GenesisSubsidy *GenesisSubsidy  `json:"genesis_subsidy,omitempty"`
	Membership     *Membership      `json:"membership,omitempty"`
	Parameters     Parameters       `json:"parameters"`
	Accounts       []GenesisAccount `json:"accounts"`
	Models         []Model          `json:"models"`
	Coordinators   []string         `json:"coordinators"`
	Auditors       []string         `json:"auditors"`
	Governors      []string         `json:"governors"`
}

type Account struct {
	ConsumptionCredit int64    `json:"consumption_credit,omitempty"`
	Marked            int64    `json:"marked_subunits,omitempty"`
	Balance           int64    `json:"balance"`
	Stake             int64    `json:"stake"`
	Nonce             uint64   `json:"nonce"`
	Unbondings        []Unbond `json:"unbondings,omitempty"`
}

type Unbond struct {
	Amount        int64 `json:"amount"`
	ReleaseHeight int64 `json:"release_height"`
}

type Hold struct {
	Adjudication         *AdjudicationCase           `json:"adjudication,omitempty"`
	IdempotencyKey       string                      `json:"idempotency_key,omitempty"`
	SamplingPolicy       *AuditSamplingSnapshot      `json:"sampling_policy,omitempty"`
	Sampling             *SamplingRecord             `json:"sampling,omitempty"`
	Unaudited            bool                        `json:"unaudited,omitempty"`
	AuditCompletedHeight int64                       `json:"audit_completed_height,omitempty"`
	AuditStatsEvents     []AuditStatsEvent           `json:"audit_stats_events,omitempty"`
	SubsidyDebit         int64                       `json:"subsidy_debit,omitempty"`
	Marked               int64                       `json:"marked_escrow_subunits,omitempty"`
	ShadowNode           string                      `json:"shadow_node,omitempty"`
	ShadowPaid           int64                       `json:"shadow_paid_subunits,omitempty"`
	ConsumerGray         *ConsumerGraySnapshot       `json:"consumer_gray,omitempty"`
	ContributionProfile  string                      `json:"contribution_profile,omitempty"`
	AuditAssignedHeight  int64                       `json:"audit_assigned_height,omitempty"`
	AuditReassignments   uint32                      `json:"audit_reassignments,omitempty"`
	EvidenceManifest     *pb.EvidenceManifest        `json:"evidence_manifest,omitempty"`
	PrivacyTier          string                      `json:"privacy_tier,omitempty"`
	AuditCompPolicy      *AuditCompParameters        `json:"audit_comp,omitempty"`
	AuditWorkPolicy      AuditWorkParameters         `json:"audit_work_policy"`
	AuditInputTokens     uint32                      `json:"audit_input_tokens"`
	AuditWorks           []AuditWork                 `json:"audit_works,omitempty"`
	AuditReceipts        []*pb.SettlementReceipt     `json:"audit_receipts,omitempty"`
	AuditEscrow          int64                       `json:"audit_escrow_subunits"`
	MisjudgeThresholds   []ThresholdPair             `json:"misjudge_thresholds,omitempty"`
	FirstGrade           *pb.OffenseGrade            `json:"first_grade,omitempty"`
	AuditOutputTokens    uint32                      `json:"audit_output_tokens"`
	RequestID            string                      `json:"request_id,omitempty"`
	FreezePayload        []byte                      `json:"freeze_payload,omitempty"`
	FreezeTransaction    []byte                      `json:"freeze_transaction,omitempty"`
	AssignmentPolicy     AssignmentParameters        `json:"assignment_policy"`
	Assignments          []AssignmentRecord          `json:"assignments,omitempty"`
	TokenFee             *FrozenFee                  `json:"token_fee,omitempty"`
	Slashes              []SlashRecord               `json:"slashes,omitempty"`
	ConsumerLatency      *ConsumerLatencyObservation `json:"consumer_latency,omitempty"`
	Pipeline             *PipelineHold               `json:"pipeline,omitempty"`
	Moe                  *MoeHold                    `json:"moe,omitempty"`
	ModelID              string                      `json:"model_id,omitempty"`
	ProducerProfile      []byte                      `json:"producer_profile,omitempty"`
	Shares               *SettlementShares           `json:"shares,omitempty"`
	ID                   string                      `json:"id"`
	Payer                string                      `json:"payer"`
	Node                 string                      `json:"node"`
	Amount               int64                       `json:"amount"`
	Actual               int64                       `json:"actual"`
	State                string                      `json:"state"`
	ReceiptRef           string                      `json:"receipt_ref,omitempty"`
	SettleHeight         int64                       `json:"settle_height,omitempty"`
	AuditVerdict         string                      `json:"audit_verdict,omitempty"`
	VerdictBytes         []byte                      `json:"verdict_bytes,omitempty"`
	AuditRef             string                      `json:"audit_ref,omitempty"`
	EvidenceRef          string                      `json:"evidence_ref,omitempty"`
	Auditor              string                      `json:"auditor,omitempty"`
	FreezeHeight         int64                       `json:"freeze_height"`
	ExpireHeight         int64                       `json:"expire_height"`
	SubmitHeight         int64                       `json:"submit_height,omitempty"`
	AuditProof           string                      `json:"audit_proof,omitempty"`
	AuditRound           uint32                      `json:"audit_round,omitempty"`
	FirstAuditor         string                      `json:"first_auditor,omitempty"`
	FirstVerdict         string                      `json:"first_verdict,omitempty"`
}

type NodeRegistration struct {
	PendingNoExec           bool              `json:"pending_no_exec,omitempty"`
	ExecExitEffectiveHeight int64             `json:"exec_exit_effective_height,omitempty"`
	NoAudit                 bool              `json:"no_audit,omitempty"`
	NoExec                  bool              `json:"no_exec,omitempty"`
	Caps                    *ContributionCaps `json:"caps,omitempty"`
	PendingCaps             *ContributionCaps `json:"pending_caps,omitempty"`
	CapsEffectiveHeight     int64             `json:"caps_effective_height,omitempty"`
	LastProofHeight         int64             `json:"last_proof_height,omitempty"`
	NodeID                  string            `json:"node_id"`
	Profile                 []byte            `json:"profile"`
	RegisteredAt            int64             `json:"registered_at"`
	ExpiresHeight           int64             `json:"expires_height"`
}

type WorkEvidence struct {
	HoldID    string `json:"hold_id"`
	Node      string `json:"node"`
	Reference string `json:"reference"`
	Claimed   bool   `json:"claimed"`
}

type State struct {
	Availability           map[string]*AvailabilityRecord `json:"availability,omitempty"`
	chunkSpoolRoot         string                         // local-only receipt spool; never consensus JSON
	history                *historyArchive                // transient archive view, never consensus JSON
	IdempotencyIndex       map[string]string              `json:"idempotency_index,omitempty"`
	NodeAuditStats         map[string]NodeAuditStats      `json:"node_audit_stats,omitempty"`
	AuditPool              map[string]map[int64]int64     `json:"audit_pool,omitempty"`
	AuditPoolDistributions []AuditPoolDistribution        `json:"audit_pool_distributions,omitempty"`
	inferenceDebit         int64
	GenesisSubsidy         *SubsidyPool                      `json:"genesis_subsidy,omitempty"`
	BlockTime              int64                             `json:"block_time,omitempty"`
	Issuance               *IssuanceState                    `json:"issuance,omitempty"`
	Time                   int64                             `json:"time_unix,omitempty"`
	ConsumerSamples        map[string]ConsumerSample         `json:"consumer_samples,omitempty"`
	Prices                 map[string]*ProfilePrice          `json:"prices,omitempty"`
	PriceEpochStart        int64                             `json:"price_epoch_start,omitempty"`
	PendingPriceParameters *PriceParameters                  `json:"pending_price_parameters,omitempty"`
	Membership             *Membership                       `json:"membership,omitempty"`
	Privacy                *PrivacyState                     `json:"privacy,omitempty"`
	AuditWorkCounts        map[string]uint64                 `json:"audit_work_counts,omitempty"`
	GrayObservations       map[string]uint64                 `json:"gray_observations,omitempty"`
	OffenseRecords         []OffenseRecord                   `json:"offense_records,omitempty"`
	BlockSlashes           map[string]*BlockSlash            `json:"block_slashes,omitempty"`
	BlockHashes            map[int64][]byte                  `json:"block_hashes,omitempty"`
	PairStreaks            map[string]PairStreak             `json:"pair_streaks,omitempty"`
	Offenses               map[string][]int64                `json:"confirmed_offenses,omitempty"`
	Latencies              map[string]*LatencyStats          `json:"latencies,omitempty"`
	finalized              *State                            // transient prior-finalized snapshot; never serialized
	legacyAppHash          []byte                            // transient; AppHash the pre-migration binary committed at legacyHeight; never serialized
	legacyHeight           int64                             // transient; boundary height at which legacyAppHash applies
	Version                uint64                            `json:"version"`
	Height                 int64                             `json:"height"`
	Parameters             Parameters                        `json:"parameters"`
	Accounts               map[string]*Account               `json:"accounts"`
	Holds                  map[string]*Hold                  `json:"holds"`
	Models                 map[string]Model                  `json:"models"`
	Coordinators           map[string]bool                   `json:"coordinators"`
	Auditors               map[string]bool                   `json:"auditors"`
	Governors              map[string]bool                   `json:"governors"`
	Evidence               map[string]*WorkEvidence          `json:"evidence"`
	Nodes                  map[string]*NodeRegistration      `json:"nodes"`
	Endorsements           map[string]map[string]Endorsement `json:"endorsements,omitempty"`
	PublisherLast          map[string]int64                  `json:"publisher_last,omitempty"`
	PublisherForfeited     int64                             `json:"publisher_forfeited,omitempty"`
	SubsidyIssued          int64                             `json:"subsidy_issued"`
	Entropy                []byte                            `json:"entropy,omitempty"`
}

type Action struct {
	SpotStepCount          uint64                      `json:"spot_step_count,omitempty"`
	CommitmentRoot         []byte                      `json:"commitment_root,omitempty"`
	Spool                  *SpoolAttestation           `json:"spool,omitempty"`
	ChunkRoot              []byte                      `json:"chunk_root,omitempty"`
	ChunkBytes             int                         `json:"chunk_bytes,omitempty"`
	ChunkTotal             int                         `json:"chunk_total,omitempty"`
	ChunkHashes            [][]byte                    `json:"chunk_hashes,omitempty"`
	ChunkIndex             int                         `json:"chunk_index,omitempty"`
	ReceiptChunk           []byte                      `json:"receipt_chunk,omitempty"`
	TensorShapes           []core.TensorShape          `json:"tensor_shapes,omitempty"`
	DeclaredTotalParams    uint64                      `json:"declared_total_params,omitempty"`
	DeclaredActiveParams   uint64                      `json:"declared_active_params,omitempty"`
	DeclaredFlopsPerToken  uint64                      `json:"declared_flops_per_token,omitempty"`
	Reason                 string                      `json:"reason,omitempty"`
	NoAudit                bool                        `json:"no_audit,omitempty"`
	NoExec                 bool                        `json:"no_exec,omitempty"`
	AdjudicationCommitment string                      `json:"adjudication_commitment,omitempty"`
	IdempotencyKey         string                      `json:"idempotency_key,omitempty"`
	AuditSampling          *AuditSamplingParameters    `json:"audit_sampling,omitempty"`
	Epoch                  int64                       `json:"epoch,omitempty"`
	Subsidy                *SubsidyAction              `json:"subsidy,omitempty"`
	ExpectedPrice          *PriceStamp                 `json:"expected_price,omitempty"`
	SupplyDemand           *PriceParameters            `json:"supply_demand,omitempty"`
	Validator              *Validator                  `json:"validator,omitempty"`
	SetChange              *ValidatorSetChange         `json:"set_change,omitempty"`
	Ready                  *ValidatorReady             `json:"ready,omitempty"`
	Caps                   *ContributionCaps           `json:"caps,omitempty"`
	Notice                 string                      `json:"notice,omitempty"`
	Tier                   string                      `json:"tier,omitempty"`
	Privacy                *PrivacyAction              `json:"privacy,omitempty"`
	AuditComp              *AuditCompParameters        `json:"audit_comp,omitempty"`
	AuditWork              *AuditWorkParameters        `json:"audit_work,omitempty"`
	AuditReceipts          []*pb.SettlementReceipt     `json:"audit_receipts,omitempty"`
	Misjudge               *MisjudgeParameters         `json:"misjudge,omitempty"`
	PricingCommitment      []byte                      `json:"pricing_commitment,omitempty"`
	ConsumerLatency        *ConsumerLatencyObservation `json:"consumer_latency,omitempty"`
	PipelineAuthorizations []PipelineAuthorization     `json:"pipeline_authorizations,omitempty"`
	MoeAuthorizations      []MoeAuthorization          `json:"moe_authorizations,omitempty"`
	MoeInputTokens         uint32                      `json:"moe_input_tokens,omitempty"`
	MoeMaxTokens           uint32                      `json:"moe_max_tokens,omitempty"`
	TotalLayers            uint32                      `json:"total_layers,omitempty"`
	Genesis                []byte                      `json:"genesis,omitempty"`
	Manifest               []byte                      `json:"manifest,omitempty"`
	Origin                 *Origin                     `json:"origin,omitempty"`
	OriginProof            *OriginProof                `json:"origin_proof,omitempty"`
	PreviousProfileHash    string                      `json:"previous_profile_hash,omitempty"`
	TokenPrices            *core.TokenPrices           `json:"token_prices,omitempty"`
	Pricing                *RolePricing                `json:"pricing,omitempty"`
	Type                   string                      `json:"type"`
	To                     string                      `json:"to,omitempty"`
	ShadowNode             string                      `json:"shadow_node,omitempty"`
	Amount                 int64                       `json:"amount,omitempty"`
	HoldID                 string                      `json:"hold_id,omitempty"`
	Payer                  string                      `json:"payer,omitempty"`
	Node                   string                      `json:"node,omitempty"`
	Actual                 int64                       `json:"actual,omitempty"`
	Receipt                []byte                      `json:"receipt,omitempty"`
	Challenge              []byte                      `json:"challenge,omitempty"`
	Verdict                []byte                      `json:"verdict,omitempty"`
	Outcome                string                      `json:"outcome,omitempty"`
	EvidenceRef            string                      `json:"evidence_ref,omitempty"`
	ModelID                string                      `json:"model_id,omitempty"`
	GenesisHash            string                      `json:"genesis_hash,omitempty"`
	ProfileHash            string                      `json:"profile_hash,omitempty"`
	Profile                []byte                      `json:"profile,omitempty"`
	ExpiresHeight          int64                       `json:"expires_height,omitempty"`
}

type Envelope struct {
	ChainID   string `json:"chain_id"`
	PublicKey []byte `json:"public_key"`
	Nonce     uint64 `json:"nonce"`
	Action    Action `json:"action"`
	Signature []byte `json:"signature"`
}

func NewState(gen GenesisState) (*State, error) {
	gen.Parameters.AuditSampling.defaults()
	if !gen.Parameters.AuditSampling.valid() {
		return nil, errors.New("invalid audit sampling policy")
	}
	if gen.Parameters.SupplyDemand == (PriceParameters{}) {
		gen.Parameters.SupplyDemand = DefaultPriceParameters()
	}
	if !gen.Parameters.SupplyDemand.valid() {
		return nil, errors.New("invalid supply demand parameters")
	}
	if gen.Parameters.AuditWork.EpochBlocks == 0 {
		gen.Parameters.AuditWork.EpochBlocks = 1000
	}
	if gen.Parameters.AuditWork.MaxPerExecutor == 0 {
		gen.Parameters.AuditWork.MaxPerExecutor = 64
	}
	if !gen.Parameters.AuditWork.valid() {
		return nil, errors.New("invalid audit work policy")
	}
	if gen.Parameters.Assignment.MBlocks == 0 {
		gen.Parameters.Assignment.MBlocks = 3
	}
	if gen.Parameters.Assignment.CooldownRequests == 0 {
		gen.Parameters.Assignment.CooldownRequests = 8
	}
	if gen.Parameters.Assignment.MBlocks < 1 || gen.Parameters.Assignment.MBlocks > 64 {
		return nil, errors.New("invalid assignment parameters")
	}

	if gen.Parameters.SlashFloorBPS == 0 {
		gen.Parameters.SlashFloorBPS = 100
	}
	if gen.Parameters.SlashRepeatWindow == 0 {
		gen.Parameters.SlashRepeatWindow = 1000
	}
	if gen.Parameters.SlashFloorBPS > 10000 || gen.Parameters.SlashRepeatWindow < 1 {
		return nil, errors.New("invalid confirmed-cheating slash policy")
	}
	if gen.Parameters.GenesisIssuance != nil {
		if gen.Parameters.Misjudge.SlashMode != "" && gen.Parameters.Misjudge.SlashMode != "refund_only" {
			return nil, errors.New("genesis issuance requires refund_only slash_mode")
		}
		gen.Parameters.Misjudge.SlashMode = "refund_only"
	}
	gen.Parameters.Misjudge.defaults()
	if !gen.Parameters.Misjudge.valid() {
		return nil, errors.New("invalid misjudge policy")
	}
	gen.Parameters.AuditEquality.defaults()
	if !gen.Parameters.AuditEquality.valid() {
		return nil, errors.New("invalid audit equality parameters")
	}
	gen.Parameters.Liveness.defaults()
	if !gen.Parameters.Liveness.valid() {
		return nil, errors.New("invalid liveness parameters")
	}
	if gen.Parameters.MinNodeStake == 0 {
		gen.Parameters.MinNodeStake = 1
	}
	if gen.Parameters.HoldTimeout == 0 {
		gen.Parameters.HoldTimeout = 20
	}
	if gen.Parameters.RegistrationTTL == 0 {
		gen.Parameters.RegistrationTTL = 100
	}
	if gen.Parameters.ChainID == "" || gen.Parameters.SubsidyCap < 0 || gen.Parameters.UnbondingBlocks < 1 ||
		gen.Parameters.SlashMultiplier != 20 || gen.Parameters.DisputeBlocks < 0 {
		return nil, errors.New("invalid genesis parameters")
	}
	if gen.Parameters.MinNodeStake < 1 || gen.Parameters.HoldTimeout < 2 || gen.Parameters.RegistrationTTL < 2 {
		return nil, errors.New("invalid S19 security parameters")
	}
	if err := gen.Parameters.Scheduling.Validate(); err != nil {
		return nil, err
	}
	if err := gen.Parameters.Publishing.validate(); err != nil {
		return nil, err
	}
	gen.Parameters.Adjudication.defaults()
	if !gen.Parameters.Adjudication.valid() {
		return nil, errors.New("invalid adjudication parameters")
	}
	s := &State{Version: AppVersion, Parameters: gen.Parameters, Accounts: map[string]*Account{}, Holds: map[string]*Hold{},
		Models: map[string]Model{}, Coordinators: boolSet(gen.Coordinators), Auditors: boolSet(gen.Auditors),
		Governors: boolSet(gen.Governors), Evidence: map[string]*WorkEvidence{}, Nodes: map[string]*NodeRegistration{}}
	for _, value := range gen.Accounts {
		if value.ID == "" || value.Balance < 0 || value.Stake < 0 || s.Accounts[value.ID] != nil {
			return nil, errors.New("invalid genesis account")
		}
		for _, unbonding := range value.Unbondings {
			if unbonding.Amount <= 0 || unbonding.ReleaseHeight <= 0 {
				return nil, errors.New("invalid genesis unbonding")
			}
		}
		s.Accounts[value.ID] = &Account{Balance: value.Balance, Stake: value.Stake,
			Nonce: value.Nonce, Unbondings: append([]Unbond(nil), value.Unbondings...)}
	}
	for _, model := range gen.Models {
		if err := validateModel(model); err != nil || s.Models[model.ID].ID != "" || (model.Pricing != nil && !s.Parameters.RegisteredAuditors) {
			return nil, errors.New("invalid genesis model")
		}
		if model.AuditComp != nil && !model.AuditComp.valid() {
			return nil, errors.New("invalid model audit compensation")
		}
		if err := validateModelArtifacts(model); err != nil {
			return nil, err
		}
		s.Models[model.ID] = model
		s.priceRecord(model.ID)
	}
	if gen.Membership != nil {
		if err := gen.Membership.validate(s); err != nil {
			return nil, err
		}
		s.Membership = gen.Membership
	}
	if err := s.initIssuance(); err != nil {
		return nil, err
	}
	if err := s.initGenesisSubsidy(gen.GenesisSubsidy); err != nil {
		return nil, err
	}
	return s, nil
}

func boolSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			out[value] = true
		}
	}
	return out
}

func (s *State) Clone() (*State, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out State
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	out.finalized = s.finalized
	out.history = s.history
	out.chunkSpoolRoot = s.chunkSpoolRoot
	return &out, nil
}

func (s *State) Bytes() ([]byte, error) { return json.Marshal(s) }

func (s *State) Hash() ([]byte, error) {
	raw, err := s.Bytes()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

// AppHash is the hash consensus must see for this state's committed
// height. A state produced by the v2 migration bridge still sits at the
// height whose AppHash the pre-migration binary committed and CometBFT
// pinned in its state store; the handshake asserts Info's hash against
// that pinned value, so at exactly that boundary height AppHash returns
// the pre-migration hash recorded by MigrateV2State. From the first
// block committed past the boundary on, it is the state's own Hash().
func (s *State) AppHash() ([]byte, error) {
	if len(s.legacyAppHash) > 0 && s.Height == s.legacyHeight {
		return append([]byte(nil), s.legacyAppHash...), nil
	}
	return s.Hash()
}

func AccountID(public ed25519.PublicKey) string { return core.NodeID(public) }

func signingBytes(env Envelope) ([]byte, error) {
	env.Signature = nil
	return json.Marshal(env)
}

func Sign(chainID string, key ed25519.PrivateKey, nonce uint64, action Action) ([]byte, error) {
	env := Envelope{ChainID: chainID, PublicKey: key.Public().(ed25519.PublicKey), Nonce: nonce, Action: action}
	raw, err := signingBytes(env)
	if err != nil {
		return nil, err
	}
	env.Signature = ed25519.Sign(key, append([]byte("ember/chain-tx/v1\x00"), raw...))
	return json.Marshal(env)
}

// DecodeAndVerify retains the legacy (pre-v12) envelope limit for callers without state.
func DecodeAndVerify(raw []byte) (Envelope, string, error) {
	return DecodeAndVerifyVersion(raw, 11)
}

// DecodeAndVerifyVersion verifies an envelope using the active chain version.
func DecodeAndVerifyVersion(raw []byte, version uint64) (Envelope, string, error) {
	limit := 1 << 20
	if version >= 12 {
		limit = 1 << 24
	}
	var env Envelope
	if len(raw) == 0 || len(raw) > limit {
		return env, "", errors.New("invalid transaction envelope")
	}
	var decodeErr error
	if version < 14 {
		// These keys were unknown and ignored before v14, including arbitrary JSON
		// values. Shadow them as raw JSON to preserve legacy signature normalization.
		type oldAction Action
		var legacy struct {
			ChainID   string `json:"chain_id"`
			PublicKey []byte `json:"public_key"`
			Nonce     uint64 `json:"nonce"`
			Action    struct {
				oldAction
				CommitmentRoot json.RawMessage `json:"commitment_root"`
				SpotStepCount  json.RawMessage `json:"spot_step_count"`
				Spool          json.RawMessage `json:"spool"`
			} `json:"action"`
			Signature []byte `json:"signature"`
		}
		decodeErr = json.Unmarshal(raw, &legacy)
		env = Envelope{ChainID: legacy.ChainID, PublicKey: legacy.PublicKey, Nonce: legacy.Nonce, Action: Action(legacy.Action.oldAction), Signature: legacy.Signature}
	} else {
		decodeErr = json.Unmarshal(raw, &env)
	}
	if len(raw) == 0 || len(raw) > limit || decodeErr != nil || len(env.PublicKey) != ed25519.PublicKeySize || len(env.Signature) != ed25519.SignatureSize {
		return env, "", errors.New("invalid transaction envelope")
	}
	signed, err := signingBytes(env)
	if err != nil || !ed25519.Verify(env.PublicKey, append([]byte("ember/chain-tx/v1\x00"), signed...), env.Signature) {
		return env, "", errors.New("invalid transaction signature")
	}
	return env, AccountID(env.PublicKey), nil
}

func (s *State) Apply(raw []byte, height int64) (string, error) {
	return s.applyWithCopy(raw, height, s.transactionCopy)
}

func (s *State) applyWithCopy(raw []byte, height int64, copyState func() (*State, error)) (string, error) {
	env, signer, err := DecodeAndVerifyVersion(raw, s.Version)
	if err != nil {
		return "", err
	}
	if env.ChainID != s.Parameters.ChainID {
		return "", errors.New("wrong chain id")
	}
	account := s.Accounts[signer]
	if account == nil {
		if (s.GenesisSubsidy == nil || env.Action.Type != "subsidy_notice" || env.Action.Notice != SubsidyNotice) && (s.Issuance == nil || env.Action.Type != "subsidy" || env.Action.Subsidy == nil || (env.Action.Subsidy.Op != "claim" && env.Action.Subsidy.Op != "confirm")) {
			return "", errors.New("unknown signer")
		}
		account = &Account{}
	}
	if isFreeze(env.Action) && env.Action.IdempotencyKey != "" {
		key, err := hex.DecodeString(env.Action.IdempotencyKey)
		if err != nil || len(key) != sha256.Size {
			return "", errors.New("invalid idempotency key: require lowercase SHA256 hex")
		}
		if hex.EncodeToString(key) != env.Action.IdempotencyKey {
			return "", errors.New("invalid idempotency key: require lowercase SHA256 hex")
		}
		if id, ok := s.IdempotencyIndex[env.Action.IdempotencyKey]; ok {
			h := s.LookupHold(id)
			if h == nil || signer != h.Payer {
				return "", errors.New("idempotency key reused with different payload")
			}
			equal, err := freezeEconomicallyEqual(h.FreezePayload, env.Action)
			if err != nil || !equal {
				return "", errors.New("idempotency key reused with different payload")
			}
			sum := sha256.Sum256(h.FreezeTransaction)
			return hex.EncodeToString(sum[:]), nil
		}
	}
	if isFreeze(env.Action) {
		if h := s.LookupHold(env.Action.HoldID); h != nil {
			if signer != h.Payer || !bytes.Equal(h.FreezePayload, freezePayload(env.Action)) {
				return "", errors.New("first-data finality: changed request payload")
			}
			if bytes.Equal(h.FreezeTransaction, raw) {
				sum := sha256.Sum256(raw)
				return hex.EncodeToString(sum[:]), nil
			}
		}
	}
	if env.Nonce != account.Nonce+1 {
		return "", fmt.Errorf("invalid nonce: got %d want %d", env.Nonce, account.Nonce+1)
	}
	if height < s.Height {
		return "", errors.New("height regression")
	}
	trial, err := copyState()
	if err != nil {
		return "", err
	}
	trial.restoreReferencedHold(env.Action)
	if trial.Accounts[signer] == nil {
		trial.Accounts[signer] = &Account{}
	}
	if trial.finalized == nil {
		trial.finalized = s
	}
	if isFreeze(env.Action) && trial.Holds[env.Action.HoldID] != nil {
		// Identical payload with a fresh envelope nonce is also an idempotent retry.
	} else if err = trial.applyAction(signer, env.Action, height); err != nil {
		return "", err
	}
	if isFreeze(env.Action) && len(trial.Holds[env.Action.HoldID].FreezeTransaction) == 0 {
		trial.pinRequest(env, raw)
		if env.Action.IdempotencyKey != "" {
			if trial.IdempotencyIndex == nil {
				trial.IdempotencyIndex = map[string]string{}
			}
			trial.IdempotencyIndex[env.Action.IdempotencyKey] = env.Action.HoldID
			trial.Holds[env.Action.HoldID].IdempotencyKey = env.Action.IdempotencyKey
		}
		trial.freezeAuditComp(trial.Holds[env.Action.HoldID])
		trial.Holds[env.Action.HoldID].MisjudgeThresholds = append([]ThresholdPair(nil), trial.Parameters.Misjudge.Thresholds...)
	}
	if env.Action.Type == "privacy" && env.Action.Privacy != nil && env.Action.Privacy.Op == "request" {
		privacy := env.Action.Privacy
		frozen := env
		frozen.Action = Action{Type: "freeze", HoldID: privacy.RequestID, Payer: signer, Node: privacy.Executor, Amount: privacy.Amount, PricingCommitment: privacy.PricingCommitment, ExpectedPrice: privacy.ExpectedPrice}
		trial.pinRequest(frozen, raw)
		hold := trial.Holds[privacy.RequestID]
		trial.freezeAuditComp(hold)
		hold.MisjudgeThresholds = append([]ThresholdPair(nil), trial.Parameters.Misjudge.Thresholds...)
	}
	if env.Action.Type == "observe_latency" {
		trial.Holds[env.Action.HoldID].ConsumerLatency.SignedTransaction = append([]byte(nil), raw...)
	}
	trial.Accounts[signer].Nonce = env.Nonce
	trial.Height = height
	trial.finalized = s.finalized
	*s = *trial
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *State) applyAction(signer string, action Action, height int64) error {
	if handled, err := s.validatorAction(signer, action, height); handled {
		return err
	}
	if s.Privacy != nil && s.Privacy.Requests[action.HoldID] != nil {
		switch action.Type {
		case "submit", "submit_pipeline", "resolve_audit", "assign_audit", "reassign_audit", "audit", "settle", "cancel", "expire_hold", "register_consumer_appeal", "commit_third_adjudication":
			return errors.New("use privacy request transition")
		}
	}
	return s.applyActionInternal(signer, action, height)
}

func (s *State) applyActionInternal(signer string, action Action, height int64) error {
	if err := s.checkContributionAction(action, height); err != nil {
		return err
	}
	if action.Type == "freeze" || action.Type == "freeze_pipeline" || action.Type == "freeze_moe" {
		if action.Tier == "P1" {
			return errors.New("TIER_UNAVAILABLE")
		}
		if action.Tier != "" && action.Tier != "P0" {
			return errors.New("invalid tier")
		}
	}

	if err := s.authorizeGovernor(signer, action); err != nil {
		return err
	}
	switch action.Type {
	case "subsidy":
		return s.applySubsidy(signer, action.Subsidy)
	case "subsidy_c":
		return s.subsidyC(signer, action.Subsidy)
	case "subsidy_claim_b":
		return s.claimRetrospective(signer)
	case "subsidy_notice", "subsidy_claim_a":
		return s.subsidyA(signer, action)
	case "privacy":
		return s.applyPrivacy(signer, action.Privacy, height)
	case "freeze_model_registration":
		if !s.Governors[signer] || s.Models[action.ModelID].ID == "" {
			return errors.New("invalid registration freeze governance")
		}
		m := s.Models[action.ModelID]
		m.RegistrationFrozen = true
		s.Models[action.ModelID] = m
	case "correct_model_compute":
		return s.correctModelCompute(signer, action, height)
	case "set_pricing":
		if !s.Governors[signer] || action.Pricing == nil || !action.Pricing.valid() || s.Models[action.ModelID].ID == "" {
			return errors.New("invalid pricing governance")
		}
		m := s.Models[action.ModelID]
		if s.Version >= 11 && action.TokenPrices != nil {
			if _, err := core.EffectiveTokenFee(&pb.ChainGenesis{}, action.TokenPrices, 0, 0); err != nil {
				return err
			}
			cp := *action.TokenPrices
			m.TokenPrices = &cp
		}
		cp := *action.Pricing
		m.Pricing = &cp
		s.Models[action.ModelID] = m
	case "set_supply_demand":
		if !s.Governors[signer] || action.SupplyDemand == nil || !action.SupplyDemand.valid() {
			return errors.New("invalid supply demand governance")
		}
		// Apply only after the current complete epoch. Keep the current multiplier
		// inside both corridors so governance cannot bypass the step limit.
		for _, p := range s.Prices {
			if p.MultiplierPPM != PriceScale && p.CumulativeSamples < action.SupplyDemand.MinSettledSamples {
				return errors.New("protection change would bypass price step")
			}
			if p.MultiplierPPM < action.SupplyDemand.MinPPM || p.MultiplierPPM > action.SupplyDemand.MaxPPM {
				return errors.New("new corridor excludes active price")
			}
		}
		cp := *action.SupplyDemand
		s.PendingPriceParameters = &cp
	case "set_audit_comp":
		if !s.Governors[signer] || !action.AuditComp.valid() || s.Models[action.ModelID].ID == "" {
			return errors.New("invalid audit compensation governance")
		}
		m := s.Models[action.ModelID]
		cp := *action.AuditComp
		m.AuditComp = &cp
		s.Models[action.ModelID] = m
	case "set_audit_work":
		if !s.Governors[signer] || action.AuditWork == nil || !action.AuditWork.valid() {
			return errors.New("invalid audit work governance")
		}
		s.Parameters.AuditWork = *action.AuditWork
	case "set_misjudge":
		if !s.Governors[signer] || action.Misjudge == nil || !action.Misjudge.valid() {
			return errors.New("invalid misjudge governance update")
		}
		s.Parameters.Misjudge = *action.Misjudge
		return nil
	case "observe_latency":
		return s.observeConsumerLatency(signer, action)
	case "freeze_pipeline":
		return s.freezePipeline(signer, action, height)
	case "submit_pipeline":
		return s.submitPipeline(signer, action, height)
	case "freeze_moe":
		return s.freezeMoe(signer, action, height)
	case "submit_moe_manifest":
		return s.submitMoeManifest(signer, action, height)
	case "submit_moe_chunk":
		return s.submitMoeChunk(signer, action, height)
	case "submit_moe_finalize":
		return s.submitMoeFinalize(signer, action, height)
	case "submit_moe":
		return s.submitMoe(signer, action, height)
	case "transfer":
		if action.To == "" || action.To == signer || action.Amount <= 0 || (s.GenesisSubsidy != nil && action.To == SubsidyPoolAccount) {
			return errors.New("invalid transfer")
		}
		if err := s.debitBalance(signer, action.Amount); err != nil {
			return err
		}
		if err := s.creditBalance(action.To, action.Amount); err != nil {
			return err
		}
	case "stake":
		if action.Amount <= 0 {
			return errors.New("invalid stake")
		}
		if err := s.debitBalance(signer, action.Amount); err != nil {
			return err
		}
		return add(&s.Accounts[signer].Stake, action.Amount)
	case "prove_liveness":
		if s.Version < 6 {
			return errors.New("liveness upgrade not active")
		}
		r := s.Nodes[signer]
		if r == nil || action.Node != signer || r.ExpiresHeight <= height || height-r.LastProofHeight < s.Parameters.Liveness.ProofMinInterval {
			return errors.New("invalid or rate limited liveness proof")
		}
		r.LastProofHeight = height
	case "reassign_audit":
		if s.Version < 6 {
			return errors.New("liveness upgrade not active")
		}
		return s.reassignAudit(s.Holds[action.HoldID], height)
	case "request_exec_exit":
		return s.requestExecExit(signer, height)
	case "update_caps":
		return s.updateContributionCaps(signer, action.Caps, height)
	case "register_node":
		profile := &pb.ResourceProfile{}
		if proto.Unmarshal(action.Profile, profile) != nil || core.Verify(profile) != nil ||
			profile.NodeId != signer || core.NodeID(profile.Signer) != signer || s.Models[profile.ChainId].ID == "" ||
			action.ExpiresHeight <= height || action.ExpiresHeight > height+s.Parameters.RegistrationTTL ||
			s.Accounts[signer].Stake < s.Parameters.MinNodeStake {
			return errors.New("invalid or under-staked node registration")
		}
		if s.Models[profile.ChainId].RegistrationFrozen {
			return errors.New("model frozen for new registrations")
		}
		if err := s.validatePublishedNode(profile); err != nil {
			return err
		}
		if err := s.validateGPUBudget(profile); err != nil {
			return err
		}
		if err := s.validateMoeRegistration(profile); err != nil {
			return err
		}
		if prior := s.Nodes[signer]; prior != nil && height-prior.RegisteredAt < 2 {
			return errors.New("node registration rate limited")
		}
		if prior := s.Nodes[signer]; s.Parameters.RegisteredAuditors && prior != nil && prior.ExpiresHeight > height {
			old := &pb.ResourceProfile{}
			if (s.Version >= 10 && ((!prior.NoAudit && action.NoAudit) || (!prior.NoExec && action.NoExec))) || proto.Unmarshal(prior.Profile, old) != nil || action.ExpiresHeight < prior.ExpiresHeight || !registrationPreservesCoverage(old, profile) {
				return errors.New("active coverage is TTL-locked; wait for registration expiry before removing roles or changing runtime")
			}
		}
		if err := validateCaps(action.Caps); err != nil {
			return err
		}
		registration := &NodeRegistration{NoAudit: s.Version >= 10 && action.NoAudit, NoExec: s.Version >= 10 && action.NoExec, LastProofHeight: 0, NodeID: signer, Profile: append([]byte(nil), action.Profile...), RegisteredAt: height, ExpiresHeight: action.ExpiresHeight}
		if prior := s.Nodes[signer]; prior != nil {
			if s.Version >= 13 {
				registration.PendingNoExec, registration.ExecExitEffectiveHeight = prior.PendingNoExec, prior.ExecExitEffectiveHeight
			}
			registration.Caps = prior.CapsAt(height)
			registration.PendingCaps, registration.CapsEffectiveHeight = prior.PendingCaps, prior.CapsEffectiveHeight
		}
		if s.Version >= 6 {
			registration.LastProofHeight = height
		}
		s.Nodes[signer] = registration
		if action.Caps != nil {
			return s.updateContributionCaps(signer, action.Caps, height)
		}
	case "unbond":
		if err := s.checkExitWindows(signer, height); err != nil {
			return err
		}
		if r := s.Nodes[signer]; s.Parameters.RegisteredAuditors && r != nil && r.ExpiresHeight > height {
			return errors.New("active registration locks stake until TTL expiry")
		}
		if action.Amount <= 0 || action.Amount > s.Accounts[signer].Stake {
			return errors.New("invalid unbond")
		}
		if s.exposure(signer) > (s.Accounts[signer].Stake-action.Amount)/5 {
			return errors.New("stake remains exposed")
		}
		releaseHeight := height
		if err := add(&releaseHeight, s.Parameters.UnbondingBlocks); err != nil {
			return err
		}
		s.Accounts[signer].Stake -= action.Amount
		s.Accounts[signer].Unbondings = append(s.Accounts[signer].Unbondings, Unbond{Amount: action.Amount, ReleaseHeight: releaseHeight})
	case "claim_unbonded":
		if err := s.checkExitWindows(signer, height); err != nil {
			return err
		}
		var pending []Unbond
		var matured int64
		for _, item := range s.Accounts[signer].Unbondings {
			if item.ReleaseHeight <= height {
				if err := add(&matured, item.Amount); err != nil {
					return err
				}
			} else {
				pending = append(pending, item)
			}
		}
		if matured == 0 {
			return errors.New("no matured unbonding")
		}
		s.Accounts[signer].Unbondings = pending
		return add(&s.Accounts[signer].Balance, matured)
	case "freeze":
		if s.Version >= 10 && !s.ExecutionAllowed(action.Node, height) {
			return errors.New("executor registration unavailable or opted out")
		}
		if action.HoldID == "" || action.Payer != signer || action.Node == "" || action.Node == signer || action.Amount <= 0 || s.Holds[action.HoldID] != nil {
			return errors.New("invalid freeze")
		}
		node := s.Accounts[action.Node]
		registration := s.Nodes[action.Node]
		if node == nil || registration == nil || registration.ExpiresHeight <= height || node.Stake < s.Parameters.MinNodeStake || action.Amount > node.Stake/s.newExposureDivisor(action.Node)-s.exposure(action.Node) {
			return errors.New("stake exposure limit")
		}
		marked, err := s.debitAllInference(signer, action.Amount)
		if err != nil {
			return err
		}
		s.Holds[action.HoldID] = &Hold{Marked: marked, SubsidyDebit: s.takeInferenceDebit(), PrivacyTier: "P0", ID: action.HoldID, Payer: signer, Node: action.Node, Amount: action.Amount, State: "frozen", FreezeHeight: height, ExpireHeight: height + s.Parameters.HoldTimeout}
		s.freezeConsumerGray(s.Holds[action.HoldID], height)
		p := s.ActiveProfile(action.Node, height)
		if p != nil {
			if err := s.validatePublishedNode(p); err != nil {
				return err
			}
		}
		if p != nil {
			f, err := s.freezeFee(action, p.ChainId, action.Node)
			if err != nil {
				return err
			}
			s.Holds[action.HoldID].TokenFee = f
		}
		if p != nil && (s.Holds[action.HoldID].TokenFee != nil || s.Models[p.ChainId].Pricing != nil || s.Models[p.ChainId].AuditComp != nil || s.Models[p.ChainId].Publication != nil || len(s.Parameters.Misjudge.Thresholds) > 0) {
			s.Holds[action.HoldID].ModelID = p.ChainId
			s.Holds[action.HoldID].ProducerProfile = append([]byte(nil), registration.Profile...)
		}
	case "submit":
		hold := s.Holds[action.HoldID]
		if hold == nil || hold.State != "frozen" || height >= hold.ExpireHeight || action.Actual < 0 || action.Actual > hold.Amount {
			return errors.New("invalid receipt transition")
		}
		ref, node, err := verifyReceipt(action.Receipt, action.HoldID)
		if err != nil || node != hold.Node || (signer != node && !s.Coordinators[signer]) {
			return errors.New("invalid execution receipt")
		}
		r := &pb.ExecutionReceipt{}
		if proto.Unmarshal(action.Receipt, r) != nil {
			return errors.New("invalid receipt")
		}
		limit := s.Parameters.Evidence.FrameTokens()
		if r.EvidenceManifest != nil {
			if err := evidence.Validate(r.EvidenceManifest, r.OutputTokens, limit); err != nil {
				return err
			}
		} else if r.OutputTokens > limit {
			return errors.New("long evidence requires manifest")
		}
		if hold.TokenFee != nil && (core.NodeID(r.Signer) != hold.Node || (len(hold.TokenFee.Commitment) > 0 && len(r.CommitmentRef) != 32)) {
			return errors.New("receipt signer or commitment differs from frozen producer")
		}
		if uint64(r.ReasoningTokens)+uint64(r.ContentTokens) != 0 && uint64(r.ReasoningTokens)+uint64(r.ContentTokens) != uint64(r.OutputTokens) {
			return errors.New("reasoning/content token total mismatch")
		}
		if hold.TokenFee != nil && len(hold.TokenFee.Commitment) > 0 {
			c := &pb.ExecutionCommitment{}
			if proto.Unmarshal(hold.TokenFee.Commitment, c) != nil {
				return errors.New("invalid frozen commitment")
			}
			if c.SamplingParamsVersion == 2 && (uint64(r.ReasoningTokens)+uint64(r.ContentTokens) != uint64(r.OutputTokens) || r.ContentTokens > c.AnswerBudget || r.ReasoningTokens > c.ThinkingBudget || (!c.ThinkingEnabled && r.ReasoningTokens > 0)) {
				return errors.New("reasoning receipt violates committed budgets")
			}
		}
		if err := verifyTokenFee(hold.TokenFee, r.InputTokens, r.OutputTokens, action.Actual, r.CommitmentRef); err != nil {
			return err
		}
		if hold.TokenFee != nil {
			hold.TokenFee.OutputTokens = r.OutputTokens
		}
		if action.ShadowNode != "" {
			if action.ShadowNode == hold.Node || s.Nodes[action.ShadowNode] == nil {
				return errors.New("invalid shadow node")
			}
			hold.ShadowNode = action.ShadowNode
		}
		hold.AuditInputTokens = r.InputTokens
		hold.AuditOutputTokens = r.OutputTokens
		if r.EvidenceManifest != nil {
			hold.EvidenceManifest = proto.Clone(r.EvidenceManifest).(*pb.EvidenceManifest)
		}
		hold.Actual, hold.ReceiptRef, hold.State, hold.SettleHeight, hold.SubmitHeight = action.Actual, ref, "dispute", height+s.Parameters.DisputeBlocks, height
	case "register_consumer_appeal":
		return s.registerConsumerAppeal(s.Holds[action.HoldID], signer, height)
	case "commit_third_adjudication":
		h := s.Holds[action.HoldID]
		if h == nil {
			return errors.New("hold missing")
		}
		return s.commitThirdAdjudication(h, signer, action.AdjudicationCommitment)
	case "distribute_audit_pool":
		return s.distributeAuditPool(action.ModelID, action.Epoch, height)
	case "set_audit_sampling":
		if s.Version < 8 || !s.Governors[signer] || action.AuditSampling == nil || !action.AuditSampling.valid() {
			return errors.New("invalid audit sampling governance")
		}
		s.Parameters.AuditSampling = *action.AuditSampling
	case "spot_received", "spot_unopened", "spot_counterproof":
		if s.Version < 14 {
			return errors.New("unknown transaction type")
		}
		return s.spotAction(signer, action, height)
	case "resolve_audit", "assign_audit":
		hold := s.Holds[action.HoldID]
		if hold == nil || hold.State != "dispute" || hold.Auditor != "" || hold.AuditVerdict != "" || height <= hold.SubmitHeight {
			return errors.New("invalid audit assignment transition")
		}
		sampled, err := s.resolveSampling(hold)
		if err != nil {
			return err
		}
		if !sampled {
			return nil
		}
		auditor, proof, err := s.selectAuditor(hold, "", height)
		if err != nil {
			return err
		}
		if s.Version >= 6 {
			hold.AuditAssignedHeight = height
		}
		hold.Auditor, hold.AuditProof, hold.AuditRound = auditor, hex.EncodeToString(proof), 1
		if s.Version >= 14 && hold.Pipeline != nil {
			return s.startSpot(hold, height)
		}
	case "audit":
		hold := s.Holds[action.HoldID]
		if hold == nil || hold.State != "dispute" || hold.Auditor != signer || hold.AuditVerdict != "" {
			return errors.New("invalid audit transition")
		}
		if hold.Moe != nil && hold.Moe.ChunkTotal > 0 {
			if _, err := s.moeReceipt(hold); err != nil {
				return err
			}
		}
		if s.Version >= 14 && hold.Pipeline != nil && hold.Pipeline.Spot != nil {
			if err := s.validateSpotVerdict(hold, action.Verdict, height); err != nil {
				return err
			}
		}
		outcome, auditRef, evidenceRef, err := verifyAudit(action.Challenge, action.Verdict, hold.ReceiptRef, envPublicID(signer))
		if err != nil {
			return err
		}
		if outcome == "reject" && !hold.legacyV2() && !(s.Version >= 14 && hold.Pipeline != nil && hold.Pipeline.Spot != nil) {
			grade, gradeErr := s.validateGrade(hold, action.Verdict)
			if gradeErr != nil {
				return gradeErr
			}
			if hold.AuditRound == 1 || ((s.Parameters.AuditEquality.Enabled || s.adjudicationEnabled(hold)) && hold.FirstGrade == nil) {
				hold.FirstGrade = proto.Clone(grade).(*pb.OffenseGrade)
			} else if hold.FirstGrade == nil || grade.Class != hold.FirstGrade.Class || grade.Fault != hold.FirstGrade.Fault || grade.Band != hold.FirstGrade.Band || grade.LengthClass != hold.FirstGrade.LengthClass || grade.TokenizerVersion != hold.FirstGrade.TokenizerVersion || grade.RecomputedInputTokens != hold.FirstGrade.RecomputedInputTokens {
				return errors.New("confirming offense grades disagree")
			}
		}
		if hold.EvidenceManifest != nil && evidenceRef != hex.EncodeToString(hold.EvidenceManifest.MerkleRoot) {
			return errors.New("audit verdict evidence manifest mismatch")
		}
		if hold.Pipeline != nil && outcome == "reject" && !(s.Version >= 14 && hold.Pipeline.Spot != nil) {
			if err := validatePipelineFaults(hold, action.Verdict); err != nil {
				return err
			}
		}
		if hold.Moe != nil && outcome == "reject" {
			if err := s.validateMoeFaults(hold, action.Verdict); err != nil {
				return err
			}
		}
		challenge := &pb.AuditChallenge{}
		proof, decodeErr := hex.DecodeString(hold.AuditProof)
		if decodeErr != nil || proto.Unmarshal(action.Challenge, challenge) != nil || !bytes.Equal(challenge.VrfProof, proof) {
			return errors.New("audit selection proof mismatch")
		}
		if err := hold.citeAudit(action.Verdict, action.Challenge); err != nil {
			return err
		}
		hold.AuditRef, hold.EvidenceRef = auditRef, evidenceRef
		hold.VerdictBytes = append([]byte(nil), action.Verdict...)
		return s.recordAuditOutcome(hold, signer, outcome, evidenceRef, height)
	case "settle":
		hold := s.Holds[action.HoldID]
		if hold == nil || hold.State != "dispute" || (!hold.Unaudited && (hold.AuditVerdict == "" || action.Outcome != hold.AuditVerdict)) || (hold.Unaudited && action.Outcome != "accept") {
			return errors.New("invalid settlement transition")
		}
		if action.Outcome == "accept" && height < hold.SettleHeight {
			return errors.New("dispute window open")
		}
		if hold.TokenFee != nil && action.Actual != 0 && action.Actual != hold.Actual {
			return errors.New("settlement amount differs from verified fee")
		}
		if err := verifyFrozenSettlement(hold); err != nil {
			return err
		}
		paymentOutcome, err := s.consumerPaymentOutcome(hold, action.Outcome, height)
		if err != nil {
			return err
		}
		if err := verifyAuditReceipts(hold, paymentOutcome, action.AuditReceipts); err != nil {
			return err
		}
		if err := s.settleAt(hold, action.Outcome, height); err != nil {
			return err
		}
	case "expire_hold":
		hold := s.Holds[action.HoldID]
		if hold == nil || (hold.State != "frozen" && hold.State != "dispute") || height < hold.ExpireHeight {
			return errors.New("hold is not expired")
		}
		if err := s.refundInference(hold, hold.Amount); err != nil {
			return err
		}
		hold.State = "cancelled"
	case "cancel":
		hold := s.Holds[action.HoldID]
		if hold == nil || hold.State != "frozen" || (signer != hold.Payer && !s.Coordinators[signer]) {
			return errors.New("invalid cancellation")
		}
		if err := s.refundInference(hold, hold.Amount); err != nil {
			return err
		}
		hold.State = "cancelled"
	case "register_model":
		return s.publish(signer, action, height)
	case "endorse_profile", "revoke_endorsement":
		return s.endorse(signer, action, height)
	case "claim_publisher_bond":
		return s.claimPublisherBond(signer, action.ModelID, height)
	case "challenge_publication":
		return s.challengePublication(action, height)
	case "claim_subsidy":
		if s.GenesisSubsidy != nil {
			return errors.New("legacy mint disabled on genesis subsidy chains")
		}
		if s.Issuance != nil {
			return errors.New("legacy issuance disabled")
		}
		evidence := s.Evidence[action.EvidenceRef]
		if evidence == nil || evidence.Node != signer || evidence.Claimed || action.Amount <= 0 {
			return errors.New("invalid subsidy evidence")
		}
		if action.Amount > s.Parameters.SubsidyCap-s.SubsidyIssued {
			return errors.New("subsidy cap exceeded")
		}
		if err := add(&s.SubsidyIssued, action.Amount); err != nil {
			return err
		}
		if err := add(&s.Accounts[signer].Balance, action.Amount); err != nil {
			return err
		}
		evidence.Claimed = true
	default:
		return errors.New("unknown transaction type")
	}
	return nil
}

// Shared confirmation ladder for plaintext and encrypted audit verdicts.
func (s *State) recordAuditOutcome(hold *Hold, signer, outcome, evidenceRef string, at ...int64) error {
	defer func() {
		height := s.Height
		if len(at) > 0 {
			height = at[0]
		}
		s.auditCompleted(hold, height)
	}()
	if s.Version >= 14 && hold.Pipeline != nil && hold.Pipeline.Spot != nil {
		return s.spotOutcome(hold, signer, outcome, evidenceRef, at...)
	}
	if s.adjudicationEnabled(hold) {
		v := &pb.AuditVerdict{}
		_, _, binding, _ := verdictBindings(hold.VerdictBytes, v)
		// Old signed verdicts retain the old ladder and serialized hold shape.
		// Once a bound case starts, all later reviews stay in that case.
		if hold.Adjudication != nil || len(binding) > 0 {
			return s.recordAdjudicationOutcome(hold, signer, outcome, evidenceRef, at...)
		}
	}
	if s.Parameters.AuditEquality.Enabled {
		if outcome == "accept" {
			hold.AuditVerdict = "accept"
			s.Evidence[evidenceRef] = &WorkEvidence{HoldID: hold.ID, Node: hold.Node, Reference: evidenceRef}
			return nil
		}
		if outcome == "reject" && hold.FirstAuditor != "" {
			hold.AuditVerdict = "reject"
			return nil
		}
		if outcome == "reject" {
			hold.FirstAuditor, hold.FirstVerdict = signer, outcome
		}
		if (outcome != "reject" && outcome != "inconclusive") || int(hold.AuditRound) >= s.Parameters.AuditEquality.MaxRounds {
			hold.AuditVerdict = "service_failure"
			return nil
		}
		next, proof, err := s.selectAuditor(hold, signer, at...)
		if err != nil {
			hold.AuditVerdict = "service_failure"
			return nil
		}
		hold.Auditor, hold.AuditProof = next, hex.EncodeToString(proof)
		hold.AuditRound++
		if len(at) > 0 {
			hold.AuditAssignedHeight = at[0]
		}
		hold.AuditRef, hold.EvidenceRef, hold.VerdictBytes = "", "", nil
		return nil
	}
	if outcome == "accept" {
		hold.AuditVerdict = "accept"
		s.Evidence[evidenceRef] = &WorkEvidence{HoldID: hold.ID, Node: hold.Node, Reference: evidenceRef}
	} else if outcome == "reject" && hold.AuditRound == 1 {
		hold.FirstAuditor, hold.FirstVerdict = signer, outcome
		next, nextProof, selectErr := s.selectAuditor(hold, signer, at...)
		if selectErr != nil {
			hold.AuditVerdict = "service_failure"
		} else {
			if len(at) > 0 {
				hold.AuditAssignedHeight = at[0]
			}
			hold.Auditor, hold.AuditProof, hold.AuditRound = next, hex.EncodeToString(nextProof), 2
			hold.AuditRef, hold.EvidenceRef, hold.VerdictBytes = "", "", nil
		}
	} else if outcome == "reject" && hold.AuditRound == 2 {
		hold.AuditVerdict = "reject"
	} else {
		hold.AuditVerdict = "service_failure"
	}
	return nil
}

func (s *State) selectAuditor(hold *Hold, exclude string, at ...int64) (string, []byte, error) {
	if hold.Pipeline != nil {
		return s.selectPipelineAuditor(hold, exclude, at...)
	}
	if hold.Moe != nil {
		return s.selectMoeAuditor(hold, exclude, at...)
	}
	ids := make([]string, 0, len(s.Auditors))
	for id := range s.Auditors {
		if id != exclude && id != hold.Node && (!s.Parameters.RegisteredAuditors || s.EligibleAuditor(id, hold.Node, s.Height)) {
			ids = append(ids, id)
		}
	}
	if s.Parameters.RegisteredAuditors {
		ids = nil
		source := s.finalizedState()
		producer := source.ActiveProfile(hold.Node, source.Height)
		if len(hold.ProducerProfile) > 0 {
			producer = &pb.ResourceProfile{}
			if proto.Unmarshal(hold.ProducerProfile, producer) != nil {
				return "", nil, errors.New("invalid frozen producer")
			}
		}
		for id := range source.Nodes {
			if id != exclude && id != hold.Node && source.EligibleAuditorProfile(id, hold.Node, producer, source.Height) {
				ids = append(ids, id)
			}
		}
	}
	if s.Privacy != nil && s.Privacy.Requests[hold.ID] != nil {
		r := s.Privacy.Requests[hold.ID]
		eligible := ids[:0]
		for _, id := range ids {
			revoked := false
			for _, m := range r.Migrations {
				if m.Finalized && m.From == id {
					revoked = true
				}
			}
			if !revoked && len(s.Privacy.Operators[id].WrapKey) == 32 {
				eligible = append(eligible, id)
			}
		}
		ids = eligible
	}
	if hold.legacyV2() {
		return s.legacyAuditor(hold, ids)
	}
	return s.drawAuditor(hold, ids, at...)
}

// The signer identity was already established by the envelope. This helper
// keeps the audit verifier interface explicit without introducing key lookup.
func envPublicID(id string) string { return id }

func (s *State) settle(hold *Hold, outcome string) error {
	return s.settleAt(hold, outcome, s.Height)
}

func (s *State) settleAt(hold *Hold, outcome string, height int64) (err error) {
	defer func() {
		if err == nil {
			s.recordSettledStats(hold, height)
		}
	}()
	if c := hold.Adjudication; c != nil && c.Origin == "consumer_appeal" && c.DeliveryState == "settled" {
		// The original delivery was paid already: never refund its escrow twice.
		if outcome == "reject" {
			slash, err := s.recordOffense(hold, hold.Node, height)
			if err != nil {
				return err
			}
			if slash {
				if err = s.slashWithPenaltyCosts(hold, hold.Node, hold.Actual, height); err != nil {
					return err
				}
			}
			hold.State = "slashed"
		} else {
			hold.State = "settled"
		}
		return nil
	}
	if hold.Pipeline != nil {
		return s.settlePipeline(hold, outcome, height)
	}
	if hold.Moe != nil {
		return s.settleMoe(hold, outcome, height)
	}
	paymentOutcome, err := s.consumerPaymentOutcome(hold, outcome, height)
	if err != nil {
		return err
	}
	slash := false
	if outcome == "reject" && !hold.legacyUngraded() {
		hold.State = "cancelled"
		slash, err = s.recordOffense(hold, hold.Node, height)
		if err != nil {
			return err
		}
	}
	switch paymentOutcome {
	case "accept":
		if err := s.refundInference(hold, hold.Amount-hold.Actual); err != nil {
			return err
		}
		nodeAmount := hold.Actual
		if hold.AuditCompPolicy != nil && s.Models[hold.ModelID].Pricing == nil {
			parts := allocateFee(hold.Actual, [3]int64{80, 15, 5})
			hold.Shares = &SettlementShares{Fee: hold.Actual, Executor: parts[0], Auditor: parts[1], Consumer: parts[2]}
			nodeAmount = parts[0]
			if hold.Unaudited {
				if err := s.fundAuditPool(hold, parts[1], height); err != nil {
					return err
				}
			} else {
				hold.AuditEscrow = parts[1]
			}
			if err := s.consumerRebate(hold, parts[2]); err != nil {
				return err
			}
		}
		if policy := s.Models[hold.ModelID].Pricing; policy != nil {
			shares, err := s.priceShares(hold, policy)
			if err != nil {
				return err
			}
			hold.Shares = shares
			nodeAmount = shares.Executor
			auditPayment := shares.Auditor
			if hold.Unaudited {
				if err := s.fundAuditPool(hold, auditPayment, height); err != nil {
					return err
				}
				auditPayment = 0
			} else if hold.AuditCompPolicy != nil {
				auditPayment = 0
				hold.AuditEscrow = shares.Auditor
			}
			if err = s.creditBalance(hold.Auditor, auditPayment); err != nil {
				return err
			}
			if err = s.consumerRebate(hold, shares.Consumer); err != nil {
				return err
			}
		}
		if hold.AuditCompPolicy != nil {
			if hold.Shares == nil {
				return errors.New("audit compensation requires role-priced whole-model settlement")
			}
			paid := int64(0)
			for _, r := range hold.AuditReceipts {
				paid += r.AuditComp.CompSubunits
			}
			if paid > hold.AuditEscrow {
				return errors.New("audit allocation below payment")
			}
			hold.AuditEscrow -= paid
			if err := s.payAuditComp(hold); err != nil {
				return err
			}
		}
		// Probation redirects executor income only; the audit allocation has
		// already been paid/escrowed/pooled above and is never debited twice.
		shadowCut := int64(0)
		if s.Parameters.ProbationShareRedirect && hold.ShadowNode != "" && hold.ShadowNode != hold.Auditor &&
			s.settledWorkCount(hold.Node) < core.ProbationSettledThreshold {
			shadowCut = hold.Actual * 15 / 100
			if shadowCut > nodeAmount {
				shadowCut = nodeAmount
			}
			nodeAmount -= shadowCut
		}
		if err := s.creditBalance(hold.Node, nodeAmount); err != nil {
			return err
		}
		if err := s.recordPriceDemand(hold); err != nil {
			return err
		}
		if shadowCut > 0 {
			if err := s.creditBalance(hold.ShadowNode, shadowCut); err != nil {
				return err
			}
			hold.ShadowPaid = shadowCut
		}
		hold.State = "settled"
		s.subsidySettlement(hold)
		s.activateSubsidy(hold)
		if m := s.Models[hold.ModelID]; m.Publication != nil {
			m.Publication.SettledRequests++
		}
	case "reject", "service_failure":
		if outcome == "reject" && hold.legacyUngraded() {
			if err := s.refundInference(hold, hold.Amount); err != nil {
				return err
			}
			hold.State = "cancelled"
			return s.settleLegacyRejection(hold)
		}
		if err := s.refundInference(hold, hold.Amount); err != nil {
			return err
		}
		if outcome == "service_failure" {
			hold.State = "cancelled"
		}
		if slash {
			if err := s.slashWithPenaltyCosts(hold, hold.Node, hold.Actual, height); err != nil {
				return err
			}
			hold.State = "slashed"
		}
	default:
		return errors.New("invalid settlement outcome")
	}
	s.recordConsumerSample(hold, outcome, height)
	return nil
}

// settledWorkCount mirrors the gateway reputation rule's graduation
// counter over the same finalized records: settled whole-model holds
// plus settled pipeline credits, at most one credit per hold.
func (s *State) settledWorkCount(node string) int {
	count := 0
	for _, h := range s.historicalHolds() {
		if h.State != "settled" {
			continue
		}
		if h.Pipeline != nil {
			for _, c := range h.Pipeline.Credits {
				if c.NodeID == node {
					count++
					break
				}
			}
		} else if h.Node == node {
			count++
		}
	}
	return count
}

func (s *State) exposure(node string) int64 {
	var total int64
	for _, hold := range s.Holds {
		if hold.Pipeline != nil && (hold.State == "frozen" || hold.State == "dispute") {
			for _, a := range hold.Pipeline.Authorizations {
				if a.NodeID == node {
					total += a.Reserved
				}
			}
		}
		if hold.Moe != nil && (hold.State == "frozen" || hold.State == "dispute") {
			for _, a := range hold.Moe.Authorizations {
				if a.NodeID == node {
					total += a.Reserved
				}
			}
		}
		if hold.Node == node && (hold.State == "frozen" || hold.State == "dispute") {
			total += hold.Amount
		}
	}
	return total
}

func (s *State) debitBalance(id string, amount int64) error {
	account := s.Accounts[id]
	if account == nil || amount <= 0 || account.Balance < amount {
		return errors.New("insufficient funds or missing account")
	}
	account.Balance -= amount
	return nil
}

func (s *State) creditBalance(id string, amount int64) error {
	if s.Issuance != nil && id == PoolAccount {
		return errors.New("pool replenishment requires a governance process")
	}
	if amount < 0 {
		return errors.New("negative credit")
	}
	if s.Accounts[id] == nil {
		s.Accounts[id] = &Account{}
	}
	return add(&s.Accounts[id].Balance, amount)
}

func add(target *int64, amount int64) error {
	if amount < 0 || *target > math.MaxInt64-amount {
		return errors.New("integer overflow")
	}
	*target += amount
	return nil
}

func multiply(a, b int64) (int64, error) {
	if a < 0 || b < 0 || (a != 0 && b > math.MaxInt64/a) {
		return 0, errors.New("integer overflow")
	}
	return a * b, nil
}

func validateModel(model Model) error {
	if !model.Pricing.valid() || model.ID == "" || len(model.GenesisHash) != 64 || len(model.ProfileHash) != 64 {
		return errors.New("invalid model")
	}
	if _, err := hex.DecodeString(model.GenesisHash); err != nil {
		return err
	}
	_, err := hex.DecodeString(model.ProfileHash)
	return err
}

func verifyReceipt(raw []byte, holdID string) (string, string, error) {
	receipt := &pb.ExecutionReceipt{}
	if proto.Unmarshal(raw, receipt) != nil || receipt.RequestId != holdID || core.Verify(receipt) != nil || core.NodeID(receipt.Signer) != receipt.NodeId {
		return "", "", errors.New("invalid receipt")
	}
	chunks := make([]proto.Message, len(receipt.ChunkRoots))
	for i, chunk := range receipt.ChunkRoots {
		chunks[i] = chunk
	}
	if err := core.VerifyChunks(chunks, holdID, receipt.OutputTokens, func(m proto.Message) (string, uint32, uint32, []byte, []byte) {
		chunk := m.(*pb.ChunkRoot)
		return chunk.RequestId, chunk.Seq, chunk.UptoStep, chunk.PrevRoot, chunk.MerkleRoot
	}); err != nil {
		return "", "", err
	}
	ref, err := core.Digest(receipt)
	return hex.EncodeToString(ref), receipt.NodeId, err
}

func verifyAudit(challengeRaw, verdictRaw []byte, receiptRef, auditor string) (string, string, string, error) {
	challenge, verdict := &pb.AuditChallenge{}, &pb.AuditVerdict{}
	if proto.Unmarshal(challengeRaw, challenge) != nil || proto.Unmarshal(verdictRaw, verdict) != nil || core.Verify(challenge) != nil || core.Verify(verdict) != nil {
		return "", "", "", errors.New("invalid signed audit")
	}
	if core.NodeID(challenge.Signer) != auditor || core.NodeID(verdict.Signer) != auditor || (verdict.Verdict != "accept" && verdict.Verdict != "reject" && verdict.Verdict != "inconclusive") {
		return "", "", "", errors.New("invalid audit signer or verdict")
	}
	if hex.EncodeToString(challenge.ReceiptRef) != receiptRef {
		return "", "", "", errors.New("audit receipt reference mismatch")
	}
	challengeRef, err := core.Digest(challenge)
	if err != nil || !equal(challengeRef, verdict.ChallengeRef) || len(verdict.EvidenceRef) != sha256.Size {
		return "", "", "", errors.New("audit reference mismatch")
	}
	verdictRef, err := core.Digest(verdict)
	if err != nil {
		return "", "", "", err
	}
	return verdict.Verdict, hex.EncodeToString(verdictRef), hex.EncodeToString(verdict.EvidenceRef), nil
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var value byte
	for i := range a {
		value |= a[i] ^ b[i]
	}
	return value == 0
}

func SaveState(path string, state *State) error {
	state.SetChunkSpoolPath(path)
	raw, err := state.Bytes()
	if err != nil {
		return err
	}
	if err = persistArchive(path, state, raw); err != nil {
		return err
	}
	if state.Version >= 11 {
		if err := durableWrite(path, raw); err != nil {
			return err
		}
		state.cleanupChunkSpools()
		return nil
	}
	return saveStateBytes(path, raw)
}

func saveStateBytes(path string, raw []byte) error {
	var err error
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func LoadState(path string) (*State, error) {
	return LoadStateWithUpgrade(path, nil)
}

func LoadStateWithUpgrade(path string, upgrade *MembershipUpgrade) (*State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state.Version == 2 && upgrade != nil {
		return MigrateV2State(raw, *upgrade)
	}
	if state.Version != 14 && state.Version != 13 && state.Version != 12 && state.Version != 11 && state.Version != 10 && state.Version != 9 && state.Version != 8 && state.Version != 7 && state.Version != 6 && state.Version != 5 && state.Version != 4 {
		return nil, errors.New("unsupported state version")
	}
	if err = loadArchive(path, &state, raw); err != nil {
		return nil, err
	}
	state.SetChunkSpoolPath(path)
	return &state, nil
}

func SortedAccountIDs(state *State) []string {
	ids := make([]string, 0, len(state.Accounts))
	for id := range state.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
