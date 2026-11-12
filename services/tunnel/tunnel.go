package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/evidence"
	"ember/services/ledger"
	"ember/services/tracker"
	"google.golang.org/protobuf/proto"
)

type JoinData struct {
	Genesis   []byte   `json:"genesis"`
	Manifest  []byte   `json:"manifest"`
	Nonce     []byte   `json:"nonce"`
	Questions []string `json:"questions"`
}
type JoinProof struct {
	Profile   []byte   `json:"profile"`
	Signature []byte   `json:"signature"`
	Answers   []string `json:"answers"`
}

type peer struct {
	stream  pb.NodeTunnel_ConnectServer
	mu      sync.Mutex
	jobs    map[string]chan *pb.TunnelFrame
	jobDone map[string]chan struct{}
	done    chan struct{}
}
type Server struct {
	Evidence          *evidence.Store
	EvidenceLimit     func() (uint32, error)
	AuthorizeEvidence func(string, string) (*pb.EvidenceManifest, uint32, error)
	pb.UnimplementedNodeTunnelServer
	mu                  sync.Mutex
	peers               map[string]*peer
	Tracker             *tracker.Tracker
	Ledger              ledger.Backend
	Genesis             *pb.ChainGenesis
	Manifest            *pb.ModelManifest
	GenesisBytes        []byte
	ManifestBytes       []byte
	JoinGrant           int64
	Legacy              bool
	SchedulerKey        ed25519.PrivateKey
	RequireRegistration func(*pb.ResourceProfile) error
	nonce               uint64
}

func New() *Server                      { return &Server{peers: map[string]*peer{}} }
func (s *Server) Online(id string) bool { s.mu.Lock(); defer s.mu.Unlock(); return s.peers[id] != nil }
func (s *Server) OnlineCount() int      { s.mu.Lock(); defer s.mu.Unlock(); return len(s.peers) }
func (s *Server) Connect(stream pb.NodeTunnel_ConnectServer) error {
	hello, err := stream.Recv()
	if err != nil {
		return err
	}
	id := hello.NodeId
	if s.Legacy && hello.Kind == "hello" && id != "" {
		log.Printf("LEGACY DEMO node=%s", id)
	} else {
		if hello.Kind != "join" || s.Tracker == nil || s.Genesis == nil {
			log.Printf("JOIN REJECT node=%s reason=expected join", id)
			return errors.New("expected join")
		}
		nonce := make([]byte, 32)
		if _, err = rand.Read(nonce); err != nil {
			return err
		}
		challenge, _ := json.Marshal(JoinData{Genesis: s.GenesisBytes, Manifest: s.ManifestBytes, Nonce: nonce, Questions: tracker.Questions})
		if err = stream.Send(&pb.TunnelFrame{Kind: "challenge", Payload: challenge}); err != nil {
			return err
		}
		response, e := stream.Recv()
		if e != nil {
			return e
		}
		var proof JoinProof
		if response.Kind != "authenticate" || json.Unmarshal(response.Payload, &proof) != nil {
			return s.reject(id, "invalid authentication frame")
		}
		profile := &pb.ResourceProfile{}
		if proto.Unmarshal(proof.Profile, profile) != nil || profile.NodeId != id || core.NodeID(profile.Signer) != id || !ed25519.Verify(profile.Signer, core.JoinChallenge(nonce), proof.Signature) {
			return s.reject(id, "invalid challenge signature or identity")
		}
		if profile.ChainId != s.Genesis.ChainId || string(profile.ManifestRoot) != string(s.Genesis.ModelManifestRoot) || profile.ExecutionProfileVer != s.Genesis.ExecutionProfileVer {
			return s.reject(id, "genesis profile mismatch")
		}
		if strings.HasPrefix(profile.EngineVer, "pipeline-") && !bytes.Equal(profile.ShardParametersHash, s.Genesis.ModelManifestRoot) {
			return s.reject(id, "pipeline topology mismatch")
		}
		if profile.ShardTotalLayers > 0 {
			if s.Manifest == nil || !validShardProfile(profile, s.Manifest) {
				return s.reject(id, "shard topology mismatch")
			}
		}
		if s.RequireRegistration != nil {
			if e = s.RequireRegistration(profile); e != nil {
				return s.reject(id, "chain registration: "+e.Error())
			}
		}
		observed, score, e := tracker.Benchmark(stream.Context(), func(_ context.Context, _ []string) ([]string, error) { return proof.Answers, nil })
		if e != nil {
			return s.reject(id, "benchmark failed")
		}
		if e = s.Tracker.Register(profile, observed, score, time.Now()); e != nil {
			return s.reject(id, e.Error())
		}
		if s.Ledger != nil {
			if _, _, e = s.Ledger.Balance(id); e != nil {
				if e = s.Ledger.Create(id, s.JoinGrant); e != nil {
					return s.reject(id, e.Error())
				}
				if s.JoinGrant > 0 {
					if e = s.Ledger.Stake(id, s.JoinGrant/2); e != nil {
						return s.reject(id, e.Error())
					}
				}
			}
		}
		if err = stream.Send(&pb.TunnelFrame{Kind: "ready"}); err != nil {
			return err
		}
		log.Printf("JOIN ACCEPT node=%s score=%d", id, score)
	}
	p := &peer{stream: stream, jobs: map[string]chan *pb.TunnelFrame{}, jobDone: map[string]chan struct{}{}, done: make(chan struct{})}
	s.mu.Lock()
	if s.peers[id] != nil {
		s.mu.Unlock()
		return errors.New("duplicate node connection")
	}
	s.peers[id] = p
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.peers[id] == p {
			delete(s.peers, id)
		}
		s.mu.Unlock()
		close(p.done)
	}()
	for {
		f, e := stream.Recv()
		if e != nil {
			return e
		}
		if f.NodeId != id {
			return s.reject(id, "node identity changed")
		}
		if f.Kind == "heartbeat" && !s.Legacy {
			renew := &pb.ResourceProfile{}
			if proto.Unmarshal(f.Payload, renew) != nil || renew.NodeId != id || s.Tracker.Heartbeat(renew, time.Now()) != nil {
				return s.reject(id, "invalid heartbeat")
			}
			continue
		}
		p.mu.Lock()
		ch := p.jobs[f.AttemptId]
		done := p.jobDone[f.AttemptId]
		p.mu.Unlock()
		if ch != nil {
			select {
			case ch <- f:
			case <-done:
			case <-stream.Context().Done():
				return stream.Context().Err()
			}
		}
	}
}

