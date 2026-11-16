package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ember/services/chain"
	cmtcfg "github.com/cometbft/cometbft/config"
)

func TestPublicGenesisCeremony(t *testing.T) {
	root := t.TempDir()
	homes := []string{filepath.Join(root, "v1"), filepath.Join(root, "v2"), filepath.Join(root, "v3")}
	descriptors := make([]string, len(homes))
	for i, home := range homes {
		if err := initValidator([]string{"--home", home}); err != nil {
			t.Fatal(err)
		}
		descriptors[i] = filepath.Join(root, "v"+string(rune('1'+i))+".json")
		if err := validatorPublic([]string{"--home", home, "--name", "operator-" + string(rune('a'+i)), "--out", descriptors[i]}); err != nil {
			t.Fatal(err)
		}
	}
	app := chain.GenesisState{Parameters: chain.Parameters{ChainID: "ember-ceremony-test", SubsidyCap: 0,
		UnbondingBlocks: 1, SlashMultiplier: 20, MinNodeStake: 1, HoldTimeout: 2, RegistrationTTL: 2}}
	appRaw, err := json.Marshal(app)
	if err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(root, "app-state.json")
	if err = os.WriteFile(appPath, appRaw, 0600); err != nil {
		t.Fatal(err)
	}
	genesisPath := filepath.Join(root, "genesis.json")
	publicSet := strings.Join(descriptors, ",")
	if err = genesis([]string{"--out", genesisPath, "--chain-id", "ember-ceremony-test", "--app-state", appPath, "--validator-public", publicSet}); err != nil {
		t.Fatal(err)
	}
	for _, home := range homes {
		if err = verifyGenesis([]string{"--genesis", genesisPath, "--app-state", appPath, "--validator-public", publicSet, "--local-home", home}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDescriptorRejectsAddressSubstitution(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "validator")
	if err := initValidator([]string{"--home", home}); err != nil {
		t.Fatal(err)
	}
	descriptor, err := descriptorFromHome(home, "operator")
	if err != nil {
		t.Fatal(err)
	}
	descriptor.ValidatorAddress = strings.Repeat("0", len(descriptor.ValidatorAddress))
	if _, err = descriptorGenesisValidator(descriptor); err == nil {
		t.Fatal("substituted validator address accepted")
	}
}

func TestIssuanceGenesisConsensusTimestamp(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "validator")
	if err := initValidator([]string{"--home", home}); err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(root, "validator.json")
	if err := validatorPublic([]string{"--home", home, "--name", "local-validator", "--out", descriptor}); err != nil {
		t.Fatal(err)
	}
	governors := []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64), strings.Repeat("5", 64)}
	app := chain.GenesisState{Parameters: chain.Parameters{ChainID: "ember-issuance-ceremony", UnbondingBlocks: 1, SlashMultiplier: 20, MinNodeStake: 1, HoldTimeout: 2, RegistrationTTL: 2,
		GenesisIssuance: &chain.IssuanceParameters{StandardPrice: 1000000, PriceRef: "published-price", OrganicForecast: 2 * chain.PoolEquivalents, ForecastRef: "published-forecast", GenesisTime: 1767225600, GovernorThreshold: 3, GovernorSigners: governors, FoundingEntities: []string{"founder"}}}, Governors: governors}
	raw, _ := json.Marshal(app)
	appPath := filepath.Join(root, "app.json")
	if err := os.WriteFile(appPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "genesis.json")
	if err := genesis([]string{"--out", out, "--chain-id", app.Parameters.ChainID, "--app-state", appPath, "--validator-public", descriptor}); err != nil {
		t.Fatal(err)
	}
	args := []string{"--genesis", out, "--app-state", appPath, "--validator-public", descriptor}
	if err := verifyGenesis(args); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var assembled map[string]any
	if err = json.Unmarshal(content, &assembled); err != nil {
		t.Fatal(err)
	}
	assembled["genesis_time"] = "2026-01-02T00:00:00Z"
	content, _ = json.Marshal(assembled)
	if err = os.WriteFile(out, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifyGenesis(args); err == nil {
		t.Fatal("mismatched consensus timestamp accepted")
	}
	// The recomputation command also validates the exact genesis configuration.
	if err = retrospective([]string{"--app-state", appPath}); err != nil {
		t.Fatal(err)
	}
}

