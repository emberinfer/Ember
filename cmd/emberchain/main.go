package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ember/services/chain"
	"ember/version"
	cmtcfg "github.com/cometbft/cometbft/config"
	cmtted25519 "github.com/cometbft/cometbft/crypto/ed25519"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/node"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/privval"
	"github.com/cometbft/cometbft/proxy"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	cmttypes "github.com/cometbft/cometbft/types"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "init-validator":
		err = initValidator(os.Args[2:])
	case "validator-public":
		err = validatorPublic(os.Args[2:])
	case "keygen":
		err = keygen(os.Args[2:])
	case "genesis":
		err = genesis(os.Args[2:])
	case "verify-genesis":
		err = verifyGenesis(os.Args[2:])
	case "start":
		err = start(os.Args[2:])
	case "account":
		err = account(os.Args[2:])
	case "tx":
		err = transact(os.Args[2:])
	case "query":
		err = query(os.Args[2:])
	case "version":
		fmt.Println(version.Version)
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: emberchain init-validator|validator-public|keygen|genesis|verify-genesis|start|account|tx|query|version")
	os.Exit(2)
}

type validatorDescriptor struct {
	Name             string `json:"name"`
	ValidatorAddress string `json:"validator_address"`
	ValidatorPubKey  string `json:"validator_public_key"`
	NodeID           string `json:"node_id"`
}

func initValidator(args []string) error {
	f := flag.NewFlagSet("init-validator", flag.ContinueOnError)
	home := f.String("home", "", "validator home")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *home == "" {
		return errors.New("--home required")
	}
	cmtcfg.EnsureRoot(*home)
	cfg := cmtcfg.DefaultConfig().SetRoot(*home)
	pv, err := privval.GenFilePV(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile(), nil)
	if err != nil {
		return err
	}
	pv.Save()
	nk, err := p2p.LoadOrGenNodeKey(cfg.NodeKeyFile())
	if err != nil {
		return err
	}
	for _, path := range []string{cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile(), cfg.NodeKeyFile(), filepath.Join(*home, "config", "config.toml")} {
		if err = os.Chmod(path, 0600); err != nil {
			return err
		}
	}
	pub, err := pv.GetPubKey()
	if err != nil {
		return err
	}
	fmt.Printf("validator_address=%X validator_pubkey=%X node_id=%s\n", pub.Address(), pub.Bytes(), p2p.PubKeyToID(nk.PubKey()))
	return nil
}

func validatorPublic(args []string) error {
	f := flag.NewFlagSet("validator-public", flag.ContinueOnError)
	home := f.String("home", "", "validator home")
	name := f.String("name", "", "operator-visible validator name")
	out := f.String("out", "", "public descriptor output")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *home == "" || *name == "" || *out == "" {
		return errors.New("--home, --name and --out required")
	}
	descriptor, err := descriptorFromHome(*home, *name)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err = writePublicExclusive(*out, raw); err != nil {
		return err
	}
	sum := sha256Bytes(raw)
	fmt.Printf("validator=%s node_id=%s descriptor_sha256=%x\n", descriptor.Name, descriptor.NodeID, sum)
	return nil
}

func keygen(args []string) error {
	f := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := f.String("out", "", "private key output")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out required")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err = writeExclusive(*out, key); err != nil {
		return err
	}
	fmt.Printf("account=%s public_key=%x\n", chain.AccountID(key.Public().(ed25519.PublicKey)), key.Public())
	return nil
}

