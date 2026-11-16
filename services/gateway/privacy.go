package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// PrivacyProxy has no decryption keys and no plaintext inference callbacks.
// Register an operator endpoint locally; P1 has no capable route in this release.
func PrivacyProxy(endpoint string, capture func([]byte, []byte)) (http.Handler, error) {
	return privacyProxy(endpoint, capture, &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
}

func privacyProxy(endpoint string, capture func([]byte, []byte), client *http.Client) (http.Handler, error) {
	target, err := url.Parse(endpoint)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil || target.RawQuery != "" {
		return nil, errors.New("invalid privacy executor endpoint")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/requests" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1048576))
		if err != nil {
			http.Error(w, "invalid envelope", 400)
			return
		}
		allowed := false
		for _, b := range []int{4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576} {
			if len(raw) == b {
				allowed = true
			}
		}
		var q struct {
			Envelope struct {
				Version int    `json:"version"`
				Tier    string `json:"tier"`
			} `json:"envelope"`
			Blob string `json:"blob"`
		}
		if !allowed || json.Unmarshal(raw, &q) != nil || q.Envelope.Version != 2 || q.Blob == "" {
			http.Error(w, "invalid envelope", 400)
			return
		}
		if q.Envelope.Tier == "P1" {
			http.Error(w, "TIER_UNAVAILABLE", 503)
			return
		}
		if q.Envelope.Tier != "P0" {
			http.Error(w, "invalid tier", 400)
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), "POST", target.String(), bytes.NewReader(raw))
		if err != nil {
			http.Error(w, "upstream unavailable", 503)
			return
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			http.Error(w, "upstream unavailable", 503)
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		if err != nil {
			http.Error(w, "upstream unavailable", 503)
			return
		}
		if capture != nil {
			capture(raw, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
	}), nil
}
