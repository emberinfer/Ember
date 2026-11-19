package faucet

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFaucetTransfersOnceAndPersistsRateLimit(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "faucet.json")
	now := time.Unix(100000, 0)
	account := strings.Repeat("a", 64)
	transfers := 0
	handler := &Handler{Amount: 25, Window: time.Hour, StatePath: statePath, Now: func() time.Time { return now },
		Transfer: func(got string, amount int64) error {
			if amount != 25 {
				t.Fatalf("transfer account=%s amount=%d", got, amount)
			}
			transfers++
			return nil
		}}
	request := func(target *Handler, remote, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/testnet/faucet", strings.NewReader(body))
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		target.ServeHTTP(w, r)
		return w
	}
	if got := request(handler, "192.0.2.1:1234", `{"account":"`+account+`"}`); got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body.String())
	}
	if transfers != 1 {
		t.Fatalf("transfers=%d", transfers)
	}
	reloaded := &Handler{Amount: 25, Window: time.Hour, StatePath: statePath, Now: func() time.Time { return now.Add(time.Minute) }, Transfer: handler.Transfer}
	if got := request(reloaded, "192.0.2.2:4321", `{"account":"`+account+`"}`); got.Code != http.StatusTooManyRequests {
		t.Fatal(got.Code, got.Body.String())
	}
	if got := request(reloaded, "192.0.2.1:9999", `{"account":"`+strings.Repeat("b", 64)+`"}`); got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body.String())
	}
	// The limit is per account: a fresh account behind the same source
	// IP succeeds; the source is recorded as telemetry only.
	if transfers != 2 {
		t.Fatalf("transfers=%d", transfers)
	}
}

func TestFaucetRejectsInvalidAccount(t *testing.T) {
	handler := &Handler{Amount: 1, StatePath: filepath.Join(t.TempDir(), "state.json"), Transfer: func(string, int64) error { return nil }}
	r := httptest.NewRequest(http.MethodPost, "/testnet/faucet", strings.NewReader(`{"account":"not-an-account"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestTrustedProxyForwardedSourceParsing(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "untrusted", true: "trusted"}[trusted], func(t *testing.T) {
			h := &Handler{Amount: 1, StatePath: filepath.Join(t.TempDir(), "state.json"),
				TrustedProxy: "127.0.0.1", Transfer: func(string, int64) error { return nil }}
			remote := "198.51.100.9:1234"
			if trusted {
				remote = "127.0.0.1:1234"
			}
			claim := func(account, forwarded string) int {
				r := httptest.NewRequest("POST", "/testnet/faucet", strings.NewReader(`{"account":"`+strings.Repeat(account, 64)+`"}`))
				r.RemoteAddr = remote
				r.Header.Set("X-Forwarded-For", forwarded)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w.Code
			}
			if got := claim("a", "192.0.2.1"); got != 200 {
				t.Fatal(got)
			}
			// Distinct accounts are never gated by source reuse, whether
			// the forwarded source is new, reused, or (untrusted) ignored.
			if got := claim("b", "192.0.2.2"); got != 200 {
				t.Fatalf("distinct account, new source: got %d want 200", got)
			}
			if got := claim("c", "192.0.2.1"); got != 200 {
				t.Fatalf("distinct account, reused source: got %d want 200", got)
			}
			if got := claim("a", "192.0.2.3"); got != 429 {
				t.Fatal("account quota bypass", got)
			}
			if trusted && claim("d", "192.0.2.4, 192.0.2.5") != 400 {
				t.Fatal("accepted ambiguous forwarded source")
			}
		})
	}
}

func TestPublicListenerExposesOnlyFaucetAndSharesQuota(t *testing.T) {
	h := &Handler{Amount: 1000, Window: time.Hour, StatePath: filepath.Join(t.TempDir(), "state.json"), Transfer: func(string, int64) error { return nil }}
	public := PublicMux(h)
	for _, path := range []string{"/v1/chat/completions", "/metrics", "/seed/peers", "/readyz"} {
		w := httptest.NewRecorder()
		public.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("exposed %s: %d", path, w.Code)
		}
	}
	body := `{"account":"` + strings.Repeat("a", 64) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/testnet/faucet", strings.NewReader(body))
	r.RemoteAddr = "198.51.100.1:1234"
	w := httptest.NewRecorder()
	public.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	r = httptest.NewRequest(http.MethodPost, "/testnet/faucet", strings.NewReader(body))
	r.RemoteAddr = "198.51.100.2:1234"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 429 {
		t.Fatal("private listener bypassed shared quota", w.Code)
	}
}

func TestFaucetTransferErrorKeepsRateLimitRecord(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "faucet.json")
	account := strings.Repeat("a", 64)
	calls := 0
	fail := true
	handler := &Handler{Amount: 25, Window: time.Hour, StatePath: statePath,
		Transfer: func(string, int64) error {
			calls++
			if fail {
				return errors.New("broadcast outcome unknown")
			}
			return nil
		}}
	claim := func(target *Handler, acct string) int {
		r := httptest.NewRequest(http.MethodPost, "/testnet/faucet", strings.NewReader(`{"account":"`+acct+`"}`))
		r.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		target.ServeHTTP(w, r)
		return w.Code
	}
	if got := claim(handler, account); got != http.StatusServiceUnavailable {
		t.Fatalf("failed transfer: got %d want 503", got)
	}
	// The record must stand after the error: a retry is rate-limited and
	// the transfer is never attempted a second time for this account.
	fail = false
	if got := claim(handler, account); got != http.StatusTooManyRequests {
		t.Fatalf("retry after transfer error: got %d want 429", got)
	}
	if calls != 1 {
		t.Fatalf("transfer calls=%d want 1", calls)
	}
	// The record is persisted, not just in memory.
	reloaded := &Handler{Amount: 25, Window: time.Hour, StatePath: statePath, Transfer: handler.Transfer}
	if got := claim(reloaded, account); got != http.StatusTooManyRequests {
		t.Fatalf("reloaded retry: got %d want 429", got)
	}
	// Other accounts are unaffected.
	if got := claim(handler, strings.Repeat("b", 64)); got != http.StatusOK {
		t.Fatalf("other account: got %d want 200", got)
	}
}