func genesis(args []string) error {
	f := flag.NewFlagSet("genesis", flag.ContinueOnError)
	out := f.String("out", "", "genesis output")
	chainID := f.String("chain-id", "", "chain ID")
	appState := f.String("app-state", "", "Ember genesis state JSON")
	validators := f.String("validators", "", "comma-separated validator homes")
	validatorPublicFiles := f.String("validator-public", "", "comma-separated public validator descriptors")
	names := f.String("names", "", "comma-separated validator names")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *out == "" || *chainID == "" || *appState == "" || (*validators == "") == (*validatorPublicFiles == "") {
		return errors.New("--out, --chain-id, --app-state and exactly one of --validators or --validator-public required")
	}
	raw, err := os.ReadFile(*appState)
	if err != nil {
		return err
	}
	var appGenesis chain.GenesisState
	if json.Unmarshal(raw, &appGenesis) != nil || appGenesis.Parameters.ChainID != *chainID {
		return errors.New("invalid app state or chain ID mismatch")
	}
	if _, err = chain.NewState(appGenesis); err != nil {
		return err
	}
	doc := &cmttypes.GenesisDoc{ChainID: *chainID, GenesisTime: time.Now().UTC(), AppState: raw}
	if *validatorPublicFiles != "" {
		descriptors, loadErr := loadValidatorDescriptors(*validatorPublicFiles)
		if loadErr != nil {
			return loadErr
		}
		for _, descriptor := range descriptors {
			validator, validatorErr := descriptorGenesisValidator(descriptor)
			if validatorErr != nil {
				return validatorErr
			}
			doc.Validators = append(doc.Validators, validator)
		}
	} else {
		homes, labels := strings.Split(*validators, ","), strings.Split(*names, ",")
		for i, home := range homes {
			name := fmt.Sprintf("validator-%d", i+1)
			if i < len(labels) && labels[i] != "" {
				name = labels[i]
			}
			descriptor, descriptorErr := descriptorFromHome(home, name)
			if descriptorErr != nil {
				return descriptorErr
			}
			validator, validatorErr := descriptorGenesisValidator(descriptor)
			if validatorErr != nil {
				return validatorErr
			}
			doc.Validators = append(doc.Validators, validator)
		}
	}
	if err = doc.ValidateAndComplete(); err != nil {
		return err
	}
	if err = doc.SaveAs(*out); err != nil {
		return err
	}
	return os.Chmod(*out, 0600)
}