func validShardProfile(profile *pb.ResourceProfile, manifest *pb.ModelManifest) bool {
	if manifest.DynamicRanges {
		return profile.ShardDynamicRange && profile.ShardTotalLayers == manifest.TotalLayers && profile.ShardStartLayer < profile.ShardEndLayer && profile.ShardEndLayer <= manifest.TotalLayers && profile.ShardHasEmbedding == (profile.ShardStartLayer == 0) && profile.ShardHasLmHead == (profile.ShardEndLayer == manifest.TotalLayers) && bytes.Equal(profile.ShardParametersHash, core.RangeParameters(profile.ManifestRoot, profile.ShardStartLayer, profile.ShardEndLayer))
	}
	if profile.ShardTotalLayers != manifest.TotalLayers || profile.ShardIndex >= uint32(len(manifest.Shards)) {
		return false
	}
	spec := manifest.Shards[profile.ShardIndex]
	return spec.UnitKind == profile.ShardUnitKind && spec.Index == profile.ShardIndex && spec.StartLayer == profile.ShardStartLayer &&
		spec.EndLayer == profile.ShardEndLayer && spec.HasEmbedding == profile.ShardHasEmbedding &&
		spec.HasLmHead == profile.ShardHasLmHead && bytes.Equal(spec.ParametersHash, profile.ShardParametersHash)
}
func (s *Server) reject(id, reason string) error {
	log.Printf("JOIN REJECT node=%s reason=%s", id, reason)
	return errors.New(reason)
}
func (s *Server) Run(ctx context.Context, node, request, attempt string, payload []byte, receive func(*pb.TunnelFrame) error) error {
	return s.run(ctx, node, request, attempt, "run", payload, receive)
}

func (s *Server) Tokenize(ctx context.Context, node, request, attempt string, payload []byte, receive func(*pb.TunnelFrame) error) error {
	return s.run(ctx, node, request, attempt, "tokenize", payload, receive)
}

func (s *Server) Audit(ctx context.Context, node, request, attempt string, payload []byte, receive func(*pb.TunnelFrame) error) error {
	return s.run(ctx, node, request, attempt, "audit", payload, receive)
}

func (s *Server) run(ctx context.Context, node, request, attempt, kind string, payload []byte, receive func(*pb.TunnelFrame) error) error {
	s.mu.Lock()
	p := s.peers[node]
	s.mu.Unlock()
	if p == nil {
		return errors.New("node offline")
	}
	ch := make(chan *pb.TunnelFrame)
	p.mu.Lock()
	if p.jobs[attempt] != nil {
		p.mu.Unlock()
		return errors.New("duplicate attempt")
	}
	p.jobs[attempt] = ch
	done := make(chan struct{})
	p.jobDone[attempt] = done
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.jobs, attempt); delete(p.jobDone, attempt); close(done); p.mu.Unlock() }()
	frame := &pb.TunnelFrame{NodeId: node, RequestId: request, AttemptId: attempt, Kind: kind, Payload: payload}
	if len(s.SchedulerKey) != 0 {
		for {
			previous := atomic.LoadUint64(&s.nonce)
			next := uint64(time.Now().UnixNano())
			if next <= previous {
				next = previous + 1
			}
			if atomic.CompareAndSwapUint64(&s.nonce, previous, next) {
				frame.Nonce = next
				break
			}
		}
		if err := core.Sign(frame, s.SchedulerKey); err != nil {
			return err
		}
	}
	p.mu.Lock()
	err := p.stream.Send(frame)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			return errors.New("node disconnected")
		case f := <-ch:
			if f.RequestId != request {
				return fmt.Errorf("request mismatch")
			}
			if err := receive(f); err != nil {
				return err
			}
			if f.Kind == "receipt" || f.Kind == "audit_result" || f.Kind == "tokenization" || f.Kind == "error" {
				return nil
			}
		}
	}
}
