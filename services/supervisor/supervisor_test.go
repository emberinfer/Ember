package supervisor

import (
	"context"
	pb "ember/proto"
	"encoding/json"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureSource struct {
	entries []Entry
	err     error
}

func (s *fixtureSource) Discover(context.Context) ([]Entry, error) { return s.entries, s.err }

type fixtureWorker struct {
	web, tunnel string
	done        chan error
	stopped     bool
}

func (w *fixtureWorker) HTTP() string       { return w.web }
func (w *fixtureWorker) Tunnel() string     { return w.tunnel }
func (w *fixtureWorker) Done() <-chan error { return w.done }
func (w *fixtureWorker) Stop()              { w.stopped = true }
func fixture(t *testing.T) (*Manager, *fixtureSource, *atomic.Bool, *[]*fixtureWorker) {
	t.Helper()
	ready := &atomic.Bool{}
	ready.Store(true)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/availability" {
			json.NewEncoder(w).Encode(map[string]any{"ready": ready.Load(), "executors_online": btoi(ready.Load()), "reason": "no eligible online executor"})
			return
		}
		w.Header().Set("X-Worker", "isolated")
		io.Copy(w, r.Body)
	}))
	t.Cleanup(web.Close)
	src := &fixtureSource{entries: []Entry{{ProfileID: "ember-a", Supported: true, Height: 10}}}
	workers := &[]*fixtureWorker{}
	m := &Manager{Source: src, Default: "ember-a", MaxWorkers: 2, Idle: time.Minute, Start: func(context.Context, Entry) (Worker, error) {
		w := &fixtureWorker{web: web.URL, done: make(chan error, 1)}
		*workers = append(*workers, w)
		return w, nil
	}}
	return m, src, ready, workers
}
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
func tick(t *testing.T, m *Manager) {
	t.Helper()
	if e := m.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestDiscoveryCatalogRouting(t *testing.T) {
	m, src, ready, _ := fixture(t)
	tick(t, m)
	src.entries = append(src.entries, Entry{ProfileID: "ember-new", Supported: true, Height: 11}, Entry{ProfileID: "ember-image", Reason: "unsupported engine"})
	tick(t, m)
	rows := m.Catalog(context.Background())
	if len(rows) != 3 || !rows[0].Ready || rows[1].Supported {
		t.Fatal(rows)
	}
	for _, path := range []string{"/v1/quote", "/v1/chat/completions"} {
		for _, id := range []string{"ember-new", "ember-unknown"} {
			req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"`+id+`"}`))
			w := httptest.NewRecorder()
			m.ServeHTTP(w, req)
			if (id == "ember-new") != (w.Code == 200) {
				t.Fatalf("%s %d %s", id, w.Code, w.Body.String())
			}
		}
	}
	ready.Store(false)
	if m.Catalog(context.Background())[2].Ready {
		t.Fatal("offline executor still ready")
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("POST", "/v1/quote", strings.NewReader(`{"model":"ember-new"}`)))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	ready.Store(true)
	if !m.Catalog(context.Background())[2].Ready {
		t.Fatal("recovery failed")
	}
	src.err = errors.New("RPC disconnected")
	if m.Tick(context.Background()) == nil {
		t.Fatal("expected discovery error")
	}
	if m.Catalog(context.Background())[0].Ready {
		t.Fatal("stale ready")
	}
}
func TestCrashBackoffLRUAndActiveProtection(t *testing.T) {
	m, src, ready, workers := fixture(t)
	m.MaxWorkers = 1
	tick(t, m)
	(*workers)[0].done <- errors.New("crash")
	tick(t, m)
	if len(*workers) != 1 || m.slots["ember-a"].worker != nil {
		t.Fatal("restart ignored backoff")
	}
	m.slots["ember-a"].retry = time.Now().Add(-time.Second)
	tick(t, m)
	if len(*workers) != 2 {
		t.Fatal("did not restart")
	}
	m.Default = ""
	ready.Store(false)
	src.entries = append(src.entries, Entry{ProfileID: "ember-b", Supported: true})
	s := m.slots["ember-a"]
	s.last = time.Now().Add(-2 * time.Minute)
	s.active = 1
	tick(t, m)
	if len(*workers) != 2 {
		t.Fatal("evicted active worker")
	}
	s.active = 0
	src.entries[0].OrdersActive = 1
	tick(t, m)
	if len(*workers) != 2 {
		t.Fatal("evicted worker with outstanding chain settlement")
	}
	src.entries[0].OrdersActive = 0
	tick(t, m)
	if !(*workers)[1].stopped || m.slots["ember-b"].worker == nil {
		t.Fatal("LRU failed")
	}
	src.entries = nil
	tick(t, m)
	if len(m.slots) != 0 {
		t.Fatal("removed publication remains")
	}
}

type echoTunnel struct {
	pb.UnimplementedNodeTunnelServer
}

func (echoTunnel) Connect(s pb.NodeTunnel_ConnectServer) error {
	for {
		f, e := s.Recv()
		if e != nil {
			return e
		}
		if e = s.Send(f); e != nil {
			return e
		}
	}
}
func TestTunnelAssertionDefaultUnknownAndIsolation(t *testing.T) {
	m, src, _, workers := fixture(t)
	src.entries = append(src.entries, Entry{ProfileID: "ember-b", Supported: true})
	tick(t, m)
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	worker := grpc.NewServer()
	pb.RegisterNodeTunnelServer(worker, echoTunnel{})
	go worker.Serve(l)
	defer worker.Stop()
	for _, w := range *workers {
		w.tunnel = l.Addr().String()
	}
	l2, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	router := grpc.NewServer()
	pb.RegisterNodeTunnelServer(router, m)
	go router.Serve(l2)
	defer router.Stop()
	c, e := grpc.NewClient(l2.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	for _, payload := range []string{"", `{"profile_id":"ember-b"}`, `{"profile_id":"ember-unknown"}`, `{"profile_id":"../escape"}`} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		stream, e := pb.NewNodeTunnelClient(c).Connect(ctx)
		if e != nil {
			t.Fatal(e)
		}
		stream.Send(&pb.TunnelFrame{Kind: "join", Payload: []byte(payload)})
		f, e := stream.Recv()
		unknown := strings.Contains(payload, "unknown") || strings.Contains(payload, "escape")
		if unknown && e == nil {
			t.Fatal("unknown profile accepted")
		}
		if !unknown && (e != nil || string(f.Payload) != payload) {
			t.Fatalf("assertion forwarding: %v", e)
		}
		cancel()
	}
}

func TestDefaultWorkerHasCapacityPriority(t *testing.T) {
	m, src, _, _ := fixture(t)
	m.MaxWorkers = 1
	m.Default = "ember-z"
	src.entries = []Entry{{ProfileID: "ember-a", Supported: true}, {ProfileID: "ember-z", Supported: true}}
	tick(t, m)
	if m.slots["ember-z"].worker == nil || m.slots["ember-a"].worker != nil {
		t.Fatal("default profile starved by publication order")
	}
}
