package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ember/services/chain"
	abci "github.com/cometbft/cometbft/abci/types"
	cmtcfg "github.com/cometbft/cometbft/config"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
)

func TestNormalizeChunkFetchers(t *testing.T) {
	for _, body := range []string{
		"[other]\nchunk_fetchers = \"3\"\n[statesync] # keep\r\n\tchunk_fetchers = \"4\" # keep\r\n[next]\nchunk_fetchers = \"5\"",
		"[statesync]\nenable = false\n",
		"[statesync]\nchunk_fetchers = 4\n",
		"[statesync]\nchunk_fetchers = \"x\"\n",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := normalizeChunkFetchers(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := normalizeChunkFetchers(path); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if want := strings.Replace(body, `chunk_fetchers = "4"`, `chunk_fetchers = 4`, 1); err != nil || string(raw) != want {
			t.Fatalf("got %q, want %q: %v", raw, want, err)
		}
	}
}

func TestLoadChunkFetchers(t *testing.T) {
	for _, value := range []string{`"4"`, `'4'`, `4`, `"x"`} {
		home := t.TempDir()
		writeConfigFile(t, home, "[statesync]\nchunk_fetchers = "+value+"\n")
		cfg := cmtcfg.DefaultConfig()
		_, _, _, err := loadSyncConfig(cfg, filepath.Join(home, "config", "config.toml"))
		if value == `"x"` {
			if err == nil {
				t.Fatal("accepted nonnumeric chunk_fetchers")
			}
		} else if err != nil || cfg.StateSync.ChunkFetchers != 4 {
			t.Fatalf("value %s: got %d, %v", value, cfg.StateSync.ChunkFetchers, err)
		}
	}
}

func TestInitValidatorConfigLoads(t *testing.T) {
	home := t.TempDir()
	if err := initValidator([]string{"--home", home}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config", "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(raw, []byte("\nchunk_fetchers = 4\n")) {
		t.Fatalf("generated config lacks an unquoted chunk_fetchers: %v", err)
	}
	cfg := cmtcfg.DefaultConfig()
	if _, _, _, err := loadSyncConfig(cfg, path); err != nil || cfg.StateSync.ChunkFetchers != 4 {
		t.Fatalf("loading generated config: got %d, %v", cfg.StateSync.ChunkFetchers, err)
	}
}

func TestSyncConfigurationFailClosed(t *testing.T) {
	for _, body := range []string{
		"[statesync]\nenable = nonsense",
		"[statesync]\nenable = true\nrpc_servers = \"http://one,http://one\"\ntrust_height=1\ntrust_hash=\"" + strings.Repeat("a", 64) + "\"",
		"[statesync]\nenable=true\ntrust_height=0",
		"[statesync]\ntrust_period=\"oops\"",
		"[snapshots]\ninterval=-1",
		"[snapshots]\ninclude_archive=oops",
		"[snapshots]\nkeep_recent=0",
		"[statesync]\nunknown_switch=true",
	} {
		home := t.TempDir()
		writeConfigFile(t, home, body)
		cfg := cmtcfg.DefaultConfig()
		if _, _, _, e := loadSyncConfig(cfg, filepath.Join(home, "config/config.toml")); e == nil {
			t.Fatalf("accepted %q", body)
		}
		if cfg.StateSync.Enable {
			t.Fatal("partially applied")
		}
	}
	home := t.TempDir()
	writeConfigFile(t, home, "[statesync]\nenable=false\n[snapshots]\ninterval=10\nkeep_recent=3\n")
	cfg := cmtcfg.DefaultConfig()
	interval, keep, included, e := loadSyncConfig(cfg, filepath.Join(home, "config/config.toml"))
	if !included || e != nil || interval != 10 || keep != 3 || cfg.StateSync.Enable {
		t.Fatal(interval, keep, e)
	}
	writeConfigFile(t, home, "[snapshots]\ninclude_archive=false\n")
	_, _, included, e = loadSyncConfig(cfg, filepath.Join(home, "config/config.toml"))
	if e != nil || included {
		t.Fatal("archive exclusion config", e)
	}

}

// Real CometBFT P2P snapshot transfer, with two distinct local RPC processes.
// No external network, production keys or production genesis are used.
func TestLocalNetworkStateSync(t *testing.T) {
	for _, included := range []bool{true, false} {
		t.Run(fmt.Sprintf("include_archive=%t", included), func(t *testing.T) { localNetworkStateSync(t, included) })
	}
}

func localNetworkStateSync(t *testing.T, included bool) {
	if testing.Short() {
		t.Skip("real network drill")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "emberchain")
	build := exec.Command("go", "build", "-o", bin, ".")
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	homes := []string{filepath.Join(root, "donor"), filepath.Join(root, "witness"), filepath.Join(root, "receiver")}
	ports := func() string {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer l.Close()
		return l.Addr().String()
	}
	p2pAddrs := []string{ports(), ports(), ports()}
	rpcAddrs := []string{ports(), ports(), ports()}
	for _, home := range homes {
		if e := initValidator([]string{"--home", home}); e != nil {
			t.Fatal(e)
		}
	}
	g := chain.GenesisState{Parameters: chain.Parameters{ChainID: "s3-local-only", UnbondingBlocks: 1, SlashMultiplier: 20, MinNodeStake: 1, HoldTimeout: 2, RegistrationTTL: 2}}
	appJSON, _ := json.Marshal(g)
	appFile := filepath.Join(root, "app.json")
	os.WriteFile(appFile, appJSON, 0600)
	genFile := filepath.Join(root, "genesis.json")
	if e := genesis([]string{"--out", genFile, "--chain-id", g.Parameters.ChainID, "--app-state", appFile, "--validators", homes[0]}); e != nil {
		t.Fatal(e)
	}
	genesisRaw, _ := os.ReadFile(genFile)
	desc, e := descriptorFromHome(homes[0], "donor")
	if e != nil {
		t.Fatal(e)
	}
	desc2, e := descriptorFromHome(homes[1], "witness")
	if e != nil {
		t.Fatal(e)
	}
	peers := desc.NodeID + "@" + p2pAddrs[0] + "," + desc2.NodeID + "@" + p2pAddrs[1]
	for _, home := range homes {
		if e = os.WriteFile(filepath.Join(home, "config/genesis.json"), genesisRaw, 0600); e != nil {
			t.Fatal(e)
		}
		writeConfigFile(t, home, fmt.Sprintf("[consensus]\ntimeout_commit=\"150ms\"\ntimeout_propose=\"500ms\"\n[snapshots]\ninterval=5\nkeep_recent=20\ninclude_archive=%t\n", included))
	}
	startNode := func(i int) {
		t.Helper()
		f, e := os.Create(filepath.Join(root, fmt.Sprintf("node-%d.log", i)))
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(bin, "start", "--home", homes[i], "--p2p", "tcp://"+p2pAddrs[i], "--rpc", "tcp://"+rpcAddrs[i], "--peers", peers)
		cmd.Stdout = f
		cmd.Stderr = f
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cmd.Process.Kill()
				<-done
			}
			f.Close()
			raw, _ := os.ReadFile(f.Name())
			if t.Failed() {
				t.Logf("node %d:\n%s", i, raw)
			}
			if dest := os.Getenv("S3_TEST_OUTPUT"); dest != "" {
				os.MkdirAll(dest, 0700)
				os.WriteFile(filepath.Join(dest, fmt.Sprintf("node-%d.log", i)), raw, 0600)
			}
		})
	}
	clients := make([]*rpchttp.HTTP, 3)
	for i := range clients {
		clients[i], e = rpchttp.New("http://" + rpcAddrs[i])
		if e != nil {
			t.Fatal(e)
		}
	}
	waitHeight := func(i int, height int64) int64 {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			r, e := clients[i].Status(ctx)
			cancel()
			if e == nil && r.SyncInfo.LatestBlockHeight >= height {
				return r.SyncInfo.LatestBlockHeight
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("node %d did not reach %d", i, height)
		return 0
	}
	startNode(0)
	waitHeight(0, 8)
	startNode(1)
	waitHeight(1, 15)
	archiveEntries, err := os.ReadDir(chain.StatePath(homes[0]) + ".archive")
	if err != nil || len(archiveEntries) == 0 {
		t.Fatal("donor has no real archive before transfer", err)
	}
	t.Logf("donor has %d real archive files before state sync", len(archiveEntries))
	trustHeight := int64(10)
	commit, e := clients[0].Commit(context.Background(), &trustHeight)
	if e != nil {
		t.Fatal(e)
	}
	cfg := cmtcfg.DefaultStateSyncConfig()
	cfg.Enable = true
	cfg.RPCServers = []string{"http://" + rpcAddrs[0], "http://" + rpcAddrs[1]}
	cfg.TrustHeight = trustHeight
	cfg.TrustHash = fmt.Sprintf("%X", commit.SignedHeader.Header.Hash())
	cfg.TrustPeriod = time.Hour
	if e = validateSyncAnchor(context.Background(), cfg, g.Parameters.ChainID); e != nil {
		t.Fatal(e)
	}
	correct := cfg.TrustHash
	cfg.TrustHash = strings.Repeat("0", 64)
	if e = validateSyncAnchor(context.Background(), cfg, g.Parameters.ChainID); e == nil {
		t.Fatal("wrong trust hash accepted")
	}
	cfg.TrustHash = correct
	cfg.TrustPeriod = time.Nanosecond
	if e = validateSyncAnchor(context.Background(), cfg, g.Parameters.ChainID); e == nil {
		t.Fatal("expired trust anchor accepted")
	}
	cfg.TrustPeriod = time.Hour
	t.Log("wrong trust hash and expired anchor rejected before node startup")
	writeConfigFile(t, homes[2], fmt.Sprintf("[statesync]\nenable=true\nrpc_servers=\"%s,%s\"\ntrust_height=%d\ntrust_hash=\"%s\"\ntrust_period=\"1h\"\ndiscovery_time=\"5s\"\nchunk_request_timeout=\"5s\"\n", cfg.RPCServers[0], cfg.RPCServers[1], trustHeight, correct))
	startNode(2)
	waitHeight(2, 40)
	logRaw, _ := os.ReadFile(filepath.Join(root, "node-2.log"))
	marker := "snapshot installed height="
	index := strings.Index(string(logRaw), marker)
	if index < 0 {
		t.Fatal("receiver did not install an ABCI snapshot")
	}
	if !strings.Contains(string(logRaw), fmt.Sprintf("archive_included=%t", included)) {
		t.Fatal("consumer policy log missing")
	}
	var restored int64
	if _, e = fmt.Sscanf(string(logRaw[index:]), marker+"%d", &restored); e != nil {
		t.Fatal(e)
	}
	final := waitHeight(2, restored+22)
	waitHeight(0, final+2)
	// Re-execute the donor's actual blocks through an application with snapshots
	// disabled. Compare every resulting hash to the next committed header.
	baseline := chain.NewMemoryApplication()
	if _, e = baseline.InitChain(context.Background(), &abci.InitChainRequest{ChainId: g.Parameters.ChainID, AppStateBytes: appJSON}); e != nil {
		t.Fatal(e)
	}
	for h := int64(1); h <= final; h++ {
		block, e := clients[0].Block(context.Background(), &h)
		if e != nil {
			t.Fatal(e)
		}
		r, e := baseline.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: h, Time: block.Block.Time, Hash: block.BlockID.Hash})
		if e != nil {
			t.Fatal(e)
		}
		baseline.Commit(context.Background(), &abci.CommitRequest{})
		next := h + 1
		c, e := clients[0].Commit(context.Background(), &next)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(r.AppHash, c.SignedHeader.Header.AppHash) {
			t.Fatalf("snapshot-disabled baseline differs at %d", h)
		}
		if h >= restored {
			other, e := clients[2].Commit(context.Background(), &next)
			if e != nil {
				waitHeight(2, next)
				other, e = clients[2].Commit(context.Background(), &next)
			}
			if e != nil || !bytes.Equal(other.SignedHeader.Header.AppHash, r.AppHash) {
				t.Fatalf("receiver hash differs at %d: %v", h, e)
			}
		}
		t.Logf("height=%d donor=baseline app_hash=%x", h, r.AppHash)
	}
	t.Logf("real P2P state sync restored H=%d, followed through %d (%d new blocks); donor, receiver, snapshot-disabled baseline hashes match", restored, final, final-restored)
}
