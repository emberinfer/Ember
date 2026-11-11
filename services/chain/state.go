package chain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
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
	"google.golang.org/protobuf/proto"
)

const AppVersion uint64 = 3

type Parameters struct {
	SlashFloorBPS      uint32                 `json:"slash_floor_bps"`
	SlashRepeatWindow  int64                  `json:"slash_repeat_window_blocks"`
	Scheduling         *core.SchedulingPolicy `json:"scheduling,omitempty"`
	Publishing         *PublishingParameters  `json:"publishing,omitempty"`
	RegisteredAuditors bool                   `json:"registered_auditors,omitempty"`
	ChainID            string                 `json:"chain_id"`
	SubsidyCap         int64                  `json:"subsidy_cap"`
	UnbondingBlocks    int64                  `json:"unbonding_blocks"`
	SlashMultiplier    int64                  `json:"slash_multiplier"`
	DisputeBlocks      int64                  `json:"dispute_blocks"`
	MinNodeStake       int64                  `json:"min_node_stake"`
	HoldTimeout        int64                  `json:"hold_timeout_blocks"`
	RegistrationTTL    int64                  `json:"registration_ttl_blocks"`
}

type GenesisAccount struct {
	ID         string   `json:"id"`
	Balance    int64    `json:"balance"`
	Stake      int64    `json:"stake"`
	Nonce      uint64   `json:"nonce,omitempty"`
	Unbondings []Unbond `json:"unbondings,omitempty"`
}

type Model struct {
	Manifest    []byte       `json:"signed_manifest,omitempty"`
	Genesis     []byte       `json:"signed_profile,omitempty"`
	Publication *Publication `json:"publication,omitempty"`
	Pricing     *RolePricing `json:"pricing,omitempty"`
	ID          string       `json:"id"`
	GenesisHash string       `json:"genesis_hash"`
	ProfileHash string       `json:"profile_hash"`
}

type GenesisState struct {
	Parameters   Parameters       `json:"parameters"`
	Accounts     []GenesisAccount `json:"accounts"`
	Models       []Model          `json:"models"`
	Coordinators []string         `json:"coordinators"`
	Auditors     []string         `json:"auditors"`
	Governors    []string         `json:"governors"`
}

type Account struct {
	Balance    int64    `json:"balance"`
	Stake      int64    `json:"stake"`
	Nonce      uint64   `json:"nonce"`
	Unbondings []Unbond `json:"unbondings,omitempty"`
}

type Unbond struct {
	Amount        int64 `json:"amount"`
	ReleaseHeight int64 `json:"release_height"`
}

type Hold struct {
	TokenFee        *FrozenFee                  `json:"token_fee,omitempty"`
	Slashes         []SlashRecord               `json:"slashes,omitempty"`
	ConsumerLatency *ConsumerLatencyObservation `json:"consumer_latency,omitempty"`
	Pipeline        *PipelineHold               `json:"pipeline,omitempty"`
	ModelID         string                      `json:"model_id,omitempty"`
	ProducerProfile []byte                      `json:"producer_profile,omitempty"`
	Shares          *SettlementShares           `json:"shares,omitempty"`
	ID              string                      `json:"id"`
	Payer           string                      `json:"payer"`
	Node            string                      `json:"node"`
	Amount          int64                       `json:"amount"`
	Actual          int64                       `json:"actual"`
	State           string                      `json:"state"`
	ReceiptRef      string                      `json:"receipt_ref,omitempty"`
	SettleHeight    int64                       `json:"settle_height,omitempty"`
	AuditVerdict    string                      `json:"audit_verdict,omitempty"`
	VerdictBytes    []byte                      `json:"verdict_bytes,omitempty"`
	AuditRef        string                      `json:"audit_ref,omitempty"`
	EvidenceRef     string                      `json:"evidence_ref,omitempty"`
	Auditor         string                      `json:"auditor,omitempty"`
	FreezeHeight    int64                       `json:"freeze_height"`
	ExpireHeight    int64                       `json:"expire_height"`
	SubmitHeight    int64                       `json:"submit_height,omitempty"`
	AuditProof      string                      `json:"audit_proof,omitempty"`
	AuditRound      uint32                      `json:"audit_round,omitempty"`
	FirstAuditor    string                      `json:"first_auditor,omitempty"`
	FirstVerdict    string                      `json:"first_verdict,omitempty"`
}

