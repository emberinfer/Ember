package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrivacyFailClosed(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: privacyRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"output":"encrypted"}`)), Header: http.Header{}}, nil
	})}
	handler, e := privacyProxy("http://executor/v2/requests", nil, client)
	if e != nil {
		t.Fatal(e)
	}
	for _, tier := range []string{"P1", "invalid", "P0"} {
		body := `{"envelope":{"version":2,"tier":"` + tier + `"},"blob":"encrypted","padding":"`
		body += strings.Repeat("0", 4096-len(body)-2) + `"}`
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/v2/requests", strings.NewReader(body)))
		if tier == "P1" && (w.Code != 503 || !strings.Contains(w.Body.String(), "TIER_UNAVAILABLE")) {
			t.Fatal(w.Code, w.Body)
		}
		if tier == "invalid" && w.Code != 400 {
			t.Fatal(w.Code)
		}
		if tier == "P0" && w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if calls != 1 {
		t.Fatal("invalid tier reached executor")
	}
}

type privacyRoundTrip func(*http.Request) (*http.Response, error)

func (f privacyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
