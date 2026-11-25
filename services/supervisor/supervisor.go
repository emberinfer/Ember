// Package supervisor manages isolated emberd workers. It never holds model runtime state.
package supervisor

import (
	"bytes"
	"context"
	pb "ember/proto"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"sync"
	"time"
)

type Entry struct {
	ProfileID       string    `json:"profile_id"`
	Published       bool      `json:"published"`
	Supported       bool      `json:"supported"`
	Ready           bool      `json:"ready"`
	Reason          string    `json:"reason"`
	Height          int64     `json:"height"`
	ExecutorsOnline int       `json:"executors_online"`
	OrdersActive    int       `json:"orders_active"`
	GenesisSHA256   string    `json:"genesis_sha256"`
	ManifestRoot    string    `json:"manifest_root"`
	WorkerState     string    `json:"worker_state"`
	LastSeen        time.Time `json:"last_seen"`
}
type Worker interface {
	HTTP() string
	Tunnel() string
	Done() <-chan error
	Stop()
}
type Source interface {
	Discover(context.Context) ([]Entry, error)
}
type Start func(context.Context, Entry) (Worker, error)
type slot struct {
	entry    Entry
	worker   Worker
	last     time.Time
	retry    time.Time
	failures int
	active   int
}
type Manager struct {
	pb.UnimplementedNodeTunnelServer
	mu            sync.Mutex
	Source        Source
	Start         Start
	Default       string
	MaxWorkers    int
	Idle          time.Duration
	Freshness     time.Duration
	Client        *http.Client
	slots         map[string]*slot
	lastDiscovery time.Time
}

func (m *Manager) init() {
	if m.slots == nil {
		m.slots = map[string]*slot{}
	}
	if m.MaxWorkers == 0 {
		m.MaxWorkers = 16
	}
	if m.Idle == 0 {
		m.Idle = 15 * time.Minute
	}
	if m.Freshness == 0 {
		m.Freshness = 15 * time.Second
	}
	if m.Client == nil {
		m.Client = &http.Client{Timeout: 3 * time.Second}
	}
}

