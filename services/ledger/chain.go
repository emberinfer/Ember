package ledger

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	chainapp "ember/services/chain"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"google.golang.org/protobuf/proto"
)

type ChainConfig struct {
	SignLock       string // Optional supervisor-wide transaction nonce lock.
	RPC            string
	ChainID        string
	CoordinatorKey string
	AuditorKey     string
	AuthFile       string
	FaucetKey      string
}

type chainAuthEntry struct {
	Token   string `json:"token"`
	Key     string `json:"key"`
	Address string `json:"address"`
}

type ChainLedger struct {
	signLock      string
	pricing       pricingResolver
	mu            sync.Mutex
	rpc           *rpchttp.HTTP
	chainID       string
	coordinator   ed25519.PrivateKey
	auditor       ed25519.PrivateKey
	faucet        ed25519.PrivateKey
	accounts      map[string]ed25519.PrivateKey
	tokens        map[[32]byte]string
	signingTokens map[[32]byte]bool
}

func OpenChain(config ChainConfig) (*ChainLedger, error) {
	if config.RPC == "" || config.ChainID == "" || config.CoordinatorKey == "" || config.AuditorKey == "" || config.AuthFile == "" {
		return nil, errors.New("chain RPC, chain ID, authority keys and auth file are required")
	}
	client, err := rpchttp.New(config.RPC)
	if err != nil {
		return nil, err
	}
	coordinator, err := readPrivateKey(config.CoordinatorKey)
	if err != nil {
		return nil, err
	}
	auditor, err := readPrivateKey(config.AuditorKey)
	if err != nil {
		return nil, err
	}
	raw, err := readSecret(config.AuthFile)
	if err != nil {
		return nil, err
	}
	var entries []chainAuthEntry
	if json.Unmarshal(raw, &entries) != nil || len(entries) == 0 {
		return nil, errors.New("invalid chain auth file")
	}
	ledger := &ChainLedger{signLock: config.SignLock, rpc: client, chainID: config.ChainID, coordinator: coordinator, auditor: auditor, accounts: map[string]ed25519.PrivateKey{}, tokens: map[[32]byte]string{}, signingTokens: map[[32]byte]bool{}}
	if config.FaucetKey != "" {
		ledger.faucet, err = readPrivateKey(config.FaucetKey)
		if err != nil {
			return nil, err
		}
	}
	for _, entry := range entries {
		if len(entry.Token) < 32 {
			return nil, errors.New("chain auth tokens must be at least 32 bytes")
		}
		if (entry.Key == "") == (entry.Address == "") {
			return nil, errors.New("chain auth entry requires exactly one of key or address")
		}
		digest := sha256.Sum256([]byte(entry.Token))
		if ledger.tokens[digest] != "" {
			return nil, errors.New("duplicate auth token")
		}
		id := entry.Address
		if entry.Key != "" {
			key, keyErr := readPrivateKey(entry.Key)
			if keyErr != nil {
				return nil, keyErr
			}
			id = chainapp.AccountID(key.Public().(ed25519.PublicKey))
			ledger.accounts[id] = key
			ledger.signingTokens[digest] = true
		} else {
			decoded, err := hex.DecodeString(id)
			if err != nil || len(decoded) != sha256.Size {
				return nil, errors.New("chain auth address must be 64 hex characters")
			}
			id = hex.EncodeToString(decoded)
		}
		ledger.tokens[digest] = id
	}
	if len(ledger.faucet) != 0 {
		id := chainapp.AccountID(ledger.faucet.Public().(ed25519.PublicKey))
		if ledger.accounts[id] != nil || id == chainapp.AccountID(coordinator.Public().(ed25519.PublicKey)) || id == chainapp.AccountID(auditor.Public().(ed25519.PublicKey)) {
			return nil, errors.New("faucet requires a dedicated account, distinct from gateway custody and authority keys")
		}
	}
	return ledger, nil
}

