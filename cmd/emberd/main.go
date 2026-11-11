package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/auditqueue"
	chainapp "ember/services/chain"
	"ember/services/faucet"
	"ember/services/gateway"
	"ember/services/ledger"
	"ember/services/scheduler"
	"ember/services/tracker"
	"ember/services/tunnel"
	"ember/services/verifier"
	"ember/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

func main() {
	mode := "all"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "cert" {
		generateCert(os.Args[2:])
		return
	}
	if mode == "token" {
		issueToken(os.Args[2:])
		return
	}
	if mode == "version" {
		fmt.Println(version.Version)
		return
	}
	if mode != "all" && mode != "gateway" && mode != "scheduler" && mode != "tracker" && mode != "ledger" && mode != "verifier" {
		log.Fatal("mode: all|gateway|scheduler|tracker|ledger|verifier|cert|token")
	}
	flags := flag.NewFlagSet("emberd", flag.ExitOnError)
	database := flags.String("db", "ember.db", "SQLite ledger file")
	ledgerBackend := flags.String("ledger-backend", "sqlite", "ledger backend: sqlite or chain")
	chainRPC := flags.String("chain-rpc", "", "CometBFT RPC URL for chain ledger")
	chainID := flags.String("chain-id", "", "CometBFT chain ID")
	chainCoordinatorKey := flags.String("chain-coordinator-key", "", "mode-0600 coordinator Ed25519 key")
	chainAuditorKey := flags.String("chain-auditor-key", "", "mode-0600 auditor Ed25519 key")
	chainAuthFile := flags.String("chain-auth-file", "", "mode-0600 gateway bearer-token/account-key mapping")
	faucetHTTP := flags.String("faucet-http", "", "optional separate faucet-only listener; expose this instead of the gateway")
	faucetKey := flags.String("faucet-key", "", "optional mode-0600 testnet faucet account key")
	faucetAmount := flags.Int64("faucet-amount", 0, "valueless testnet Spark subunits per rate-limited claim (0 disables)")
	faucetWindow := flags.Duration("faucet-window", 24*time.Hour, "per-account and per-source faucet claim window")
	faucetTrustedProxy := flags.String("faucet-trusted-proxy", "", "single immediate proxy IP allowed to supply X-Forwarded-For; proxy must overwrite client headers")
	faucetState := flags.String("faucet-state", "", "mode-0600 persistent faucet rate-limit state")
	schedulerKeyFile := flags.String("scheduler-key", "", "mode-0600 scheduler Ed25519 key for signed node work")
	httpAddr := flags.String("http", "127.0.0.1:19090", "HTTP listen address")
	grpcAddr := flags.String("grpc", "127.0.0.1:19091", "tunnel listen address")
	engine := flags.String("engine", "mock", "audit engine")
	modelPath := flags.String("model-path", "", "model weights")
	genesisFile := flags.String("genesis", "", "signed genesis protobuf")
	auditorPython := flags.String("auditor-python", "node/.venv/bin/python", "Python interpreter for local audit")
	auditorPythonPath := flags.String("auditor-pythonpath", "core-py:proto:node", "Python import path for local audit")
	chainAuditRouting := flags.Bool("chain-audit-routing", true, "resolve assigned replay workers from finalized stake-backed registrations")
	auditDevice := flags.String("audit-device", "auto", "local audit device")
	auditJobDir := flags.String("audit-job-dir", "", "optional directory for reproducible audit jobs")
	auditSpoolDir := flags.String("audit-spool-dir", "", "durable pending-audit directory (defaults beside ledger)")
	auditKeyFile := flags.String("audit-key", "", "persistent Ed25519 audit key (defaults beside ledger)")
	pipelinePython := flags.String("pipeline-python", "", "Python interpreter for dynamic shard assembly")
	pipelineModelPath := flags.String("pipeline-model-path", "", "tokenizer/config path for dynamic shard assembly")
	pipelineSchedulerKey := flags.String("pipeline-scheduler-key", "", "mode-0600 on-chain scheduler key for shard RPC")
	auditWorkers := flags.Int("audit-workers", 2, "bounded concurrent audit jobs")
	auditTimeout := flags.Duration("audit-timeout", 3*time.Minute, "deadline for each reference replay")
	maxConcurrent := flags.Int("max-concurrent", 8, "maximum in-flight inference requests")
	requestTimeout := flags.Duration("request-timeout", 5*time.Minute, "per-attempt inference timeout")
	rateLimit := flags.Int("rate-limit", 60, "maximum requests per identity per minute (0 disables)")
	httpCert := flags.String("http-tls-cert", "", "optional HTTPS certificate")
	httpKey := flags.String("http-tls-key", "", "optional HTTPS private key")
	certFile := flags.String("tls-cert", "", "tunnel certificate")
	keyFile := flags.String("tls-key", "", "tunnel private key")
	insecure := flags.Bool("insecure-localhost", false, "allow plaintext tunnel on loopback only")
	demo := flags.Bool("demo-bootstrap", false, "enable legacy demo identities, funding and account-name auth")
	joinGrant := flags.Int64("join-grant", 0, "centrally issued onboarding funds per new node (demo only)")
	trustFile := flags.String("trust-policy", "", "local per-profile publisher/endorsement policy JSON; default requires governor endorsement for new profiles")
	manifestFile := flags.String("manifest", "", "signed manifest protobuf (defaults beside genesis)")
	_ = flags.Parse(os.Args[2:])
	if *faucetTrustedProxy != "" && net.ParseIP(*faucetTrustedProxy) == nil {
		log.Fatal("--faucet-trusted-proxy must be one IP address")
	}
	if *auditWorkers < 1 || *auditTimeout <= 0 || *maxConcurrent < 1 || *requestTimeout <= 0 || *rateLimit < 0 || *faucetAmount < 0 || *faucetWindow <= 0 || (*httpCert == "") != (*httpKey == "") {
		log.Fatal("invalid concurrency, timeout, or HTTP TLS configuration")
	}
	if mode != "all" && mode != "gateway" {
		log.Fatalf("standalone %s is not wired; use all", mode)
	}
	if *engine != "mock" && *genesisFile == "" {
		log.Fatal("real engine requires signed --genesis and manifest")
	}
	if *auditJobDir != "" {
		if err := os.MkdirAll(*auditJobDir, 0700); err != nil {
			log.Fatal(err)
		}
	}
	var book ledger.Backend
	var chainBook *ledger.ChainLedger
	var err error
	switch *ledgerBackend {
	case "sqlite":
		book, err = ledger.Open(*database)
	case "chain":
		if *demo {
			log.Fatal("chain ledger does not support demo bootstrap")
		}
		if *faucetAmount > 0 && *faucetKey == "" {
			log.Fatal("--faucet-amount requires --faucet-key")
		}
		chainBook, err = ledger.OpenChain(ledger.ChainConfig{RPC: *chainRPC, ChainID: *chainID,
			CoordinatorKey: *chainCoordinatorKey, AuditorKey: *chainAuditorKey, AuthFile: *chainAuthFile, FaucetKey: *faucetKey})
		book = chainBook
		if *schedulerKeyFile == "" {
			*schedulerKeyFile = *chainCoordinatorKey
		}
	default:
		log.Fatal("--ledger-backend must be sqlite or chain")
	}
	if *faucetHTTP != "" && *faucetAmount == 0 {
		log.Fatal("--faucet-http requires an enabled dedicated faucet account")
	}
	if *ledgerBackend != "chain" && (*faucetAmount > 0 || *faucetKey != "") {
		log.Fatal("testnet faucet requires the chain ledger backend")
	}
	if err != nil {
		log.Fatal(err)
	}
	defer book.Close()
	recovered, err := book.RecoverFrozen()
	if err != nil {
		log.Fatal(err)
	}
	if recovered > 0 {
		log.Printf("RECOVERY refunded_frozen=%d", recovered)
	}
	if chainBook != nil {
		go func() {
			for {
				time.Sleep(5 * time.Second)
				count, recoverErr := chainBook.RecoverFrozen()
				if recoverErr != nil {
					log.Printf("RECOVERY ERROR: %v", recoverErr)
				} else if count > 0 {
					log.Printf("RECOVERY refunded_expired=%d", count)
				}
			}
		}()
	}
	if *auditSpoolDir == "" {
		*auditSpoolDir = *database + ".audit-spool"
	}
	spool, err := auditqueue.Open(*auditSpoolDir)
	if err != nil {
		log.Fatal(err)
	}
	if *demo {
		for _, a := range []struct {
			id   string
			fund int64
		}{{"consumer", 1000000}, {"honest", 100000}, {"cheat", 100000}} {
			if _, _, err := book.Balance(a.id); err != nil {
				if err := book.Create(a.id, a.fund); err != nil {
					log.Fatal(err)
				}
				if a.id != "consumer" {
					if err := book.Stake(a.id, 50000); err != nil {
						log.Fatal(err)
					}
				}
			}
		}
	}
	relay := tunnel.New()
	relay.Legacy = *demo
	relay.Ledger = book
	relay.JoinGrant = *joinGrant
	relay.Tracker = tracker.New()
	if *schedulerKeyFile != "" {
		relay.SchedulerKey, err = loadAuditKey(*schedulerKeyFile)
		if err != nil {
			log.Fatal(err)
		}
	}
	if chainBook != nil {
		relay.RequireRegistration = func(profile *pb.ResourceProfile) error {
			var registered *pb.ResourceProfile
			var queryErr error
			// A remote RPC can report commit before this validator applies it.
			// Wait for propagation without relaxing signed-profile equality.
			for attempt := 0; attempt < 20; attempt++ {
				registered, queryErr = chainBook.NodeProfile(profile.NodeId)
				if queryErr == nil && proto.Equal(profile, registered) {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			if queryErr != nil {
				return queryErr
			}
			if !proto.Equal(profile, registered) {
				return errors.New("connected profile differs from finalized chain registration")
			}
			return nil
		}
	}
	var counter uint64
	model := "ember-mock"
	if *engine != "mock" {
		model = "ember-qwen2.5-1.5b"
	}
	api := &gateway.Gateway{Ledger: book, Tunnel: relay, Profile: "v1", Manifest: make([]byte, 32), Price: 1, Dispute: 0, DemoBootstrap: *demo,
		Admission: make(chan struct{}, *maxConcurrent), RequestTimeout: *requestTimeout, MaxBodyBytes: 1 << 20,
		RateLimit: *rateLimit, RateWindow: time.Minute}
	if chainBook != nil {
		var trust *chainapp.TrustPolicy
		if *trustFile != "" {
			raw, e := os.ReadFile(*trustFile)
			if e != nil {
				log.Fatal(e)
			}
			trust = &chainapp.TrustPolicy{}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if e = decoder.Decode(trust); e != nil {
				log.Fatal(e)
			}
			for _, rule := range trust.Profiles {
				if rule.MinEndorsers < 0 {
					log.Fatal("negative trust threshold")
				}
			}
		}
		api.CheckModelTrust = func(id string) error { return chainBook.CheckModelTrust(id, trust) }
		api.SchedulingPolicy = func() (*core.SchedulingPolicy, error) { policy, _, e := chainBook.SchedulingData(); return policy, e }
	}
	if *auditKeyFile == "" {
		*auditKeyFile = *database + ".audit.key"
	}
	auditKey, err := loadAuditKey(*auditKeyFile)
	if err != nil {
		log.Fatal(err)
	}
	auditTolerance := 0.001
	slashMultiplier := int64(20)
	if *genesisFile != "" {
		raw, err := os.ReadFile(*genesisFile)
		if err != nil {
			log.Fatal(err)
		}
		genesis := &pb.ChainGenesis{}
		if err := proto.Unmarshal(raw, genesis); err != nil {
			log.Fatal(err)
		}
		if err := core.Verify(genesis); err != nil {
			log.Fatal(err)
		}
		if err := verifier.ValidateNumericPolicy(genesis); err != nil {
			log.Fatal(err)
		}
		manifestPath := *manifestFile
		if manifestPath == "" {
			manifestPath = filepath.Join(filepath.Dir(*genesisFile), "manifest.pb")
		}
		manifestRaw, e := os.ReadFile(manifestPath)
		if e != nil {
			log.Fatal(e)
		}
		manifest := &pb.ModelManifest{}
		if e = proto.Unmarshal(manifestRaw, manifest); e != nil {
			log.Fatal(e)
		}
		if e = core.Verify(manifest); e != nil {
			log.Fatal(e)
		}
		digest, e := core.Digest(manifest)
		if e != nil {
			log.Fatal(e)
		}
		if manifest.ChainId != genesis.ChainId || !bytes.Equal(digest, genesis.ModelManifestRoot) {
			log.Fatal("genesis manifest mismatch")
		}
		relay.Genesis, relay.GenesisBytes, relay.ManifestBytes = genesis, raw, manifestRaw
		relay.Manifest = manifest
		model = genesis.ChainId
		api.Manifest = genesis.ModelManifestRoot
		api.Profile = genesis.ExecutionProfileVer
		api.Price = genesis.PriceOutput
		api.PriceInput, api.PriceContext, api.ChainID = genesis.PriceInput, genesis.PriceContext, genesis.ChainId
		if _, e := core.TokenFee(genesis, 0, 0); e != nil {
			log.Fatal(e)
		}
		if (genesis.PriceInput != 0 || genesis.PriceContext != 0) && chainBook == nil {
			log.Fatal("token pricing requires consensus backend")
		}
		api.TotalLayers = manifest.TotalLayers
		if *pipelinePython != "" || *pipelineModelPath != "" || *pipelineSchedulerKey != "" {
			if *pipelinePython == "" || *pipelineModelPath == "" || *pipelineSchedulerKey == "" || (len(manifest.Shards) < 2 && !manifest.DynamicRanges) {
				log.Fatal("dynamic pipeline requires --pipeline-python, --pipeline-model-path, --pipeline-scheduler-key and a sharded manifest")
			}
			if genesis.NumericAuditPolicy == nil {
				log.Fatal("dynamic pipeline requires a signed numeric audit policy")
			}
			workDir, e := os.Getwd()
			if e != nil {
				log.Fatal(e)
			}
			api.Pipeline = &gateway.PipelineConfig{DynamicRanges: manifest.DynamicRanges, MinStageLayers: manifest.MinStageLayers, AllowedCuts: manifest.AllowedLayerCuts, WitnessKeyFile: *chainAuditorKey, Python: *pipelinePython, WorkDir: workDir,
				ModelPath: *pipelineModelPath, SchedulerKeyFile: *pipelineSchedulerKey, Timeout: *requestTimeout}
		}
		if genesis.AuditParams != nil {
			auditTolerance = genesis.AuditParams.NumericTolerance
			slashMultiplier = int64(genesis.AuditParams.SlashMultiplier)
		}
	} else {
		manifest := &pb.ModelManifest{ChainId: model}
		if e := core.Sign(manifest, auditKey); e != nil {
			log.Fatal(e)
		}
		root, e := core.Digest(manifest)
		if e != nil {
			log.Fatal(e)
		}
		genesis := &pb.ChainGenesis{ChainId: model, ModelManifestRoot: root, ExecutionProfileVer: "v1", PriceOutput: 1}
		if e = core.Sign(genesis, auditKey); e != nil {
			log.Fatal(e)
		}
		relay.ManifestBytes, _ = proto.Marshal(manifest)
		relay.Manifest = manifest
		relay.GenesisBytes, _ = proto.Marshal(genesis)
		relay.Genesis = genesis
		api.Manifest = root
	}
	var opts []grpc.ServerOption
	if *certFile != "" && *keyFile != "" {
		pair, e := tls.LoadX509KeyPair(*certFile, *keyFile)
		if e != nil {
			log.Fatal(e)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})))
	} else {
		host, _, e := net.SplitHostPort(*grpcAddr)
		if e != nil {
			log.Fatal(e)
		}
		if !*insecure || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			log.Fatal("tunnel TLS required; plaintext requires --insecure-localhost on loopback")
		}
	}
	server := grpc.NewServer(opts...)
	pb.RegisterNodeTunnelServer(server, relay)
	listener, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		if e := server.Serve(listener); e != nil {
			log.Print(e)
		}
	}()
	defer server.Stop()
	api.Candidates = func() []scheduler.Candidate {
		if !*demo {
			out := []scheduler.Candidate{}
			var stats map[string]*chainapp.LatencyStats
			if chainBook != nil {
				var e error
				_, stats, e = chainBook.SchedulingData()
				if e != nil {
					return out
				}
			}
			for _, p := range relay.Tracker.Alive(time.Now()) {
				_, stake, e := book.Balance(p.NodeId)
				if e == nil && chainBook != nil {
					p, e = chainBook.NodeProfile(p.NodeId)
				}
				if e == nil {
					var latency uint64
					var samples uint32
					if data := stats[chainapp.LatencyKey(p.NodeId, p.ChainId)]; data != nil {
						latency = data.PercentileMicrosPerUnit
						samples = uint32(len(data.Window))
					}
					out = append(out, scheduler.Candidate{Profile: p, Online: relay.Online(p.NodeId), Stake: stake, Reputation: 0.8, LatencyMicrosPerUnit: latency, LatencySamples: samples})
				}
			}
			return out
		}
		n := atomic.AddUint64(&counter, 1)
		profiles := []*pb.ResourceProfile{{NodeId: "honest", ChainId: model, MemoryBytes: 16 << 30, ManifestRoot: api.Manifest, ExecutionProfileVer: api.Profile}, {NodeId: "cheat", ChainId: model, MemoryBytes: 16 << 30, ManifestRoot: api.Manifest, ExecutionProfileVer: api.Profile}}
		out := []scheduler.Candidate{}
		for _, p := range profiles {
			_, stake, _ := book.Balance(p.NodeId)
			rep := 0.8
			if (n%2 == 0 && p.NodeId == "cheat") || (n%2 == 1 && p.NodeId == "honest") {
				rep = 0.9
			}
			out = append(out, scheduler.Candidate{Profile: p, Online: relay.Online(p.NodeId), Stake: stake, Reputation: rep})
		}
		return out
	}
	type auditJob struct {
		Kind            string          `json:"kind,omitempty"`
		Receipt         string          `json:"receipt"`
		PipelineReceipt string          `json:"pipeline_receipt,omitempty"`
		Openings        json.RawMessage `json:"openings"`
		Invocations     []struct {
			Request  string  `json:"request"`
			Response string  `json:"response"`
			Profile  string  `json:"profile"`
			StageMS  float64 `json:"stage_ms"`
		} `json:"invocations,omitempty"`
		Failovers       json.RawMessage `json:"failovers,omitempty"`
		Metric          string          `json:"metric,omitempty"`
		PolicyRef       string          `json:"policy_ref,omitempty"`
		Prompt          string          `json:"prompt"`
		Seed            int64           `json:"seed"`
		Engine          string          `json:"engine"`
		ModelPath       string          `json:"model_path"`
		Tolerance       float64         `json:"tolerance"`
		Device          string          `json:"device"`
		TTFT            int64           `json:"ttft_ms"`
		Genesis         string          `json:"genesis,omitempty"`
		ProducerProfile string          `json:"producer_profile,omitempty"`
	}
	encodeAuditJob := func(receipt *pb.ExecutionReceipt, evidence []byte, prompt string, seed int64, ttft int64) ([]byte, error) {
		receiptBytes, err := proto.Marshal(receipt)
		if err != nil {
			return nil, err
		}
		job := auditJob{Receipt: fmt.Sprintf("%x", receiptBytes), Openings: json.RawMessage(evidence),
			Prompt: prompt, Seed: seed, Engine: *engine, ModelPath: *modelPath,
			Tolerance: auditTolerance, Device: *auditDevice, TTFT: ttft}
		if relay.Genesis.NumericAuditPolicy != nil {
			job.Genesis = hex.EncodeToString(relay.GenesisBytes)
			for _, p := range relay.Tracker.Alive(time.Now()) {
				if p.NodeId == receipt.NodeId {
					raw, e := proto.Marshal(p)
					if e != nil {
						return nil, e
					}
					job.ProducerProfile = hex.EncodeToString(raw)
					break
				}
			}
			if job.ProducerProfile == "" {
				return nil, errors.New("numeric policy requires authenticated producer profile")
			}
		}
		return json.Marshal(job)
	}
	api.BeforeSubmit = func(receipt *pb.ExecutionReceipt, evidence []byte, prompt string, seed int64, ttft int64) error {
		raw, err := encodeAuditJob(receipt, evidence, prompt, seed, ttft)
		if err != nil {
			return err
		}
		return spool.Put(receipt.RequestId, raw)
	}
	api.BeforePipelineSubmit = func(execution *gateway.PipelineExecution, prompt string, seed int64) error {
		receiptBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(execution.Receipt)
		if err != nil {
			return err
		}
		job := auditJob{Kind: "pipeline", PipelineReceipt: hex.EncodeToString(receiptBytes),
			Openings: execution.Openings, Failovers: execution.Failovers, Prompt: prompt, Seed: seed,
			Engine: *engine, ModelPath: *modelPath, Tolerance: auditTolerance,
			Device: *auditDevice, TTFT: execution.TTFT,
			Genesis: hex.EncodeToString(relay.GenesisBytes)}
		if relay.Genesis.NumericAuditPolicy != nil {
			job.Metric = relay.Genesis.NumericAuditPolicy.Metric
			for _, band := range relay.Genesis.NumericAuditPolicy.Bands {
				if band.Mode == "enforce" && band.ProducerDevice == "cpu" && band.ReferenceDevice == "cpu" {
					job.Tolerance = band.Tolerance
					break
				}
			}
			ref := sha256.Sum256(relay.GenesisBytes)
			job.PolicyRef = hex.EncodeToString(ref[:])
		}
		for _, invocation := range execution.Invocations {
			job.Invocations = append(job.Invocations, struct {
				Request  string  `json:"request"`
				Response string  `json:"response"`
				Profile  string  `json:"profile"`
				StageMS  float64 `json:"stage_ms"`
			}{hex.EncodeToString(invocation.Request), hex.EncodeToString(invocation.Response),
				hex.EncodeToString(invocation.Profile), invocation.StageMS})
		}
		raw, err := json.Marshal(job)
		if err != nil {
			return err
		}
		return spool.Put(execution.Receipt.RequestId, raw)
	}
	var routeMu sync.Mutex
	auditorBusy := map[string]bool{}
	processAudit := func(id string) {
		state, err := book.State(id)
		if err != nil {
			log.Printf("AUDIT RECOVERY STATE %s: %v", id, err)
			return
		}
		if state != "dispute" {
			if state == "settled" || state == "slashed" || state == "cancelled" {
				_ = spool.Done(id)
			}
			return
		}
		raw, err := spool.Get(id)
		if err != nil {
			log.Printf("AUDIT SPOOL READ %s: %v", id, err)
			return
		}
		var job auditJob
		if err = json.Unmarshal(raw, &job); err != nil {
			log.Printf("AUDIT SPOOL DECODE %s: %v", id, err)
			return
		}
		var recorded *pb.AuditVerdict
		var recordedErr error
		if job.Kind == "pipeline" {
			recorded, recordedErr = book.RecordedPipelineVerdict(id)
		} else {
			recorded, recordedErr = book.RecordedVerdict(id)
		}
		if recordedErr == nil {
			if recorded.Verdict == "accept" || recorded.Verdict == "reject" {
				if job.Kind == "pipeline" {
					recordedErr = book.FinishPipeline(id, recorded.Verdict, time.Now().Add(time.Second), slashMultiplier)
				} else {
					recordedErr = book.Finish(id, recorded.Verdict, time.Now().Add(time.Second), slashMultiplier)
				}
				if recordedErr != nil {
					log.Printf("AUDIT RECOVERY SETTLE %s: %v", id, recordedErr)
					return
				}
				_ = spool.Done(id)
			}
			return
		} else if !errors.Is(recordedErr, sql.ErrNoRows) {
			log.Printf("AUDIT RECOVERY VERDICT %s: %v", id, recordedErr)
			return
		}
		if chainBook != nil && *chainAuditRouting && job.Kind != "pipeline" {
			hold, producer, _, routeErr := chainBook.AuditRoute(id)
			if routeErr != nil {
				log.Printf("AUDIT ROUTE %s: %v", id, routeErr)
				return
			}
			profileRaw, routeErr := proto.MarshalOptions{Deterministic: true}.Marshal(producer)
			if routeErr != nil {
				log.Printf("AUDIT PROFILE %s: %v", id, routeErr)
				return
			}
			job.ProducerProfile = hex.EncodeToString(profileRaw)
			routed, routeErr := json.Marshal(job)
			if routeErr != nil {
				log.Printf("AUDIT JOB %s: %v", id, routeErr)
				return
			}
			routeMu.Lock()
			if auditorBusy[hold.Auditor] {
				routeMu.Unlock()
				return
			}
			auditorBusy[hold.Auditor] = true
			routeMu.Unlock()
			defer func() { routeMu.Lock(); delete(auditorBusy, hold.Auditor); routeMu.Unlock() }()
			ctx, cancel := context.WithTimeout(context.Background(), *auditTimeout)
			defer cancel()
			var envelope []byte
			routeErr = relay.Audit(ctx, hold.Auditor, id, fmt.Sprintf("audit-%s-%d", id, time.Now().UnixNano()), routed, func(f *pb.TunnelFrame) error {
				if f.Kind == "error" {
					return errors.New(string(f.Payload))
				}
				if f.Kind != "audit_result" {
					return errors.New("unexpected audit reply")
				}
				var result struct {
					Envelope string `json:"envelope"`
				}
				if err := json.Unmarshal(f.Payload, &result); err != nil {
					return err
				}
				var err error
				envelope, err = base64.StdEncoding.DecodeString(result.Envelope)
				return err
			})
			if routeErr == nil {
				routeErr = chainBook.RecordRemoteAudit(id, envelope)
			}
			if routeErr != nil {
				log.Printf("AUDIT REMOTE %s: %v", id, routeErr)
				return
			}
			verdict, routeErr := book.RecordedVerdict(id)
			if routeErr == nil && (verdict.Verdict == "accept" || verdict.Verdict == "reject") {
				routeErr = book.Finish(id, verdict.Verdict, time.Now(), slashMultiplier)
				if routeErr == nil {
					_ = spool.Done(id)
				}
			}
			log.Printf("AUDIT DERIVED request=%s producer=%s auditor=%s result=%v", id, hold.Node, hold.Auditor, routeErr)
			return
		}
		var challenge *pb.AuditChallenge
		var drawn bool
		var inputTokens, outputTokens uint32
		var elapsedMS int64
		nodeLabel := "pipeline-stages"
		if job.Kind == "pipeline" {
			receiptBytes, decodeErr := hex.DecodeString(job.PipelineReceipt)
			receipt := &pb.PipelineExecutionReceipt{}
			if decodeErr != nil || proto.Unmarshal(receiptBytes, receipt) != nil || receipt.RequestId != id {
				log.Printf("AUDIT PIPELINE RECEIPT INVALID %s", id)
				return
			}
			challenge, drawn, err = verifier.DrawPipeline(receipt, auditKey, 100, time.Now())
			inputTokens, outputTokens, elapsedMS = receipt.InputTokens, receipt.OutputTokens, receipt.ElapsedMs
		} else {
			receiptBytes, decodeErr := hex.DecodeString(job.Receipt)
			receipt := &pb.ExecutionReceipt{}
			if decodeErr != nil || proto.Unmarshal(receiptBytes, receipt) != nil || receipt.RequestId != id {
				log.Printf("AUDIT RECEIPT INVALID %s", id)
				return
			}
			challenge, drawn, err = verifier.Draw(receipt, auditKey, 100, time.Now())
			inputTokens, outputTokens, elapsedMS, nodeLabel = receipt.InputTokens, receipt.OutputTokens, receipt.ElapsedMs, receipt.NodeId
		}
		if err != nil || !drawn {
			log.Printf("AUDIT DRAW ERROR %s %v", id, err)
			return
		}
		if *auditJobDir != "" {
			if err := os.WriteFile(fmt.Sprintf("%s/%s.json", *auditJobDir, id), raw, 0600); err != nil {
				log.Printf("AUDIT JOB WRITE %s: %v", id, err)
				return
			}
		}
		auditCtx, stopAudit := context.WithTimeout(context.Background(), *auditTimeout)
		defer stopAudit()
		command := exec.CommandContext(auditCtx, *auditorPython, "-m", "auditor")
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
		command.WaitDelay = 5 * time.Second
		command.Env = replaceEnv(os.Environ(), "PYTHONPATH", *auditorPythonPath)
		command.Stdin = bytes.NewReader(raw)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		result, err := command.Output()
		if err != nil {
			log.Printf("AUDIT ERROR %s: %v %s", id, err, stderr.String())
			return
		}
		var verdict struct {
			Verdict      string   `json:"verdict"`
			Distance     float64  `json:"distance"`
			Device       string   `json:"device"`
			PeakRSS      int64    `json:"peak_rss_bytes"`
			PolicyRef    string   `json:"policy_ref"`
			Grade        string   `json:"grade"`
			FaultNodeIDs []string `json:"fault_node_ids"`
		}
		if err := json.Unmarshal(result, &verdict); err != nil {
			log.Printf("AUDIT DECODE %v %s", err, result)
			return
		}
		var policyRef []byte
		if relay.Genesis.NumericAuditPolicy != nil {
			ref := sha256.Sum256(relay.GenesisBytes)
			if verdict.PolicyRef != hex.EncodeToString(ref[:]) {
				log.Printf("AUDIT POLICY MISMATCH %s", id)
				return
			}
			policyRef = ref[:]
		}
		challengeRef, err := core.Digest(challenge)
		if err != nil {
			log.Printf("AUDIT DIGEST %v", err)
			return
		}
		evidenceRef := sha256.Sum256(job.Openings)
		signedVerdict := &pb.AuditVerdict{ChallengeRef: challengeRef, AuditorId: "local-reference", Distances: []float64{verdict.Distance}, Verdict: verdict.Verdict, EvidenceRef: evidenceRef[:], AuditPolicyRef: policyRef, FaultNodeIds: verdict.FaultNodeIDs}
		if err := core.Sign(signedVerdict, auditKey); err != nil {
			log.Printf("AUDIT SIGN %v", err)
			return
		}
		settled := false
		if job.Kind == "pipeline" {
			err = book.RecordPipelineAudit(id, challenge, signedVerdict)
		} else {
			err = book.RecordAudit(id, challenge, signedVerdict)
		}
		if err != nil {
			log.Printf("AUDIT RECORD %v", err)
			return
		}
		if verdict.Verdict == "accept" || verdict.Verdict == "reject" {
			if job.Kind == "pipeline" {
				err = book.FinishPipeline(id, verdict.Verdict, time.Now().Add(time.Second), slashMultiplier)
			} else {
				err = book.Finish(id, verdict.Verdict, time.Now().Add(time.Second), slashMultiplier)
			}
			if err != nil {
				log.Printf("SETTLE ERROR %s %v", id, err)
				return
			}
			settled = true
		}
		if verdict.Verdict == "accept" || verdict.Verdict == "reject" {
			_ = spool.Done(id)
		}
		tps := float64(outputTokens) * 1000 / float64(max(1, elapsedMS))
		charge, feeErr := core.TokenFee(relay.Genesis, inputTokens, outputTokens)
		if feeErr != nil {
			log.Printf("AUDIT FEE %s: %v", id, feeErr)
			return
		}
		log.Printf("AUDIT request=%s node=%s verdict=%s distance=%g device=%s ttft_ms=%d tok_s=%.2f settled_subunits=%d auditor_peak_rss_bytes=%d grade=%s", id, nodeLabel, verdict.Verdict, verdict.Distance, verdict.Device, job.TTFT, tps, charge.Total, verdict.PeakRSS, verdict.Grade)
		if job.Kind == "pipeline" && settled {
			if shares, shareErr := book.PipelineCredits(id); shareErr == nil {
				for _, share := range shares {
					log.Printf("PIPELINE SETTLEMENT request=%s node=%s work=%d amount=%d", id, share.NodeID, share.Work, share.Amount)
				}
			}
		}
	}

	// Admission and response handling never wait on reference replay. Pending
	// jobs are durable; bounded workers retry them without one global audit lock.
	permits := make(chan struct{}, *auditWorkers)
	var pendingMu sync.Mutex
	activeAudits := map[string]bool{}
	scheduleAudit := func(id string) {
		pendingMu.Lock()
		if activeAudits[id] {
			pendingMu.Unlock()
			return
		}
		select {
		case permits <- struct{}{}:
		default:
			pendingMu.Unlock()
			return
		}
		activeAudits[id] = true
		pendingMu.Unlock()
		go func() {
			defer func() { pendingMu.Lock(); delete(activeAudits, id); pendingMu.Unlock(); <-permits }()
			processAudit(id)
		}()
	}
	api.OnReceipt = func(receipt *pb.ExecutionReceipt, _ []byte, _ string, _ int64, _ int64) {
		scheduleAudit(receipt.RequestId)
	}
	api.OnPipelineReceipt = func(execution *gateway.PipelineExecution) {
		scheduleAudit(execution.Receipt.RequestId)
	}
	go func() {
		for {
			ids, err := spool.Pending()
			if err != nil {
				log.Printf("AUDIT SPOOL LIST: %v", err)
			} else {
				for _, id := range ids {
					scheduleAudit(id)
				}
			}
			time.Sleep(10 * time.Second)
		}
	}()
	var faucetServer *http.Server
	errCh := make(chan error, 2)
	mux := http.NewServeMux()
	mux.Handle("/v1/chat/completions", api)
	mux.Handle("/v1/quote", api)
	mux.Handle("/v1/pricing", api)
	if *faucetAmount > 0 {
		if *faucetState == "" {
			*faucetState = *database + ".faucet-state.json"
		}
		handler := &faucet.Handler{Transfer: chainBook.FaucetTransfer, Amount: *faucetAmount,
			Window: *faucetWindow, StatePath: *faucetState, TrustedProxy: *faucetTrustedProxy}
		mux.Handle("/testnet/faucet", handler)
		if *faucetHTTP != "" {
			faucetServer = &http.Server{Addr: *faucetHTTP, Handler: faucet.PublicMux(handler), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second}
			go func() {
				if *httpCert != "" {
					errCh <- faucetServer.ListenAndServeTLS(*httpCert, *httpKey)
				} else {
					errCh <- faucetServer.ListenAndServe()
				}
			}()
		}
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := book.Ping(); err != nil || relay.OnlineCount() == 0 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, api.Metrics())
		fmt.Fprintf(w, "ember_tunnel_online_nodes %d\n", relay.OnlineCount())
	})
	mux.HandleFunc("/seed/peers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || chainBook == nil {
			http.NotFound(w, r)
			return
		}
		profiles, queryErr := chainBook.NodeProfiles()
		if queryErr != nil {
			http.Error(w, queryErr.Error(), http.StatusServiceUnavailable)
			return
		}
		encoded := make([]string, 0, len(profiles))
		for _, profile := range profiles {
			raw, marshalErr := proto.MarshalOptions{Deterministic: true}.Marshal(profile)
			if marshalErr != nil {
				http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
				return
			}
			encoded = append(encoded, hex.EncodeToString(raw))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"profiles": encoded})
	})
	mux.HandleFunc("/demo/nodes", func(w http.ResponseWriter, r *http.Request) {
		if !*demo {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"honest": relay.Online("honest"), "cheat": relay.Online("cheat")})
	})
	mux.HandleFunc("/demo/balances", func(w http.ResponseWriter, r *http.Request) {
		if !*demo {
			http.NotFound(w, r)
			return
		}
		if r.Method != "GET" {
			http.Error(w, "GET only", 405)
			return
		}
		data := map[string]any{}
		for _, id := range []string{"consumer", "honest", "cheat"} {
			b, s, _ := book.Balance(id)
			data[id] = map[string]int64{"balance": b, "stake": s}
		}
		_ = json.NewEncoder(w).Encode(data)
	})
	log.Printf("emberd %s http=%s grpc=%s", mode, *httpAddr, *grpcAddr)
	httpServer := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: *requestTimeout + 30*time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		if *httpCert != "" {
			errCh <- httpServer.ListenAndServeTLS(*httpCert, *httpKey)
		} else {
			errCh <- httpServer.ListenAndServe()
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
		if faucetServer != nil {
			_ = faucetServer.Shutdown(shutdown)
		}
		stopped := make(chan struct{})
		go func() { server.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-shutdown.Done():
			server.Stop()
		}
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}
}

