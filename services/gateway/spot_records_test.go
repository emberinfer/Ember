package gateway

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	pb "ember/proto"
	"ember/services/jsonspool"
	"google.golang.org/protobuf/proto"
)

// Real signed two-stage records, Go indexed spool traversal versus B library.
func TestPipelineRecordRootPython(t *testing.T) {
	python := os.Getenv("EMBER_AUDIT_TEST_PYTHON")
	if python == "" {
		t.Skip("EMBER_AUDIT_TEST_PYTHON required")
	}
	cmd := exec.Command(python, "-c", `
import json
from spotcheck import canonical, pipeline_commitment
with open('../../node/auditor/testdata/pipeline-record-v1.json') as f:
 golden=json.load(f)
j=golden['job']
raws=[canonical(v) for v in j['openings']]
m=pipeline_commitment('ab'*32,'tiny',j['invocations'],raws,2)
assert m==golden['manifest']
print(json.dumps(dict(job=j,manifest=m),sort_keys=True,separators=(',',':')))
`)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join("..", "..", "node")+":"+filepath.Join("..", "..", "core-py")+":"+filepath.Join("..", "..", "proto"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	var result struct {
		Job      json.RawMessage        `json:"job"`
		Manifest PipelineSpotCommitment `json:"manifest"`
	}
	if err = json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "records")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(result.Job); err != nil {
		t.Fatal(err)
	}
	source, err := jsonspool.Index(f)
	if err != nil {
		t.Fatal(err)
	}
	var receiptHex string
	if err = source.Decode("pipeline_receipt", &receiptHex); err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(receiptHex)
	if err != nil {
		t.Fatal(err)
	}
	receipt := new(pb.PipelineExecutionReceipt)
	if err = proto.Unmarshal(raw, receipt); err != nil {
		t.Fatal(err)
	}
	execution := &PipelineExecution{Source: source, Receipt: receipt}
	got, err := BuildPipelineSpotCommitment(execution, result.Manifest.RequestID, "tiny")
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(result.Manifest)
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("cross-language commitment differs\ngot %s\nwant %s", gotJSON, wantJSON)
	}
	receipt.OutputTokens = 1
	if _, err = BuildPipelineSpotCommitment(execution, result.Manifest.RequestID, "tiny"); err == nil {
		t.Fatal("N mismatch accepted")
	}
	receipt.OutputTokens = 2
	execution.Source.Arrays["invocations"] = execution.Source.Arrays["invocations"][2:]
	if _, err = BuildPipelineSpotCommitment(execution, result.Manifest.RequestID, "tiny"); err == nil {
		t.Fatal("missing step accepted")
	}
}
