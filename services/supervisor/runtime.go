package supervisor

import (
	"context"
	"crypto/tls"
	pb "ember/proto"
	"ember/services/chain"
	"ember/services/ledger"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Registry struct{ Artifacts *ledger.ArtifactClient }

func (r Registry) Discover(ctx context.Context) ([]Entry, error) {
	status, err := r.Artifacts.RPC.Status(ctx)
	if err != nil {
		return nil, err
	}
	if status.NodeInfo.Network != r.Artifacts.ChainID {
		return nil, errors.New("ledger chain ID mismatch")
	}
	result, err := r.Artifacts.RPC.ABCIQuery(ctx, "/state", nil)
	if err != nil {
		return nil, err
	}
	if result.Response.Code != 0 || len(result.Response.Value) > 64<<20 {
		return nil, errors.New("registry query failed or too large")
	}
	var state struct {
		Models map[string]chain.Model `json:"models"`
		Holds  map[string]*chain.Hold `json:"holds"`
	}
	if err = json.Unmarshal(result.Response.Value, &state); err != nil {
		return nil, err
	}
	orders := map[string]int{}
	for _, hold := range state.Holds {
		if hold != nil && (hold.State == "frozen" || hold.State == "dispute") {
			orders[hold.ModelID]++
		}
	}
	out := []Entry{}
	for id, model := range state.Models {
		e := Entry{ProfileID: id, Height: result.Response.Height, OrdersActive: orders[id], Published: model.Publication != nil}
		a, err := ledger.VerifyModelArtifacts(id, model)
		if err != nil {
			e.Reason = err.Error()
			out = append(out, e)
			continue
		}
		e.GenesisSHA256 = a.GenesisSHA256
		e.ManifestRoot = a.ManifestRoot
		g, m := &pb.ChainGenesis{}, &pb.ModelManifest{}
		proto.Unmarshal(a.Genesis, g)
		proto.Unmarshal(a.Manifest, m)
		e.Supported = true
		if model.Kind == "text-to-image" || model.ImageProfile != nil || m.MoeLayout != nil || strings.Contains(strings.ToLower(g.ExecutionProfileVer), "moe") || strings.Contains(strings.ToLower(g.ExecutionProfileVer), "image") || strings.Contains(strings.ToLower(g.ExecutionProfileVer), "diffusion") || strings.Contains(strings.ToLower(g.ExecutionProfileVer), "diffusers") {
			e.Supported = false
			e.Reason = "unsupported engine; dedicated gateway required"
		}
		// Pipeline serving needs profile-specific tokenizer/layout configuration. Do
		// not advertise partial shard coverage as a whole-model execution context.
		if len(m.Shards) > 0 || m.DynamicRanges {
			e.Supported = false
			e.Reason = "pipeline context unavailable; complete shard coverage required"
		}
		out = append(out, e)
	}
	return out, nil
}

type process struct {
	cmd          *exec.Cmd
	http, tunnel string
	done         chan error
}

func (p *process) HTTP() string       { return p.http }
func (p *process) Tunnel() string     { return p.tunnel }
func (p *process) Done() <-chan error { return p.done }
func (p *process) Stop() {
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (p *process) wait(output *os.File, logStart int64) {
	err := p.cmd.Wait()
	output.Close()
	if line := workerLogTail(output.Name(), logStart); line != "" {
		err = errors.New(line)
	}
	p.done <- err
	close(p.done)
}

func workerLogTail(path string, logStart int64) string {
	logFile, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer logFile.Close()
	info, err := logFile.Stat()
	if err != nil || info.Size() <= logStart {
		return ""
	}
	const tailBytes = 8192
	start := max(logStart, info.Size()-tailBytes)
	tail, err := io.ReadAll(io.NewSectionReader(logFile, start, info.Size()-start))
	if err != nil {
		return ""
	}
	trimmed := strings.TrimRight(string(tail), " \t\r\n")
	return strings.TrimSpace(trimmed[strings.LastIndexByte(trimmed, '\n')+1:])
}

func freeAddress() (string, error) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return "", e
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// Run adds an opt-in process supervisor; normal emberd modes are untouched.
func Run(args []string) error {
	f := flag.NewFlagSet("supervisor", flag.ContinueOnError)
	rpc := f.String("chain-rpc", "", "operator-pinned local/test ledger RPC")
	chainID := f.String("chain-id", "", "ledger chain ID")
	def := f.String("default-profile", "", "legacy JOIN default profile")
	httpAddr := f.String("http", "127.0.0.1:19090", "router HTTP address")
	tunnelAddr := f.String("grpc", "127.0.0.1:19091", "router tunnel address")
	dir := f.String("worker-dir", "workers", "isolated worker data directory")
	config := f.String("worker-args", "", "JSON array of common emberd worker arguments")
	cert := f.String("tls-cert", "", "tunnel certificate")
	key := f.String("tls-key", "", "tunnel private key")
	local := f.Bool("insecure-localhost", false, "allow plaintext tunnel on loopback only")
	max := f.Int("max-workers", 16, "concurrent worker limit")
	poll := f.Duration("poll", 3*time.Second, "registry poll interval")
	idle := f.Duration("idle", 15*time.Minute, "minimum idle age for LRU eviction")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *max < 1 || *poll <= 0 || *idle <= 0 || *def == "" {
		return errors.New("invalid supervisor limits or missing default profile")
	}
	var common []string
	if *config != "" {
		raw, e := os.ReadFile(*config)
		if e != nil {
			return e
		}
		if e = json.Unmarshal(raw, &common); e != nil {
			return e
		}
	}
	for _, arg := range common {
		for _, reserved := range []string{"http", "grpc", "db", "profile-id", "genesis", "manifest", "trust-policy", "chain-rpc", "chain-id", "chain-sign-lock", "ledger-backend", "audit-spool-dir", "audit-key", "image-spool-dir", "image-key", "faucet"} {
			if strings.HasPrefix(arg, "--"+reserved+"=") || arg == "--"+reserved || strings.HasPrefix(arg, "--faucet") {
				return fmt.Errorf("worker argument %s is managed by supervisor", arg)
			}
		}
	}
	artifacts, err := ledger.NewArtifactClient(*rpc, *chainID)
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	manager := &Manager{Source: Registry{artifacts}, Default: *def, MaxWorkers: *max, Idle: *idle, Freshness: 3 * *poll}
	manager.Start = func(startCtx context.Context, e Entry) (Worker, error) {
		// Artifact verification validates ID before it is used as a directory name.
		a, err := artifacts.Fetch(startCtx, e.ProfileID)
		if err != nil {
			return nil, err
		}
		home := filepath.Join(*dir, e.ProfileID)
		if err = os.MkdirAll(home, 0700); err != nil {
			return nil, err
		}
		// Automatic publication admission is local policy, never a chain-rule change.
		trust := chain.TrustPolicy{Profiles: map[string]chain.ProfileTrust{}}
		if e.Published {
			trust.Profiles[e.ProfileID] = chain.ProfileTrust{Publishers: []string{a.Publisher}}
		}
		raw, _ := json.Marshal(trust)
		trustFile := filepath.Join(home, "trust.json")
		if err = os.WriteFile(trustFile, raw, 0600); err != nil {
			return nil, err
		}
		h, err := freeAddress()
		if err != nil {
			return nil, err
		}
		t, err := freeAddress()
		if err != nil {
			return nil, err
		}
		argv := append([]string{"all"}, common...)
		argv = append(argv, "--ledger-backend", "chain", "--chain-rpc", *rpc, "--chain-id", *chainID, "--profile-id", e.ProfileID, "--http", h, "--grpc", t, "--insecure-localhost", "--db", filepath.Join(home, "ledger.db"), "--trust-policy", trustFile, "--chain-sign-lock", filepath.Join(*dir, "transactions.lock"))
		output, err := os.OpenFile(filepath.Join(home, "worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return nil, err
		}
		logStart, err := output.Seek(0, io.SeekEnd)
		if err != nil {
			output.Close()
			return nil, err
		}
		cmd := exec.Command(binary, argv...)
		cmd.Stdout = output
		cmd.Stderr = output
		if err = cmd.Start(); err != nil {
			output.Close()
			return nil, err
		}
		p := &process{cmd: cmd, http: "http://" + h, tunnel: t, done: make(chan error, 1)}
		go p.wait(output, logStart)
		return p, nil
	}
	defer manager.Close()
	listener, err := net.Listen("tcp", *tunnelAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	var options []grpc.ServerOption
	if *local {
		host, _, e := net.SplitHostPort(*tunnelAddr)
		if e != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			return errors.New("plaintext router tunnel must bind loopback")
		}
	} else {
		pair, e := tls.LoadX509KeyPair(*cert, *key)
		if e != nil {
			return e
		}
		options = append(options, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})))
	}
	server := grpc.NewServer(options...)
	pb.RegisterNodeTunnelServer(server, manager)
	defer server.Stop()
	web := &http.Server{Addr: *httpAddr, Handler: manager, ReadHeaderTimeout: 5 * time.Second}
	defer web.Close()
	failures := make(chan error, 2)
	go func() { failures <- server.Serve(listener) }()
	go func() { failures <- web.ListenAndServe() }()
	reconcile := func() {
		c, stop := context.WithTimeout(ctx, *poll)
		defer stop()
		if e := manager.Tick(c); e != nil {
			log.Printf("registry: %v", e)
		}
	}
	reconcile()
	ticker := time.NewTicker(*poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-failures:
			return e
		case <-ticker.C:
			reconcile()
		}
	}
}