func loadAuditKey(path string) (ed25519.PrivateKey, error) {
	if raw, err := os.ReadFile(path); err == nil {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, statErr
		}
		if info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("audit key must not be group/world accessible")
		}
		if len(raw) != ed25519.PrivateKeySize {
			return nil, errors.New("invalid audit key length")
		}
		return ed25519.PrivateKey(raw), nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if _, err = f.Write(key); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return key, nil
}

func replaceEnv(environment []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func issueToken(args []string) {
	f := flag.NewFlagSet("token", flag.ExitOnError)
	db := f.String("db", "ember.db", "ledger")
	account := f.String("account", "", "new account name")
	fund := f.Int64("fund", 0, "centrally issued initial subunits")
	_ = f.Parse(args)
	b, e := ledger.Open(*db)
	if e != nil {
		log.Fatal(e)
	}
	defer b.Close()
	raw := make([]byte, 32)
	if _, e = rand.Read(raw); e != nil {
		log.Fatal(e)
	}
	token := hex.EncodeToString(raw)
	if e = b.IssueToken(*account, *fund, token); e != nil {
		log.Fatal(e)
	}
	fmt.Printf("account=%s token=%s\n", *account, token)
}

func generateCert(args []string) {
	f := flag.NewFlagSet("cert", flag.ExitOnError)
	cert := f.String("cert", "tunnel.crt", "certificate output")
	key := f.String("key", "tunnel.key", "private key output")
	host := f.String("host", "127.0.0.1", "comma-separated IP or DNS SAN")
	_ = f.Parse(args)
	priv, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		log.Fatal(e)
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		log.Fatal(e)
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "emberd tunnel"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	for _, h := range bytes.Split([]byte(*host), []byte(",")) {
		if ip := net.ParseIP(string(h)); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, string(h))
		}
	}
	der, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if e != nil {
		log.Fatal(e)
	}
	pk, e := x509.MarshalPKCS8PrivateKey(priv)
	if e != nil {
		log.Fatal(e)
	}
	if e = os.WriteFile(*cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); e != nil {
		log.Fatal(e)
	}
	if e = os.WriteFile(*key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0600); e != nil {
		log.Fatal(e)
	}
	h := sha256.Sum256(der)
	fmt.Printf("tls-fingerprint=%x\n", h)
}
