package ledger

import (
	chainapp "ember/services/chain"
	"encoding/json"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExecExitedNodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		if q.Params.Path != "/node-statuses" {
			t.Error(q.Params.Path)
		}
		data, _ := json.Marshal(map[string]*chainapp.NodeStatusView{"pending": {ExecExitPending: true, ExecExitEffectiveHeight: 1010}, "exited": {ExecExited: true}, "normal": {}, "nil": nil})
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": map[string]any{"response": map[string]any{"code": 0, "value": data}}})
	}))
	defer server.Close()
	rpc, err := rpchttp.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	l := &ChainLedger{rpc: rpc}
	got, err := l.ExecExitedNodes()
	if err != nil || len(got) != 1 || !got["exited"] {
		t.Fatal(got, err)
	}
}
