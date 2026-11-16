package faucet

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type TransferFunc func(string, int64) error

type Handler struct {
	Transfer  TransferFunc
	Amount    int64
	Window    time.Duration
	StatePath string
	Now       func() time.Time
	// Only this immediate proxy may supply one canonical client IP.
	TrustedProxy string

	mu     sync.Mutex
	loaded bool
	state  persistedState
}

type persistedState struct {
	Accounts map[string]int64 `json:"accounts"`
	Sources  map[string]int64 `json:"sources"`
}

type request struct {
	Account string `json:"account"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if h.Transfer == nil || h.Amount <= 0 || h.StatePath == "" {
		http.Error(w, "faucet disabled", http.StatusNotFound)
		return
	}
	var body request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if decoder.Decode(&body) != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	account := strings.ToLower(strings.TrimSpace(body.Account))
	decoded, err := hex.DecodeString(account)
	if err != nil || len(decoded) != 32 {
		http.Error(w, "invalid account", http.StatusBadRequest)
		return
	}
	source := sourceIP(r.RemoteAddr)
	if proxy := net.ParseIP(h.TrustedProxy); proxy != nil && proxy.Equal(net.ParseIP(source)) {
		if forwarded := r.Header.Values("X-Forwarded-For"); len(forwarded) != 0 {
			if len(forwarded) != 1 || net.ParseIP(forwarded[0]) == nil {
				http.Error(w, "invalid forwarded source", http.StatusBadRequest)
				return
			}
			source = net.ParseIP(forwarded[0]).String()
		}
	}
	if source == "" {
		http.Error(w, "invalid source", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	window := h.Window
	if window <= 0 {
		window = 24 * time.Hour
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if err = h.load(); err != nil {
		http.Error(w, "faucet state unavailable", http.StatusServiceUnavailable)
		return
	}
	cutoff := now.Add(-window).Unix()
	if h.state.Accounts[account] > cutoff || h.state.Sources[source] > cutoff {
		w.Header().Set("Retry-After", durationSeconds(window))
		http.Error(w, "faucet rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	h.state.Accounts[account] = now.Unix()
	h.state.Sources[source] = now.Unix()
	if err = h.save(); err != nil {
		http.Error(w, "faucet state persistence failed", http.StatusInternalServerError)
		return
	}
	if err = h.Transfer(account, h.Amount); err != nil {
		delete(h.state.Accounts, account)
		delete(h.state.Sources, source)
		_ = h.save()
		http.Error(w, "faucet transfer failed", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"account": account, "amount": h.Amount, "unit": "Spark-subunit", "network": "testnet", "value": "none"})
}

func (h *Handler) load() error {
	if h.loaded {
		return nil
	}
	h.state = persistedState{Accounts: map[string]int64{}, Sources: map[string]int64{}}
	raw, err := os.ReadFile(h.StatePath)
	if os.IsNotExist(err) {
		h.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	info, err := os.Stat(h.StatePath)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("faucet state must be mode 0600")
	}
	if json.Unmarshal(raw, &h.state) != nil || h.state.Accounts == nil || h.state.Sources == nil {
		return errors.New("invalid faucet state")
	}
	h.loaded = true
	return nil
}

func (h *Handler) save() error {
	raw, err := json.MarshalIndent(h.state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err = os.MkdirAll(filepath.Dir(h.StatePath), 0700); err != nil {
		return err
	}
	tmp := h.StatePath + ".tmp"
	if err = os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, h.StatePath); err != nil {
		return err
	}
	return os.Chmod(h.StatePath, 0600)
}

func sourceIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err == nil {
		remote = host
	}
	if ip := net.ParseIP(remote); ip != nil {
		return ip.String()
	}
	return ""
}

func durationSeconds(value time.Duration) string {
	seconds := int64(value / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}

// PublicMux exposes only the rate-limited funding path. Sharing the handler
// preserves one account/source quota across the private and public listeners.
func PublicMux(handler *Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/testnet/faucet", handler)
	return mux
}