type NodeRegistration struct {
	NodeID        string `json:"node_id"`
	Profile       []byte `json:"profile"`
	RegisteredAt  int64  `json:"registered_at"`
	ExpiresHeight int64  `json:"expires_height"`
}

type WorkEvidence struct {
	HoldID    string `json:"hold_id"`
	Node      string `json:"node"`
	Reference string `json:"reference"`
	Claimed   bool   `json:"claimed"`
}

type State struct {
	Offenses           map[string][]int64                `json:"confirmed_offenses,omitempty"`
	Latencies          map[string]*LatencyStats          `json:"latencies,omitempty"`
	finalized          *State                            // transient prior-finalized snapshot; never serialized
	Version            uint64                            `json:"version"`
	Height             int64                             `json:"height"`
	Parameters         Parameters                        `json:"parameters"`
	Accounts           map[string]*Account               `json:"accounts"`
	Holds              map[string]*Hold                  `json:"holds"`
	Models             map[string]Model                  `json:"models"`
	Coordinators       map[string]bool                   `json:"coordinators"`
	Auditors           map[string]bool                   `json:"auditors"`
	Governors          map[string]bool                   `json:"governors"`
	Evidence           map[string]*WorkEvidence          `json:"evidence"`
	Nodes              map[string]*NodeRegistration      `json:"nodes"`
	Endorsements       map[string]map[string]Endorsement `json:"endorsements,omitempty"`
	PublisherLast      map[string]int64                  `json:"publisher_last,omitempty"`
	PublisherForfeited int64                             `json:"publisher_forfeited,omitempty"`
	SubsidyIssued      int64                             `json:"subsidy_issued"`
	Entropy            []byte                            `json:"entropy,omitempty"`
}