// Tick serializes reconciliation. A failed registry read immediately closes admission.
func (m *Manager) Tick(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	entries, err := m.Source.Discover(ctx)
	if err != nil {
		m.lastDiscovery = time.Time{}
		return err
	}
	now := time.Now()
	m.lastDiscovery = now
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.ProfileID] = true
		s := m.slots[e.ProfileID]
		if s == nil {
			s = &slot{last: now}
			m.slots[e.ProfileID] = s
		}
		if s.worker != nil && (s.entry.GenesisSHA256 != e.GenesisSHA256 || !e.Supported) && s.active == 0 && e.OrdersActive == 0 {
			s.worker.Stop()
			s.worker = nil
		}
		s.entry = e
	}
	for id, s := range m.slots {
		if !seen[id] {
			if s.active == 0 {
				if s.worker != nil {
					s.worker.Stop()
				}
				delete(m.slots, id)
			}
			continue
		}
		if s.worker != nil {
			select {
			case <-s.worker.Done():
				s.worker = nil
				s.failures++
				delay := time.Second << min(s.failures, 6)
				s.retry = now.Add(delay)
			default:
			}
		}
	}
	ids := make([]string, 0, len(m.slots))
	for id := range m.slots {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == m.Default) != (ids[j] == m.Default) {
			return ids[i] == m.Default
		}
		return ids[i] < ids[j]
	})
	count := 0
	for _, s := range m.slots {
		if s.worker != nil {
			count++
		}
	}
	for _, id := range ids {
		s := m.slots[id]
		if s.worker != nil || !s.entry.Supported || now.Before(s.retry) {
			continue
		}
		if count >= m.MaxWorkers {
			var victim *slot
			for _, v := range m.slots {
				if v.worker == nil || v.active != 0 || v.entry.ProfileID == m.Default || now.Sub(v.last) < m.Idle {
					continue
				}
				e := m.probe(ctx, v)
				if e.ExecutorsOnline == 0 && e.OrdersActive == 0 && e.WorkerState == "running" && e.Reason != "worker unavailable" && e.Reason != "worker context unavailable" && (victim == nil || v.last.Before(victim.last)) {
					victim = v
				}
			}
			if victim == nil {
				continue
			}
			victim.worker.Stop()
			victim.worker = nil
			victim.retry = now.Add(m.Idle)
			count--
		}
		w, e := m.Start(ctx, s.entry)
		if e != nil {
			s.failures++
			s.retry = now.Add(time.Second << min(s.failures, 6))
			continue
		}
		s.worker = w
		s.last = now
		count++
	}
	return nil
}
func (m *Manager) probe(ctx context.Context, s *slot) Entry {
	e := s.entry
	e.Ready = false
	e.ExecutorsOnline = 0
	e.WorkerState = "stopped"
	e.LastSeen = m.lastDiscovery
	if m.lastDiscovery.IsZero() || time.Since(m.lastDiscovery) > m.Freshness {
		e.Reason = "registry unavailable or stale"
		return e
	}
	if !e.Supported {
		return e
	}
	if s.worker == nil {
		e.Reason = "worker pending or restarting"
		return e
	}
	e.WorkerState = "running"
	req, _ := http.NewRequestWithContext(ctx, "GET", s.worker.HTTP()+"/v1/availability", nil)
	r, err := m.Client.Do(req)
	if err != nil {
		e.Reason = "worker unavailable"
		return e
	}
	defer r.Body.Close()
	var status struct {
		Ready     bool   `json:"ready"`
		Reason    string `json:"reason"`
		Executors int    `json:"executors_online"`
	}
	if r.StatusCode != 200 || json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&status) != nil {
		e.Reason = "worker context unavailable"
		return e
	}
	e.Ready = status.Ready
	e.Reason = status.Reason
	e.ExecutorsOnline = status.Executors
	if e.ExecutorsOnline > 0 {
		s.last = time.Now()
	}
	return e
}
func (m *Manager) Catalog(ctx context.Context) []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	out := make([]Entry, 0, len(m.slots))
	for _, s := range m.slots {
		out = append(out, m.probe(ctx, s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProfileID < out[j].ProfileID })
	return out
}
func (m *Manager) acquire(ctx context.Context, id string, ready bool) (Worker, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	s := m.slots[id]
	if s == nil {
		return nil, nil, errors.New("unknown profile")
	}
	e := m.probe(ctx, s)
	if !e.Supported || s.worker == nil || (ready && !e.Ready) || m.lastDiscovery.IsZero() || time.Since(m.lastDiscovery) > m.Freshness {
		return nil, nil, fmt.Errorf("profile unavailable: %s", e.Reason)
	}
	s.active++
	s.last = time.Now()
	return s.worker, func() { m.mu.Lock(); s.active--; s.last = time.Now(); m.mu.Unlock() }, nil
}
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.slots {
		if s.worker != nil {
			s.worker.Stop()
		}
	}
}
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/catalog" && r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"profiles": m.Catalog(r.Context())})
		return
	}
	if r.URL.Path != "/v1/quote" && r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/pricing" {
		http.NotFound(w, r)
		return
	}
	id := r.URL.Query().Get("model")
	if r.Method == "POST" {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			http.Error(w, "request too large", 413)
			return
		}
		var q struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(raw, &q) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		id = q.Model
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}
	worker, release, err := m.acquire(r.Context(), id, true)
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	defer release()
	target, _ := url.Parse(worker.HTTP())
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ServeHTTP(w, r)
}
func (m *Manager) Connect(stream pb.NodeTunnel_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.Kind != "join" || len(first.Payload) > 1024 {
		return errors.New("invalid JOIN assertion")
	}
	id := m.Default
	if len(first.Payload) > 0 {
		var a struct {
			ID string `json:"profile_id"`
		}
		if json.Unmarshal(first.Payload, &a) != nil || a.ID == "" {
			return errors.New("invalid JOIN assertion")
		}
		id = a.ID
	}
	worker, release, err := m.acquire(stream.Context(), id, false)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	conn, err := grpc.NewClient(worker.Tunnel(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	upstream, err := pb.NewNodeTunnelClient(conn).Connect(ctx)
	if err != nil {
		return err
	}
	if err = upstream.Send(first); err != nil {
		return err
	}
	errorsCh := make(chan error, 2)
	go func() {
		for {
			f, e := stream.Recv()
			if e != nil {
				errorsCh <- e
				return
			}
			if e = upstream.Send(f); e != nil {
				errorsCh <- e
				return
			}
		}
	}()
	go func() {
		for {
			f, e := upstream.Recv()
			if e != nil {
				errorsCh <- e
				return
			}
			if e = stream.Send(f); e != nil {
				errorsCh <- e
				return
			}
		}
	}()
	err = <-errorsCh
	if err == io.EOF {
		return nil
	}
	return err
}
