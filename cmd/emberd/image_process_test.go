package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ember/services/chain"
	cfg "github.com/cometbft/cometbft/config"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/node"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/privval"
	"github.com/cometbft/cometbft/proxy"
	"github.com/cometbft/cometbft/types"
)

func TestImageDaemonHelper(t *testing.T) {
	if os.Getenv("EMBER_IMAGE_DAEMON_HELPER") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("EMBER_IMAGE_DAEMON_ARGS")), &args); err != nil {
		panic(err)
	}
	os.Args = append([]string{"emberd", "gateway"}, args...)
	main()
	os.Exit(0)
}

func imageTestAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func TestImageDaemonProcess(t *testing.T) {
	// A real CometBFT process-in-test supplies the same RPC and WebSocket API as
	// deployment. No image service or gateway object is injected into the daemon.
	root := t.TempDir()
	chainID := "image-daemon-test"
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(root, "authority.key")
	if err = os.WriteFile(keyFile, key, 0600); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(root, "auth.json")
	raw, _ := json.Marshal([]map[string]string{{"token": strings.Repeat("a", 32), "key": keyFile}})
	if err = os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "chain")
	config := cfg.DefaultConfig().SetRoot(home)
	cfg.EnsureRoot(home)
	rpcAddr := imageTestAddr(t)
	config.P2P.ListenAddress = "tcp://" + imageTestAddr(t)
	config.RPC.ListenAddress = "tcp://" + rpcAddr
	config.P2P.PexReactor = false
	config.P2P.AddrBookStrict = false
	config.Consensus.TimeoutCommit = 20 * time.Millisecond
	pv, err := privval.GenFilePV(config.PrivValidatorKeyFile(), config.PrivValidatorStateFile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pv.Save()
	nk, err := p2p.LoadOrGenNodeKey(config.NodeKeyFile())
	if err != nil {
		t.Fatal(err)
	}
	pub, err := pv.GetPubKey()
	if err != nil {
		t.Fatal(err)
	}
	id := chain.AccountID(key.Public().(ed25519.PublicKey))
	state := chain.GenesisState{Parameters: chain.Parameters{ChainID: chainID, UnbondingBlocks: 3, SlashMultiplier: 20}, Accounts: []chain.GenesisAccount{{ID: id, Balance: 100000, Stake: 10000}}, Coordinators: []string{id}, Auditors: []string{id}}
	raw, _ = json.Marshal(state)
	doc := &types.GenesisDoc{ChainID: chainID, GenesisTime: time.Now().UTC(), AppState: raw, Validators: []types.GenesisValidator{{Address: pub.Address(), PubKey: pub, Power: 10}}}
	if err = doc.ValidateAndComplete(); err != nil {
		t.Fatal(err)
	}
	if err = doc.SaveAs(config.GenesisFile()); err != nil {
		t.Fatal(err)
	}
	app, err := chain.OpenApplication(chain.StatePath(home))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := node.NewNode(ctx, config, pv, nk, proxy.NewLocalClientCreator(app), node.DefaultGenesisDocProviderFunc(config), cfg.DefaultDBProvider, node.DefaultMetricsProvider(config.Instrumentation), cmtlog.NewNopLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err = n.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { n.Stop(); n.Wait() }()
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			dir := t.TempDir()
			db := filepath.Join(dir, "daemon.db")
			httpAddr := imageTestAddr(t)
			args := []string{"--db", db, "--http", httpAddr, "--grpc", imageTestAddr(t), "--insecure-localhost", "--liveness-enabled=false"}
			if enabled {
				args = append(args, "--image-enabled", "--ledger-backend", "chain", "--chain-rpc", "http://"+rpcAddr, "--chain-id", chainID, "--chain-coordinator-key", keyFile, "--chain-auditor-key", keyFile, "--chain-auth-file", authFile)
			}
			raw, _ := json.Marshal(args)
			command := exec.Command(os.Args[0], "-test.run=^TestImageDaemonHelper$")
			command.Env = append(os.Environ(), "EMBER_IMAGE_DAEMON_HELPER=1", "EMBER_IMAGE_DAEMON_ARGS="+string(raw))
			output, err := os.Create(filepath.Join(dir, "daemon.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			command.Stdout = output
			command.Stderr = output
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				command.Process.Signal(syscall.SIGTERM)
				done := make(chan error, 1)
				go func() { done <- command.Wait() }()
				select {
				case err := <-done:
					if err != nil {
						log, _ := os.ReadFile(output.Name())
						t.Errorf("daemon exit: %v\n%s", err, log)
					}
				case <-time.After(20 * time.Second):
					command.Process.Kill()
					t.Error("daemon shutdown timed out")
				}
			}()
			client := &http.Client{Timeout: time.Second}
			deadline := time.Now().Add(20 * time.Second)
			want := 404
			if enabled {
				want = 401
			}
			for {
				response, e := client.Get("http://" + httpAddr + "/v1/images/quote")
				if e == nil {
					body, _ := io.ReadAll(response.Body)
					response.Body.Close()
					if response.StatusCode != want {
						t.Fatalf("HTTP %d: %s", response.StatusCode, body)
					}
					break
				}
				if time.Now().After(deadline) {
					log, _ := os.ReadFile(output.Name())
					t.Fatalf("daemon not ready: %v\n%s", e, log)
				}
				time.Sleep(20 * time.Millisecond)
			}
			for _, suffix := range []string{".image.key", ".image-spool"} {
				_, e := os.Stat(db + suffix)
				if enabled && e != nil {
					t.Fatal(e)
				}
				if !enabled && !os.IsNotExist(e) {
					t.Fatalf("disabled initialized %s", suffix)
				}
			}
		})
	}
}