func writeConfigFile(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadWhitelistedTimeoutsAppliesWhitelist(t *testing.T) {
	home := t.TempDir()
	writeConfigFile(t, home, "[consensus]\ntimeout_propose = \"30s\"\ntimeout_propose_delta = \"2s\"\ntimeout_prevote = \"3s\"\ntimeout_commit = \"2s\"\n[rpc]\ntimeout_broadcast_tx_commit = \"30s\"\n")
	cfg := cmtcfg.DefaultConfig().SetRoot(home)
	applyWhitelistedConfigFile(cfg, home)
	if cfg.Consensus.TimeoutPropose != 30*time.Second {
		t.Fatalf("TimeoutPropose=%v want 30s", cfg.Consensus.TimeoutPropose)
	}
	if cfg.Consensus.TimeoutProposeDelta != 2*time.Second {
		t.Fatalf("TimeoutProposeDelta=%v want 2s", cfg.Consensus.TimeoutProposeDelta)
	}
	if cfg.Consensus.TimeoutPrevote != 3*time.Second {
		t.Fatalf("TimeoutPrevote=%v want 3s", cfg.Consensus.TimeoutPrevote)
	}
	if cfg.Consensus.TimeoutCommit != 2*time.Second {
		t.Fatalf("TimeoutCommit=%v want 2s", cfg.Consensus.TimeoutCommit)
	}
	if cfg.RPC.TimeoutBroadcastTxCommit != 30*time.Second {
		t.Fatalf("TimeoutBroadcastTxCommit=%v want 30s", cfg.RPC.TimeoutBroadcastTxCommit)
	}
	// Absent whitelist fields keep defaults.
	if cfg.Consensus.TimeoutPrecommit != time.Second {
		t.Fatalf("TimeoutPrecommit=%v want default 1s", cfg.Consensus.TimeoutPrecommit)
	}
	if cfg.Consensus.TimeoutPrevoteDelta != 500*time.Millisecond {
		t.Fatalf("TimeoutPrevoteDelta=%v want default 500ms", cfg.Consensus.TimeoutPrevoteDelta)
	}
}

func TestLoadWhitelistedTimeoutsIgnoresNonWhitelist(t *testing.T) {
	home := t.TempDir()
	writeConfigFile(t, home, "moniker = \"evil\"\nlog_level = \"debug\"\n[rpc]\nladdr = \"tcp://0.0.0.0:9999\"\nunsafe = true\ntimeout_broadcast_tx_commit = \"30s\"\n[p2p]\nladdr = \"tcp://0.0.0.0:9998\"\nallow_duplicate_ip = false\naddr_book_strict = true\npersistent_peers = \"deadbeef@127.0.0.1:9997\"\n[consensus]\ncreate_empty_blocks = false\ntimeout_propose = \"30s\"\n[mempool]\nsize = 1\n")
	cfg := cmtcfg.DefaultConfig().SetRoot(home)
	defaultP2P := cfg.P2P.ListenAddress
	defaultRPC := cfg.RPC.ListenAddress
	applyWhitelistedConfigFile(cfg, home)
	if cfg.Consensus.TimeoutPropose != 30*time.Second {
		t.Fatalf("TimeoutPropose=%v want 30s", cfg.Consensus.TimeoutPropose)
	}
	if cfg.RPC.TimeoutBroadcastTxCommit != 30*time.Second {
		t.Fatalf("TimeoutBroadcastTxCommit=%v want 30s", cfg.RPC.TimeoutBroadcastTxCommit)
	}
	if cfg.P2P.ListenAddress != defaultP2P {
		t.Fatalf("P2P.ListenAddress changed to %q", cfg.P2P.ListenAddress)
	}
	if cfg.RPC.ListenAddress != defaultRPC {
		t.Fatalf("RPC.ListenAddress changed to %q", cfg.RPC.ListenAddress)
	}
	if cfg.P2P.PersistentPeers != "" {
		t.Fatalf("PersistentPeers changed to %q", cfg.P2P.PersistentPeers)
	}
	if !cfg.Consensus.CreateEmptyBlocks {
		t.Fatal("CreateEmptyBlocks changed by non-whitelist key")
	}
	if cfg.Mempool.Size != 5000 {
		t.Fatalf("Mempool.Size changed to %d", cfg.Mempool.Size)
	}
	if cfg.RPC.Unsafe {
		t.Fatal("RPC.Unsafe changed by non-whitelist key")
	}
}

func TestLoadWhitelistedTimeoutsMissingFileKeepsDefaults(t *testing.T) {
	home := t.TempDir()
	cfg := cmtcfg.DefaultConfig().SetRoot(home)
	applyWhitelistedConfigFile(cfg, home)
	if cfg.Consensus.TimeoutPropose != 3*time.Second || cfg.Consensus.TimeoutProposeDelta != 500*time.Millisecond {
		t.Fatalf("missing file changed propose timeouts: %v/%v", cfg.Consensus.TimeoutPropose, cfg.Consensus.TimeoutProposeDelta)
	}
	if cfg.RPC.TimeoutBroadcastTxCommit != 10*time.Second {
		t.Fatalf("missing file changed broadcast timeout: %v", cfg.RPC.TimeoutBroadcastTxCommit)
	}
}

func TestLoadWhitelistedTimeoutsBadDurationKeepsDefaults(t *testing.T) {
	home := t.TempDir()
	writeConfigFile(t, home, "[consensus]\ntimeout_propose = \"30s\"\ntimeout_prevote = \"not-a-duration\"\n")
	cfg := cmtcfg.DefaultConfig().SetRoot(home)
	applyWhitelistedConfigFile(cfg, home)
	if cfg.Consensus.TimeoutPropose != 3*time.Second {
		t.Fatalf("bad file half-applied: TimeoutPropose=%v want default 3s", cfg.Consensus.TimeoutPropose)
	}
	if cfg.Consensus.TimeoutPrevote != time.Second {
		t.Fatalf("bad file changed TimeoutPrevote=%v", cfg.Consensus.TimeoutPrevote)
	}
}