func verifyGenesis(args []string) error {
	f := flag.NewFlagSet("verify-genesis", flag.ContinueOnError)
	genesisPath := f.String("genesis", "", "assembled CometBFT genesis")
	appStatePath := f.String("app-state", "", "reviewed Ember app state")
	validatorPublicFiles := f.String("validator-public", "", "comma-separated public validator descriptors")
	localHome := f.String("local-home", "", "optional local validator home that must match one descriptor")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *genesisPath == "" || *appStatePath == "" || *validatorPublicFiles == "" {
		return errors.New("--genesis, --app-state and --validator-public required")
	}
	doc, err := cmttypes.GenesisDocFromFile(*genesisPath)
	if err != nil {
		return err
	}
	appRaw, err := os.ReadFile(*appStatePath)
	if err != nil {
		return err
	}
	var expectedState, actualState chain.GenesisState
	if json.Unmarshal(appRaw, &expectedState) != nil || json.Unmarshal(doc.AppState, &actualState) != nil {
		return errors.New("invalid expected or assembled app state")
	}
	expectedJSON, _ := json.Marshal(expectedState)
	actualJSON, _ := json.Marshal(actualState)
	if !bytes.Equal(expectedJSON, actualJSON) || expectedState.Parameters.ChainID != doc.ChainID {
		return errors.New("assembled genesis app state differs from reviewed input")
	}
	descriptors, err := loadValidatorDescriptors(*validatorPublicFiles)
	if err != nil {
		return err
	}
	if len(doc.Validators) != len(descriptors) {
		return errors.New("assembled validator count differs from public set")
	}
	for i, descriptor := range descriptors {
		expected, descriptorErr := descriptorGenesisValidator(descriptor)
		if descriptorErr != nil {
			return descriptorErr
		}
		actual := doc.Validators[i]
		if actual.Name != expected.Name || actual.Power != expected.Power ||
			!bytes.Equal(actual.Address, expected.Address) || !bytes.Equal(actual.PubKey.Bytes(), expected.PubKey.Bytes()) {
			return fmt.Errorf("assembled validator %d differs from descriptor %s", i+1, descriptor.Name)
		}
	}
	if *localHome != "" {
		matched := false
		for _, descriptor := range descriptors {
			local, localErr := descriptorFromHome(*localHome, descriptor.Name)
			if localErr != nil {
				return localErr
			}
			if local.ValidatorAddress == descriptor.ValidatorAddress && local.ValidatorPubKey == descriptor.ValidatorPubKey && local.NodeID == descriptor.NodeID {
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("local validator keys are absent from the reviewed public set")
		}
	}
	raw, err := os.ReadFile(*genesisPath)
	if err != nil {
		return err
	}
	fmt.Printf("chain_id=%s validators=%d genesis_sha256=%x result=PASS\n", doc.ChainID, len(doc.Validators), sha256Bytes(raw))
	return nil
}

func descriptorFromHome(home, name string) (validatorDescriptor, error) {
	cfg := cmtcfg.DefaultConfig().SetRoot(home)
	pv := privval.LoadFilePV(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile())
	pub, err := pv.GetPubKey()
	if err != nil {
		return validatorDescriptor{}, err
	}
	nodeKey, err := p2p.LoadNodeKey(cfg.NodeKeyFile())
	if err != nil {
		return validatorDescriptor{}, err
	}
	return validatorDescriptor{Name: name, ValidatorAddress: strings.ToUpper(hex.EncodeToString(pub.Address())),
		ValidatorPubKey: strings.ToUpper(hex.EncodeToString(pub.Bytes())), NodeID: string(p2p.PubKeyToID(nodeKey.PubKey()))}, nil
}

func loadValidatorDescriptors(paths string) ([]validatorDescriptor, error) {
	files := strings.Split(paths, ",")
	if len(files) == 0 {
		return nil, errors.New("empty public validator set")
	}
	seenNames, seenValidators, seenNodes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	descriptors := make([]validatorDescriptor, 0, len(files))
	for _, path := range files {
		raw, err := os.ReadFile(strings.TrimSpace(path))
		if err != nil {
			return nil, err
		}
		var descriptor validatorDescriptor
		if json.Unmarshal(raw, &descriptor) != nil {
			return nil, fmt.Errorf("invalid validator descriptor %s", path)
		}
		if _, err = descriptorGenesisValidator(descriptor); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if seenNames[descriptor.Name] || seenValidators[descriptor.ValidatorPubKey] || seenNodes[descriptor.NodeID] {
			return nil, fmt.Errorf("duplicate validator descriptor identity in %s", path)
		}
		seenNames[descriptor.Name], seenValidators[descriptor.ValidatorPubKey], seenNodes[descriptor.NodeID] = true, true, true
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, nil
}

func descriptorGenesisValidator(descriptor validatorDescriptor) (cmttypes.GenesisValidator, error) {
	if descriptor.Name == "" || strings.ContainsAny(descriptor.Name, "\r\n") {
		return cmttypes.GenesisValidator{}, errors.New("invalid validator name")
	}
	pubRaw, err := hex.DecodeString(descriptor.ValidatorPubKey)
	if err != nil || len(pubRaw) != cmtted25519.PubKeySize {
		return cmttypes.GenesisValidator{}, errors.New("invalid validator public key")
	}
	address, err := hex.DecodeString(descriptor.ValidatorAddress)
	if err != nil {
		return cmttypes.GenesisValidator{}, errors.New("invalid validator address")
	}
	pub := cmtted25519.PubKey(pubRaw)
	if !bytes.Equal(address, pub.Address()) {
		return cmttypes.GenesisValidator{}, errors.New("validator address does not match public key")
	}
	nodeID, err := hex.DecodeString(descriptor.NodeID)
	if err != nil || len(nodeID) != 20 {
		return cmttypes.GenesisValidator{}, errors.New("invalid node ID")
	}
	return cmttypes.GenesisValidator{Address: pub.Address(), PubKey: pub, Power: 10, Name: descriptor.Name}, nil
}

func sha256Bytes(raw []byte) [32]byte {
	return sha256.Sum256(raw)
}

func start(args []string) error {
	f := flag.NewFlagSet("start", flag.ContinueOnError)
	home := f.String("home", "", "validator home")
	p2pAddr := f.String("p2p", "", "P2P listen URI")
	rpcAddr := f.String("rpc", "", "RPC listen URI")
	peers := f.String("peers", "", "persistent peers")
	metricsAddr := f.String("metrics", "", "optional Prometheus listen host:port")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *home == "" || *p2pAddr == "" || *rpcAddr == "" {
		return errors.New("--home, --p2p and --rpc required")
	}
	cfg := cmtcfg.DefaultConfig().SetRoot(*home)
	cfg.P2P.ListenAddress = *p2pAddr
	cfg.RPC.ListenAddress = *rpcAddr
	cfg.P2P.PersistentPeers = *peers
	cfg.P2P.AllowDuplicateIP = true
	cfg.P2P.AddrBookStrict = false
	cfg.RPC.CORSAllowedOrigins = []string{}
	if *metricsAddr != "" {
		cfg.Instrumentation.Prometheus = true
		cfg.Instrumentation.PrometheusListenAddr = *metricsAddr
	}
	app, err := chain.OpenApplication(chain.StatePath(*home))
	if err != nil {
		return err
	}
	pv := privval.LoadFilePV(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile())
	nodeKey, err := p2p.LoadNodeKey(cfg.NodeKeyFile())
	if err != nil {
		return err
	}
	logger := cmtlog.NewTMLogger(cmtlog.NewSyncWriter(os.Stdout))
	if level, levelErr := cmtlog.AllowLevel("info"); levelErr == nil {
		logger = cmtlog.NewFilter(logger, level)
	}
	n, err := node.NewNode(context.Background(), cfg, pv, nodeKey, proxy.NewLocalClientCreator(app), node.DefaultGenesisDocProviderFunc(cfg), cmtcfg.DefaultDBProvider, node.DefaultMetricsProvider(cfg.Instrumentation), logger)
	if err != nil {
		return err
	}
	if err = n.Start(); err != nil {
		return err
	}
	log.Printf("emberchain started home=%s p2p=%s rpc=%s", *home, *p2pAddr, *rpcAddr)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return n.Stop()
}

func account(args []string) error {
	f := flag.NewFlagSet("account", flag.ContinueOnError)
	keyPath := f.String("key", "", "private key")
	if err := f.Parse(args); err != nil {
		return err
	}
	key, err := readKey(*keyPath)
	if err != nil {
		return err
	}
	fmt.Println(chain.AccountID(key.Public().(ed25519.PublicKey)))
	return nil
}

func transact(args []string) error {
	f := flag.NewFlagSet("tx", flag.ContinueOnError)
	rpc := f.String("rpc", "", "RPC URL")
	keyPath := f.String("key", "", "private key")
	chainID := f.String("chain-id", "", "chain ID")
	nonce := f.Uint64("nonce", 0, "next account nonce")
	actionJSON := f.String("action", "", "action JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *rpc == "" || *chainID == "" || *nonce == 0 || *actionJSON == "" {
		return errors.New("--rpc, --key, --chain-id, --nonce and --action required")
	}
	key, err := readKey(*keyPath)
	if err != nil {
		return err
	}
	var action chain.Action
	if err = json.Unmarshal([]byte(*actionJSON), &action); err != nil {
		return err
	}
	raw, err := chain.Sign(*chainID, key, *nonce, action)
	if err != nil {
		return err
	}
	client, err := rpchttp.New(*rpc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := client.BroadcastTxCommit(ctx, raw)
	if err != nil {
		return err
	}
	if result.CheckTx.Code != 0 {
		return fmt.Errorf("CheckTx code=%d log=%s", result.CheckTx.Code, result.CheckTx.Log)
	}
	if result.TxResult.Code != 0 {
		return fmt.Errorf("DeliverTx code=%d log=%s", result.TxResult.Code, result.TxResult.Log)
	}
	fmt.Printf("height=%d tx_hash=%X\n", result.Height, result.Hash)
	return nil
}

func query(args []string) error {
	f := flag.NewFlagSet("query", flag.ContinueOnError)
	rpc := f.String("rpc", "", "RPC URL")
	path := f.String("path", "/state", "query path")
	verify := f.Bool("verify-state", false, "verify whole-state hash against a genesis-anchored CometBFT commit")
	genesisPath := f.String("genesis", "", "mode-0600 CometBFT genesis file for state verification")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *rpc == "" {
		return errors.New("--rpc required")
	}
	client, err := rpchttp.New(*rpc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := client.ABCIQuery(ctx, *path, nil)
	if err != nil {
		return err
	}
	if result.Response.Code != 0 {
		return fmt.Errorf("query code=%d log=%s", result.Response.Code, result.Response.Log)
	}
	if *verify {
		if *path != "/state" {
			return errors.New("--verify-state requires --path /state")
		}
		if *genesisPath == "" {
			return errors.New("--verify-state requires --genesis")
		}
		genesisInfo, statErr := os.Stat(*genesisPath)
		if statErr != nil {
			return statErr
		}
		if genesisInfo.Mode().Perm()&0077 != 0 {
			return errors.New("verification genesis must be mode 0600")
		}
		genesis, genesisErr := cmttypes.GenesisDocFromFile(*genesisPath)
		if genesisErr != nil {
			return genesisErr
		}
		var state chain.State
		if err = json.Unmarshal(result.Response.Value, &state); err != nil {
			return err
		}
		hash, hashErr := state.Hash()
		if hashErr != nil {
			return hashErr
		}
		stateHeight := result.Response.Height
		headerHeight := stateHeight + 1
		var commitErr error
		var signed *cmttypes.SignedHeader
		for attempt := 0; attempt < 30; attempt++ {
			commit, err := client.Commit(ctx, &headerHeight)
			if err == nil {
				signed = &commit.SignedHeader
				break
			}
			commitErr = err
			time.Sleep(200 * time.Millisecond)
		}
		if signed == nil {
			return commitErr
		}
		if err := signed.ValidateBasic(genesis.ChainID); err != nil {
			return err
		}
		if !bytes.Equal(genesis.ValidatorHash(), signed.Header.ValidatorsHash) {
			return errors.New("header validator set differs from genesis")
		}
		validators := make([]*cmttypes.Validator, len(genesis.Validators))
		for i, validator := range genesis.Validators {
			validators[i] = cmttypes.NewValidator(validator.PubKey, validator.Power)
		}
		validatorSet := cmttypes.NewValidatorSet(validators)
		if err := validatorSet.VerifyCommit(genesis.ChainID, signed.Commit.BlockID, headerHeight, signed.Commit); err != nil {
			return err
		}
		if !bytes.Equal(hash, signed.Header.AppHash) {
			return fmt.Errorf("state hash %x != committed app hash %x", hash, signed.Header.AppHash)
		}
		fmt.Printf("state_height=%d proof_header_height=%d app_hash=%x proof=whole-state commit=genesis-verified\n", stateHeight, headerHeight, hash)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, result.Response.Value, "", "  ") == nil {
		fmt.Println(pretty.String())
	} else {
		fmt.Println(string(result.Response.Value))
	}
	return nil
}

func readKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("--key required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("key must be mode 0600")
	}
	if len(raw) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(raw), nil
	}
	if len(raw) != ed25519.PrivateKeySize {
		if decoded, decodeErr := hex.DecodeString(strings.TrimSpace(string(raw))); decodeErr == nil {
			raw = decoded
		}
	}
	if len(raw) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(raw), nil
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	return ed25519.PrivateKey(raw), nil
}

func writeExclusive(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func writePublicExclusive(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