func readSecret(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s must be mode 0600", path)
	}
	return raw, nil
}
func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := readSecret(path)
	if err != nil {
		return nil, err
	}
	if len(raw) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(raw), nil
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	return ed25519.PrivateKey(raw), nil
}
func (l *ChainLedger) Close() error { return nil }
func (l *ChainLedger) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := l.rpc.Status(ctx)
	return err
}
func (l *ChainLedger) Create(string, int64) error {
	return errors.New("chain accounts must be allocated by genesis or transfer")
}
func (l *ChainLedger) IssueToken(string, int64, string) error {
	return errors.New("chain auth mappings are loaded from --chain-auth-file")
}
func (l *ChainLedger) TokenAccount(token string) (string, error) {
	digest := sha256.Sum256([]byte(token))
	id := l.tokens[digest]
	if id == "" {
		return "", errors.New("invalid token")
	}
	return id, nil
}

// TokenCanSign is a credential capability, independent of other tokens for
// the same address. Address bindings never grant access to custody keys.
func (l *ChainLedger) TokenCanSign(token string) bool {
	return l.signingTokens[sha256.Sum256([]byte(token))]
}
func (l *ChainLedger) Balance(id string) (int64, int64, error) {
	var account chainapp.Account
	if err := l.query("/account/"+id, &account); err != nil {
		return 0, 0, err
	}
	return account.Balance, account.Stake, nil
}
func (l *ChainLedger) Stake(id string, amount int64) error {
	return l.withAccount(id, chainapp.Action{Type: "stake", Amount: amount})
}

func (l *ChainLedger) FaucetTransfer(to string, amount int64) error {
	if len(l.faucet) == 0 {
		return errors.New("testnet faucet is disabled")
	}
	if to == "" || amount <= 0 {
		return errors.New("invalid faucet transfer")
	}
	return l.submit(l.faucet, chainapp.Action{Type: "transfer", To: to, Amount: amount})
}
func (l *ChainLedger) Freeze(id, payer, node string, amount int64) error {
	return l.withAccount(payer, chainapp.Action{Type: "freeze", HoldID: id, Payer: payer, Node: node, Amount: amount})
}
func (l *ChainLedger) Submit(id string, receipt *pb.ExecutionReceipt, actual int64, _ time.Duration) error {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(receipt)
	if err != nil {
		return err
	}
	return l.submit(l.coordinator, chainapp.Action{Type: "submit", HoldID: id, Actual: actual, Receipt: raw})
}
func (l *ChainLedger) Finish(id, verdict string, _ time.Time, multiplier int64) error {
	if multiplier != 20 {
		return errors.New("chain slash multiplier is fixed by genesis")
	}
	h, err := l.hold(id)
	if err != nil {
		return err
	}
	var receipts []*pb.SettlementReceipt
	if verdict == "reject" && h.AuditCompPolicy != nil && h.ConsumerGray != nil && h.ConsumerGray.Charge {
		err = l.query("/settlement-receipts/"+id+"?outcome="+url.QueryEscape(verdict), &receipts)
	} else {
		receipts, err = chainapp.ExpectedAuditReceipts(h, verdict)
	}
	if err != nil {
		return err
	}
	return l.submit(l.coordinator, chainapp.Action{Type: "settle", HoldID: id, Outcome: verdict, AuditReceipts: receipts})
}
func (l *ChainLedger) Cancel(id string) error {
	hold, err := l.hold(id)
	if err != nil {
		return err
	}
	if key := l.accounts[hold.Payer]; key != nil {
		return l.submit(key, chainapp.Action{Type: "cancel", HoldID: id})
	}
	return l.submit(l.coordinator, chainapp.Action{Type: "cancel", HoldID: id})
}
func (l *ChainLedger) RecordAudit(id string, challenge *pb.AuditChallenge, verdict *pb.AuditVerdict) error {
	hold, err := l.hold(id)
	if err != nil {
		return err
	}
	if hold.Auditor == "" {
		if err = l.submit(l.coordinator, chainapp.Action{Type: "assign_audit", HoldID: id}); err != nil {
			return err
		}
		hold, err = l.hold(id)
		if err != nil {
			return err
		}
	}
	auditorID := chainapp.AccountID(l.auditor.Public().(ed25519.PublicKey))
	if hold.Auditor != auditorID {
		return fmt.Errorf("audit assigned to %s, local auditor is %s", hold.Auditor, auditorID)
	}
	proof, err := hex.DecodeString(hold.AuditProof)
	if err != nil || len(proof) != sha256.Size {
		return errors.New("invalid chain audit assignment proof")
	}
	challenge.VrfProof = proof
	if err = core.Sign(challenge, l.auditor); err != nil {
		return err
	}
	challengeRef, err := core.Digest(challenge)
	if err != nil {
		return err
	}
	verdict.ChallengeRef = challengeRef
	verdict.AuditorId = auditorID
	if err = core.Sign(verdict, l.auditor); err != nil {
		return err
	}
	c, err := proto.MarshalOptions{Deterministic: true}.Marshal(challenge)
	if err != nil {
		return err
	}
	v, err := proto.MarshalOptions{Deterministic: true}.Marshal(verdict)
	if err != nil {
		return err
	}
	return l.submit(l.auditor, chainapp.Action{Type: "audit", HoldID: id, Challenge: c, Verdict: v})
}