type Action struct {
	PricingCommitment      []byte                      `json:"pricing_commitment,omitempty"`
	ConsumerLatency        *ConsumerLatencyObservation `json:"consumer_latency,omitempty"`
	PipelineAuthorizations []PipelineAuthorization     `json:"pipeline_authorizations,omitempty"`
	TotalLayers            uint32                      `json:"total_layers,omitempty"`
	Genesis                []byte                      `json:"genesis,omitempty"`
	Manifest               []byte                      `json:"manifest,omitempty"`
	Origin                 *Origin                     `json:"origin,omitempty"`
	OriginProof            *OriginProof                `json:"origin_proof,omitempty"`
	PreviousProfileHash    string                      `json:"previous_profile_hash,omitempty"`
	Pricing                *RolePricing                `json:"pricing,omitempty"`
	Type                   string                      `json:"type"`
	To                     string                      `json:"to,omitempty"`
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
	if gen.Parameters.SlashFloorBPS == 0 {
		gen.Parameters.SlashFloorBPS = 100
	}
	if gen.Parameters.SlashRepeatWindow == 0 {
		gen.Parameters.SlashRepeatWindow = 1000
	}
	if gen.Parameters.SlashFloorBPS > 10000 || gen.Parameters.SlashRepeatWindow < 1 {
		return nil, errors.New("invalid confirmed-cheating slash policy")
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
		return nil, errors.New("invalid CPU strict security parameters")
	}
	if err := gen.Parameters.Scheduling.Validate(); err != nil {
		return nil, err
	}
	if err := gen.Parameters.Publishing.validate(); err != nil {
		return nil, err
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
		if err := validateModelArtifacts(model); err != nil {
			return nil, err
		}
		s.Models[model.ID] = model
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

func DecodeAndVerify(raw []byte) (Envelope, string, error) {
	var env Envelope
	if len(raw) == 0 || len(raw) > 1<<20 || json.Unmarshal(raw, &env) != nil || len(env.PublicKey) != ed25519.PublicKeySize || len(env.Signature) != ed25519.SignatureSize {
		return env, "", errors.New("invalid transaction envelope")
	}
	signed, err := signingBytes(env)
	if err != nil || !ed25519.Verify(env.PublicKey, append([]byte("ember/chain-tx/v1\x00"), signed...), env.Signature) {
		return env, "", errors.New("invalid transaction signature")
	}
	return env, AccountID(env.PublicKey), nil
}

func (s *State) Apply(raw []byte, height int64) (string, error) {
	env, signer, err := DecodeAndVerify(raw)
	if err != nil {
		return "", err
	}
	if env.ChainID != s.Parameters.ChainID {
		return "", errors.New("wrong chain id")
	}
	account := s.Accounts[signer]
	if account == nil {
		return "", errors.New("unknown signer")
	}
	if env.Nonce != account.Nonce+1 {
		return "", fmt.Errorf("invalid nonce: got %d want %d", env.Nonce, account.Nonce+1)
	}
	if height < s.Height {
		return "", errors.New("height regression")
	}
	trial, err := s.Clone()
	if err != nil {
		return "", err
	}
	if trial.finalized == nil {
		trial.finalized = s
	}
	if err = trial.applyAction(signer, env.Action, height); err != nil {
		return "", err
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
	switch action.Type {
	case "observe_latency":
		return s.observeConsumerLatency(signer, action)
	case "freeze_pipeline":
		return s.freezePipeline(signer, action, height)
	case "submit_pipeline":
		return s.submitPipeline(signer, action, height)
	case "transfer":
		if action.To == "" || action.To == signer || action.Amount <= 0 {
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
	case "register_node":
		profile := &pb.ResourceProfile{}
		if proto.Unmarshal(action.Profile, profile) != nil || core.Verify(profile) != nil ||
			profile.NodeId != signer || core.NodeID(profile.Signer) != signer || s.Models[profile.ChainId].ID == "" ||
			action.ExpiresHeight <= height || action.ExpiresHeight > height+s.Parameters.RegistrationTTL ||
			s.Accounts[signer].Stake < s.Parameters.MinNodeStake {
			return errors.New("invalid or under-staked node registration")
		}
		if err := s.validatePublishedNode(profile); err != nil {
			return err
		}
		if prior := s.Nodes[signer]; prior != nil && height-prior.RegisteredAt < 2 {
			return errors.New("node registration rate limited")
		}
		if prior := s.Nodes[signer]; s.Parameters.RegisteredAuditors && prior != nil && prior.ExpiresHeight > height {
			old := &pb.ResourceProfile{}
			if proto.Unmarshal(prior.Profile, old) != nil || action.ExpiresHeight < prior.ExpiresHeight || !registrationPreservesCoverage(old, profile) {
				return errors.New("active coverage is TTL-locked; wait for registration expiry before removing roles or changing runtime")
			}
		}
		s.Nodes[signer] = &NodeRegistration{NodeID: signer, Profile: append([]byte(nil), action.Profile...), RegisteredAt: height, ExpiresHeight: action.ExpiresHeight}
	case "unbond":
		if r := s.Nodes[signer]; s.Parameters.RegisteredAuditors && r != nil && r.ExpiresHeight > height {
			return errors.New("active registration locks stake until TTL expiry")
		}
		if action.Amount <= 0 || action.Amount > s.Accounts[signer].Stake {
			return errors.New("invalid unbond")
		}
		if s.exposure(signer) > (s.Accounts[signer].Stake-action.Amount)/5 {
			return errors.New("stake remains exposed")
		}
		s.Accounts[signer].Stake -= action.Amount
		s.Accounts[signer].Unbondings = append(s.Accounts[signer].Unbondings, Unbond{Amount: action.Amount, ReleaseHeight: height + s.Parameters.UnbondingBlocks})
	case "claim_unbonded":
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
		if action.HoldID == "" || action.Payer != signer || action.Node == "" || action.Node == signer || action.Amount <= 0 || s.Holds[action.HoldID] != nil {
			return errors.New("invalid freeze")
		}
		node := s.Accounts[action.Node]
		registration := s.Nodes[action.Node]
		if node == nil || registration == nil || registration.ExpiresHeight <= height || node.Stake < s.Parameters.MinNodeStake || action.Amount > node.Stake/5-s.exposure(action.Node) {
			return errors.New("stake exposure limit")
		}
		if err := s.debitBalance(signer, action.Amount); err != nil {
			return err
		}
		s.Holds[action.HoldID] = &Hold{ID: action.HoldID, Payer: signer, Node: action.Node, Amount: action.Amount, State: "frozen", FreezeHeight: height, ExpireHeight: height + s.Parameters.HoldTimeout}
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
		if p != nil && (s.Holds[action.HoldID].TokenFee != nil || s.Models[p.ChainId].Pricing != nil || s.Models[p.ChainId].Publication != nil) {
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
		if hold.TokenFee != nil && (core.NodeID(r.Signer) != hold.Node || (len(hold.TokenFee.Commitment) > 0 && len(r.CommitmentRef) != 32)) {
			return errors.New("receipt signer or commitment differs from frozen producer")
		}
		if err := verifyTokenFee(hold.TokenFee, r.InputTokens, r.OutputTokens, action.Actual, r.CommitmentRef); err != nil {
			return err
		}
		if hold.TokenFee != nil {
			hold.TokenFee.OutputTokens = r.OutputTokens
		}
		hold.Actual, hold.ReceiptRef, hold.State, hold.SettleHeight, hold.SubmitHeight = action.Actual, ref, "dispute", height+s.Parameters.DisputeBlocks, height
	case "assign_audit":
		hold := s.Holds[action.HoldID]
		if hold == nil || hold.State != "dispute" || hold.Auditor != "" || hold.AuditVerdict != "" || height <= hold.SubmitHeight {
			return errors.New("invalid audit assignment transition")
		}
		auditor, proof, err := s.selectAuditor(hold, "")
		if err != nil {
			return err
		}
		hold.Auditor, hold.AuditProof, hold.AuditRound = auditor, hex.EncodeToString(proof), 1
	case "audit":
		hold := s.Holds[action.HoldID]
		if hold == nil || hold.State != "dispute" || hold.Auditor != signer || hold.AuditVerdict != "" {
			return errors.New("invalid audit transition")
		}
		outcome, auditRef, evidenceRef, err := verifyAudit(action.Challenge, action.Verdict, hold.ReceiptRef, envPublicID(signer))
		if err != nil {
			return err
		}
		if hold.Pipeline != nil && outcome == "reject" {
			if err := validatePipelineFaults(hold, action.Verdict); err != nil {
				return err
			}
		}
		challenge := &pb.AuditChallenge{}
		proof, decodeErr := hex.DecodeString(hold.AuditProof)
		if decodeErr != nil || proto.Unmarshal(action.Challenge, challenge) != nil || !bytes.Equal(challenge.VrfProof, proof) {
			return errors.New("audit selection proof mismatch")
		}
		hold.AuditRef, hold.EvidenceRef = auditRef, evidenceRef
		hold.VerdictBytes = append([]byte(nil), action.Verdict...)
		if outcome == "accept" {
			hold.AuditVerdict = "accept"
			s.Evidence[evidenceRef] = &WorkEvidence{HoldID: hold.ID, Node: hold.Node, Reference: evidenceRef}
		} else if outcome == "reject" && hold.AuditRound == 1 {
			hold.FirstAuditor, hold.FirstVerdict = signer, outcome
			next, nextProof, selectErr := s.selectAuditor(hold, signer)
			if selectErr != nil {
				hold.AuditVerdict = "service_failure"
			} else {
				hold.Auditor, hold.AuditProof, hold.AuditRound = next, hex.EncodeToString(nextProof), 2
				hold.AuditRef, hold.EvidenceRef, hold.VerdictBytes = "", "", nil
			}
		} else if outcome == "reject" && hold.AuditRound == 2 {
			hold.AuditVerdict = "reject"
		} else {
			hold.AuditVerdict = "service_failure"
		}
	case "settle":
		hold := s.Holds[action.HoldID]
		if hold == nil || hold.State != "dispute" || hold.AuditVerdict == "" || action.Outcome != hold.AuditVerdict {
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
		if err := s.settleAt(hold, action.Outcome, height); err != nil {
			return err
		}
	case "expire_hold":
		hold := s.Holds[action.HoldID]
		if hold == nil || (hold.State != "frozen" && hold.State != "dispute") || height < hold.ExpireHeight {
			return errors.New("hold is not expired")
		}
		if err := s.creditBalance(hold.Payer, hold.Amount); err != nil {
			return err
		}
		hold.State = "cancelled"
	case "cancel":
		hold := s.Holds[action.HoldID]
		if hold == nil || hold.State != "frozen" || (signer != hold.Payer && !s.Coordinators[signer]) {
			return errors.New("invalid cancellation")
		}
		if err := s.creditBalance(hold.Payer, hold.Amount); err != nil {
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

func (s *State) selectAuditor(hold *Hold, exclude string) (string, []byte, error) {
	if hold.Pipeline != nil {
		return s.selectPipelineAuditor(hold, exclude)
	}
	if len(s.Entropy) != sha256.Size {
		return "", nil, errors.New("chain randomness unavailable")
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
	sort.Strings(ids)
	if len(ids) == 0 {
		return "", nil, errors.New("no independent auditor available")
	}
	h := sha256.New()
	h.Write([]byte("ember/audit-selection/v1\x00"))
	h.Write(s.Entropy)
	h.Write([]byte(hold.ID))
	h.Write([]byte(hold.ReceiptRef))
	h.Write([]byte{byte(hold.AuditRound + 1)})
	proof := h.Sum(nil)
	index := binary.BigEndian.Uint64(proof[:8]) % uint64(len(ids))
	return ids[index], proof, nil
}

// The signer identity was already established by the envelope. This helper
// keeps the audit verifier interface explicit without introducing key lookup.
func envPublicID(id string) string { return id }

func (s *State) settle(hold *Hold, outcome string) error {
	return s.settleAt(hold, outcome, s.Height)
}

func (s *State) settleAt(hold *Hold, outcome string, height int64) error {
	if hold.Pipeline != nil {
		return s.settlePipeline(hold, outcome, height)
	}
	switch outcome {
	case "accept":
		if err := s.creditBalance(hold.Payer, hold.Amount-hold.Actual); err != nil {
			return err
		}
		nodeAmount := hold.Actual
		if policy := s.Models[hold.ModelID].Pricing; policy != nil {
			shares, err := s.priceShares(hold, policy)
			if err != nil {
				return err
			}
			hold.Shares = shares
			nodeAmount = shares.Executor
			if err = s.creditBalance(hold.Auditor, shares.Auditor); err != nil {
				return err
			}
			if err = s.creditBalance(hold.Payer, shares.Consumer); err != nil {
				return err
			}
		}
		if err := s.creditBalance(hold.Node, nodeAmount); err != nil {
			return err
		}
		hold.State = "settled"
		if m := s.Models[hold.ModelID]; m.Publication != nil {
			m.Publication.SettledRequests++
		}
	case "reject", "service_failure":
		if err := s.creditBalance(hold.Payer, hold.Amount); err != nil {
			return err
		}
		hold.State = "cancelled"
		if outcome == "reject" {
			if err := s.slashOffender(hold, hold.Node, hold.Actual, height); err != nil {
				return err
			}
			hold.State = "slashed"
		}
	default:
		return errors.New("invalid settlement outcome")
	}
	return nil
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
	if proto.Unmarshal(raw, receipt) != nil || receipt.RequestId != holdID || core.Verify(receipt) != nil {
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
	raw, err := state.Bytes()
	if err != nil {
		return err
	}
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
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state.Version != AppVersion {
		return nil, errors.New("unsupported state version")
	}
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
