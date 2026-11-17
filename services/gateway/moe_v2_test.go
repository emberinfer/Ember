package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func TestMoeReceiptV2ExecutorFlag(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			g, b := moeFixture(t)
			if g.Moe.ReceiptV2 {
				t.Fatal("compact transport enabled by default")
			}
			g.Moe.ReceiptV2 = enabled
			receipt := &pb.PipelineExecutionReceipt{RequestId: "hold", InputTokens: 3, OutputTokens: 1, Moe: &pb.MoeExecutionData{}}
			raw, _ := proto.Marshal(receipt)
			requestPath := filepath.Join(g.Moe.WorkDir, "executor-request.json")
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$2\" = tokenize_prompt ]; then\n echo '{\"input_tokens\":3,\"tokenizer_version\":\"test\"}'\nelse\n cat > %q\n echo '{\"type\":\"token\",\"text\":\"Paris\"}'\n echo '{\"type\":\"result\",\"output_tokens\":1,\"receipt_b64\":\"%s\"}'\nfi\n", requestPath, base64.StdEncoding.EncodeToString(raw))
			g.Moe.Python = filepath.Join(g.Moe.WorkDir, "executor")
			if err := os.WriteFile(g.Moe.Python, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			g.serveMoe(w, httptest.NewRequest("POST", "/", nil), request{Model: MoeProfile, MaxTokens: 2}, "prompt", "payer", "hold")
			if !b.submitted {
				t.Fatal("executor receipt was not submitted", w.Body.String())
			}
			input, err := os.ReadFile(requestPath)
			if err != nil {
				t.Fatal(err)
			}
			var q map[string]any
			if err = json.Unmarshal(input, &q); err != nil {
				t.Fatal(err)
			}
			if q["receipt_v2"] != enabled {
				t.Fatal("gateway flag not passed to executor", q)
			}
		})
	}
}