func (l *ChainLedger) RecoverFrozen() (int64, error) {
	var holds map[string]*chainapp.Hold
	if err := l.query("/expired-holds", &holds); err != nil {
		return 0, err
	}
	var recovered int64
	for _, hold := range holds {
		if err := l.submit(l.coordinator, chainapp.Action{Type: "expire_hold", HoldID: hold.ID}); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

// NodeProfile validates enrollment even when a node is currently at its cap.
func (l *ChainLedger) NodeProfile(id string) (*pb.ResourceProfile, error) {
	return l.nodeProfile(id, false)
}

// SchedulableNodeProfile additionally checks capacity for a new assignment.
func (l *ChainLedger) SchedulableNodeProfile(id string) (*pb.ResourceProfile, error) {
	return l.nodeProfile(id, true)
}

func (l *ChainLedger) nodeProfile(id string, forScheduling bool) (*pb.ResourceProfile, error) {
	var registration chainapp.NodeRegistration
	if err := l.query("/node/"+id, &registration); err != nil {
		return nil, err
	}
	profile := &pb.ResourceProfile{}
	if proto.Unmarshal(registration.Profile, profile) != nil || core.Verify(profile) != nil || profile.NodeId != id {
		return nil, errors.New("invalid chain node registration")
	}
	var status chainapp.NodeStatusView
	if err := l.query("/node-status/"+id, &status); err != nil {
		return nil, err
	}
	if registration.ExpiresHeight <= status.Height || !status.Active {
		return nil, errors.New("chain node registration expired or under-staked")
	}
	if forScheduling && !status.ExecutionAllowed {
		return nil, errors.New("execution opted out")
	}
	if forScheduling && !status.ContributionAvailable {
		return nil, errors.New("contribution unavailable: cap or qualification")
	}
	return profile, nil
}

func (l *ChainLedger) NodeProfiles() ([]*pb.ResourceProfile, error) {
	var statuses map[string]*chainapp.NodeStatusView
	if err := l.query("/node-statuses", &statuses); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(statuses))
	for id := range statuses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	profiles := make([]*pb.ResourceProfile, 0, len(ids))
	for _, id := range ids {
		status := statuses[id]
		r := status.Registration
		if r.ExpiresHeight <= status.Height || !status.ExecutionAllowed {
			continue
		}
		p := &pb.ResourceProfile{}
		if proto.Unmarshal(r.Profile, p) != nil || core.Verify(p) != nil || p.NodeId != id {
			return nil, errors.New("invalid chain node registration")
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

func (l *ChainLedger) State(id string) (string, error) {
	hold, err := l.hold(id)
	if err != nil {
		return "", err
	}
	return hold.State, nil
}
func (l *ChainLedger) RecordedVerdict(id string) (*pb.AuditVerdict, error) {
	hold, err := l.hold(id)
	if err != nil {
		return nil, err
	}
	if len(hold.VerdictBytes) == 0 {
		return nil, sql.ErrNoRows
	}
	verdict := &pb.AuditVerdict{}
	if proto.Unmarshal(hold.VerdictBytes, verdict) != nil {
		return nil, errors.New("invalid chain verdict")
	}
	return verdict, nil
}

func (l *ChainLedger) withAccount(id string, action chainapp.Action) error {
	key := l.accounts[id]
	if key == nil {
		return fmt.Errorf("no gateway-local signing key for account %s", id)
	}
	return l.submit(key, action)
}
func (l *ChainLedger) submit(key ed25519.PrivateKey, action chainapp.Action) error {
	// Workers can share authority and consumer keys. Serialize query/sign/commit
	// across processes so concurrent orders cannot reuse the same account nonce.
	if l.signLock != "" {
		file, err := os.OpenFile(l.signLock, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
			return err
		}
		defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.contributionAdmission(action); err != nil {
		return err
	}
	id := chainapp.AccountID(key.Public().(ed25519.PublicKey))
	var account chainapp.Account
	if err := l.query("/account/"+id, &account); err != nil {
		return err
	}
	raw, err := chainapp.Sign(l.chainID, key, account.Nonce+1, action)
	if err != nil {
		return err
	}
	result, err := broadcastConfirm(context.Background(), l.rpc, raw)
	if err != nil {
		return err
	}
	log.Printf("CHAIN TX type=%s height=%d tx_hash=%X", action.Type, result.Height, result.Hash)
	return nil
}
func (l *ChainLedger) query(path string, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := l.rpc.ABCIQuery(ctx, path, nil)
	if err != nil {
		return err
	}
	if result.Response.Code != 0 {
		return fmt.Errorf("chain query %s: %s", path, result.Response.Log)
	}
	return json.Unmarshal(result.Response.Value, out)
}
func (l *ChainLedger) hold(id string) (*chainapp.Hold, error) {
	var hold chainapp.Hold
	if err := l.query("/hold/"+id, &hold); err != nil {
		return nil, err
	}
	return &hold, nil
}

func (l *ChainLedger) FreezePipeline(id, payer string, amount int64, authorizations []StageAuthorization, total uint32) error {
	auth := make([]chainapp.PipelineAuthorization, len(authorizations))
	for i, v := range authorizations {
		auth[i] = chainapp.PipelineAuthorization{NodeID: v.NodeID, StartLayer: v.StartLayer, EndLayer: v.EndLayer}
	}
	return l.withAccount(payer, chainapp.Action{Type: "freeze_pipeline", HoldID: id, Payer: payer, Amount: amount, TotalLayers: total, PipelineAuthorizations: auth})
}
func (l *ChainLedger) SubmitPipeline(id string, r *pb.PipelineExecutionReceipt, actual int64, total uint32, _ time.Duration) ([]StageShare, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if err != nil {
		return nil, err
	}
	if err = l.submit(l.coordinator, chainapp.Action{Type: "submit_pipeline", HoldID: id, Actual: actual, Receipt: raw, TotalLayers: total}); err != nil {
		return nil, err
	}
	return l.PipelineCredits(id)
}
func (l *ChainLedger) FinishPipeline(id, outcome string, _ time.Time, multiplier int64) error {
	if multiplier != 20 {
		return errors.New("chain slash multiplier is fixed by genesis")
	}
	h, err := l.hold(id)
	if err != nil {
		return err
	}
	if h.AuditVerdict == "service_failure" {
		outcome = h.AuditVerdict
	}
	receipts, err := chainapp.ExpectedAuditReceipts(h, outcome)
	if err != nil {
		return err
	}
	return l.submit(l.coordinator, chainapp.Action{Type: "settle", HoldID: id, Outcome: outcome, AuditReceipts: receipts})
}
func (l *ChainLedger) CancelPipeline(id string) error { return l.Cancel(id) }
func (l *ChainLedger) RecordPipelineAudit(id string, c *pb.AuditChallenge, v *pb.AuditVerdict) error {
	return l.RecordAudit(id, c, v)
}
func (l *ChainLedger) RecordedPipelineVerdict(id string) (*pb.AuditVerdict, error) {
	return l.RecordedVerdict(id)
}
func (l *ChainLedger) PipelineCredits(id string) ([]StageShare, error) {
	h, err := l.hold(id)
	if err != nil {
		return nil, err
	}
	if h.Pipeline == nil {
		return nil, errors.New("not a pipeline hold")
	}
	return h.Pipeline.Credits, nil
}

var _ Backend = (*ChainLedger)(nil)

// CheckModelTrust refreshes finalized endorsement/revocation state for consumers.
func (l *ChainLedger) CheckModelTrust(id string, policy *chainapp.TrustPolicy) error {
	var state chainapp.State
	if err := l.query("/model-trust/"+id, &state); err != nil {
		return err
	}
	return state.CanServeModel(id, policy)
}

// SchedulingData returns only finalized, receipt-derived statistics and policy.
func (l *ChainLedger) SchedulingData() (*core.SchedulingPolicy, map[string]*chainapp.LatencyStats, error) {
	var state chainapp.State
	if err := l.query("/scheduling", &state); err != nil {
		return nil, nil, err
	}
	return state.Parameters.Scheduling, state.Latencies, nil
}

func (l *ChainLedger) contributionAdmission(action chainapp.Action) error {
	if action.Type != "freeze" && action.Type != "freeze_pipeline" {
		return nil
	}
	raw, err := json.Marshal(action)
	if err != nil {
		return err
	}
	var allowed bool
	return l.query("/contribution-admission?action="+url.QueryEscape(string(raw)), &allowed)
}

// FrozenTokenization returns the original signed commitment for evidence replay.
func (l *ChainLedger) FrozenTokenization(id string) (*chainapp.FrozenFee, error) {
	h, err := l.hold(id)
	if err != nil {
		return nil, err
	}
	return h.TokenFee, nil
}

// ResolveAudit bypasses all audit routing for a frozen unsampled request.
// Legacy holds keep their existing route and never send a new action to v7.
func (l *ChainLedger) ResolveAudit(id string) (bool, error) {
	h, err := l.hold(id)
	if err != nil {
		return false, err
	}
	if h.SamplingPolicy == nil {
		return false, nil
	}
	if h.Sampling == nil {
		if err = l.submit(l.coordinator, chainapp.Action{Type: "resolve_audit", HoldID: id}); err != nil {
			return false, err
		}
		h, err = l.hold(id)
		if err != nil {
			return false, err
		}
	}
	return h.Unaudited, nil
}

// ExecExitedNodes returns only effective chain exits, leaving pending nodes eligible.
func (l *ChainLedger) ExecExitedNodes() (map[string]bool, error) {
	var statuses map[string]*chainapp.NodeStatusView
	if err := l.query("/node-statuses", &statuses); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for id, v := range statuses {
		if v != nil && v.ExecExited {
			out[id] = true
		}
	}
	return out, nil
}

// ImageTransport exposes the existing daemon transport to its optional image
// host. All signed writes must hold the returned lock, shared with text writes.
// Credentials remain daemon-local and must never be sent over HTTP.
func (l *ChainLedger) ImageTransport() (*rpchttp.HTTP, string, ed25519.PrivateKey, map[string]ed25519.PrivateKey, *sync.Mutex) {
	return l.rpc, l.chainID, l.coordinator, l.accounts, &l.mu
}
