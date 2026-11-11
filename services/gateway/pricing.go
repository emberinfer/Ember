package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/ledger"
	"ember/services/scheduler"
	"google.golang.org/protobuf/proto"
)

func (g *Gateway) priced() bool { return g.PriceInput != 0 || g.PriceContext != 0 }
func (g *Gateway) fee(input, output uint32) (core.FeeComponents, error) {
	return core.TokenFee(&pb.ChainGenesis{PriceInput: g.PriceInput, PriceOutput: g.Price, PriceContext: g.PriceContext}, input, output)
}

func (g *Gateway) freeze(id, payer, node string, amount int64, commitment []byte) error {
	if len(commitment) == 0 {
		return g.Ledger.Freeze(id, payer, node, amount)
	}
	b, ok := g.Ledger.(ledger.PricedBackend)
	if !ok {
		return errors.New("token pricing requires consensus backend")
	}
	return b.FreezePriced(id, payer, node, amount, commitment)
}

func (g *Gateway) tokenize(ctx context.Context, q request, prompt, requestID, node string) ([]byte, uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	j := job{Pricing: true, Prompt: prompt, MaxTokens: q.MaxTokens, Seed: q.Seed, Chain: q.Model, Profile: g.Profile}
	raw, _ := json.Marshal(j)
	var wire []byte
	if g.Pipeline == nil {
		err := g.Tunnel.Tokenize(ctx, node, requestID, id(), raw, func(f *pb.TunnelFrame) error {
			if f.Kind != "tokenization" {
				return errors.New("tokenization failed")
			}
			wire = append([]byte(nil), f.Payload...)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	} else {
		cmd := exec.CommandContext(ctx, g.Pipeline.Python, "-m", "tokenize_prompt", g.Pipeline.ModelPath)
		cmd.Dir = g.Pipeline.WorkDir
		cmd.Env = append(os.Environ(), "PYTHONPATH=core-py:proto:node")
		cmd.Stdin = bytes.NewBufferString(prompt)
		out, err := cmd.Output()
		if err != nil {
			return nil, 0, err
		}
		var n uint32
		if json.Unmarshal(out, &n) != nil || n == 0 {
			return nil, 0, errors.New("invalid tokenizer response")
		}
		key, err := os.ReadFile(g.Pipeline.SchedulerKeyFile)
		if err != nil {
			return nil, 0, err
		}
		if len(key) == ed25519.SeedSize {
			key = ed25519.NewKeyFromSeed(key)
		}
		hash := sha256.Sum256([]byte(prompt))
		sampling := sha256.Sum256([]byte(strconv.Itoa(q.MaxTokens)))
		c := &pb.ExecutionCommitment{RequestId: requestID, ChainId: q.Model, ProfileVer: g.Profile, InputCommitment: hash[:], SamplingParamsHash: sampling[:], Seed: q.Seed, InputTokens: n, MaxOutputTokens: uint32(q.MaxTokens)}
		if err := core.Sign(c, ed25519.PrivateKey(key)); err != nil {
			return nil, 0, err
		}
		wire, err = proto.MarshalOptions{Deterministic: true}.Marshal(c)
		if err != nil {
			return nil, 0, err
		}
	}
	c := &pb.ExecutionCommitment{}
	hash := sha256.Sum256([]byte(prompt))
	sampling := sha256.Sum256([]byte(strconv.Itoa(q.MaxTokens)))
	if proto.Unmarshal(wire, c) != nil || core.Verify(c) != nil || c.RequestId != requestID || c.ChainId != q.Model || c.ProfileVer != g.Profile || !bytes.Equal(c.InputCommitment, hash[:]) || !bytes.Equal(c.SamplingParamsHash, sampling[:]) || c.Seed != q.Seed || c.InputTokens == 0 || c.MaxOutputTokens != uint32(q.MaxTokens) || (node != "" && core.NodeID(c.Signer) != node) {
		return nil, 0, errors.New("invalid signed tokenization")
	}
	return wire, c.InputTokens, nil
}

func (g *Gateway) servePricing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"model": g.ChainID, "profile": g.Profile, "price_input": g.PriceInput, "price_output": g.Price, "price_context": g.PriceContext, "context_divisor": core.ContextDivisor, "quote_path": "/v1/quote"})
}

func (g *Gateway) serveQuote(w http.ResponseWriter, r *http.Request, q request, prompt string) {
	node := ""
	if g.Pipeline == nil {
		c, err := scheduler.Choose(scheduler.Request{Chain: q.Model, Manifest: g.Manifest, Profile: g.Profile}, g.Candidates())
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		node = c.NodeID
	}
	_, n, err := g.tokenize(r.Context(), q, prompt, id(), node)
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	c, err := g.fee(n, uint32(q.MaxTokens))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"model": q.Model, "input_tokens": n, "max_output_tokens": q.MaxTokens, "components": c, "freeze": c.Total, "context_divisor": core.ContextDivisor})
}
