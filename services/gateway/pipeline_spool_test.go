package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/auditqueue"
	"ember/services/jsonspool"
	"google.golang.org/protobuf/proto"
)

func TestPipelineResultUsesDisk(t *testing.T) {
	dir := t.TempDir()
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		io.WriteString(w, `{"type":"token","text":"x"}`+"\n"+`{"type":"result","invocations":[`)
		payload := strings.Repeat("ab", 128<<10)
		for i := 0; i < 128; i++ {
			if i > 0 {
				io.WriteString(w, ",")
			}
			fmt.Fprintf(w, `{"request":"%s"}`, payload)
		}
		io.WriteString(w, "]}\n")
	}()
	scan := newEventReader(r, dir)
	defer scan.Close()
	if !scan.Scan() || string(scan.Bytes()) != "{\"type\":\"token\",\"text\":\"x\"}\n" {
		t.Fatal("small frame changed", scan.Err())
	}
	if !scan.Scan() || scan.file == nil || len(scan.Bytes()) != 0 {
		t.Fatal("result must use disk", scan.Err())
	}
	st, err := scan.file.Stat()
	if err != nil || st.Size() < 32<<20 {
		t.Fatal(st, err)
	}
	source, err := jsonspool.Index(scan.file)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.Arrays["invocations"]) != 128 {
		t.Fatal("lost invocations")
	}
	scan.Close()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("leaked spool", err)
	}
}

func TestPipelineAuditSpoolBytes(t *testing.T) {
	raw := `{"type":"result","openings": [ { "s": "<a>&", "n": 1.0 } ], "invocations":[{"request":"ABCD","response":"01","profile":"FF","stage_ms":1.0}],"failovers": []}`
	f, err := os.CreateTemp(t.TempDir(), "frame")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(raw)
	source, err := jsonspool.Index(f)
	if err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"kind":"pipeline","openings":null,"invocations":[{}],"failovers":null,"prompt":"x"}`)
	var got bytes.Buffer
	if err = WritePipelineAudit(&got, meta, source); err != nil {
		t.Fatal(err)
	}
	expected := `{"kind":"pipeline","openings":[{"s":"\u003ca\u003e\u0026","n":1.0}],"invocations":[{"request":"abcd","response":"01","profile":"ff","stage_ms":1}],"failovers":[],"prompt":"x"}`
	if got.String() != expected {
		t.Fatalf("audit bytes changed\n%s\n%s", got.String(), expected)
	}
}

// Set EMBER_PIPELINE_GOLDEN to replay an external real executor transcript. The
// standalone test binary can be measured by time -v independently of Go builds.
func TestPipelineSpoolGolden(t *testing.T) {
	path := os.Getenv("EMBER_PIPELINE_GOLDEN")
	if path == "" {
		t.Skip("external golden transcript")
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	dir := os.Getenv("EMBER_PIPELINE_SCRATCH")
	if dir == "" {
		dir = t.TempDir()
	}
	scan := newEventReader(input, dir)
	defer scan.Close()
	var source *jsonspool.Object
	tokens := 0
	for scan.Scan() {
		if scan.file != nil {
			indexed, e := jsonspool.Index(scan.file)
			if e != nil {
				t.Fatal(e)
			}
			header, e := decodeSpoolEvent(indexed)
			if e != nil {
				t.Fatal(e)
			}
			if header.Type == "result" {
				if source != nil {
					t.Fatal("duplicate result")
				}
				source = indexed
				scan.file = nil
				continue
			}
		}
		var event pipelineEvent
		if err = json.Unmarshal(scan.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "token" {
			tokens++
		}
	}
	if err = scan.Err(); err != nil {
		t.Fatal(err)
	}
	if source == nil {
		t.Fatal("no result")
	}
	defer func() { source.File.Close(); os.Remove(source.File.Name()) }()
	var receiptHex string
	if err = source.Decode("receipt", &receiptHex); err != nil {
		t.Fatal(err)
	}
	wire, err := hex.DecodeString(receiptHex)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &pb.PipelineExecutionReceipt{}
	if err = proto.Unmarshal(wire, receipt); err != nil {
		t.Fatal(err)
	}
	e := PipelineExecution{Source: source, Receipt: receipt}
	for i := 0; i < e.InvocationCount(); i++ {
		if _, err = e.ReadInvocation(i); err != nil {
			t.Fatal(err)
		}
	}
	first := receipt.StageReceipts[0]
	verify := func(count int, read func(int) (core.PipelineInvocation, error)) error {
		_, err := core.VerifyPipelineExecutionStream(receipt, count, read, first.ChainId, first.ManifestRoot, first.ExecutionProfileVer, 28, dir)
		return err
	}
	if err = verify(e.InvocationCount(), e.ReadInvocation); err != nil {
		t.Fatal(err)
	}
	if tokens != int(receipt.OutputTokens) {
		t.Fatal("token mismatch")
	}
	if os.Getenv("EMBER_PIPELINE_COMPARE") == "1" {
		inv := make([]core.PipelineInvocation, e.InvocationCount())
		for i := range inv {
			inv[i], err = e.ReadInvocation(i)
			if err != nil {
				t.Fatal(err)
			}
		}
		cases := []struct {
			name   string
			values []core.PipelineInvocation
		}{{"valid", inv}, {"deleted", inv[:len(inv)-1]}}
		changed := append([]core.PipelineInvocation(nil), inv...)
		changed[0].Request = append([]byte(nil), changed[0].Request...)
		changed[0].Request[len(changed[0].Request)-1] ^= 1
		swapped := append([]core.PipelineInvocation(nil), inv...)
		swapped[0], swapped[1] = swapped[1], swapped[0]
		cases = append(cases, struct {
			name   string
			values []core.PipelineInvocation
		}{"tampered", changed}, struct {
			name   string
			values []core.PipelineInvocation
		}{"reordered", swapped})
		for _, c := range cases {
			_, oldErr := core.VerifyPipelineExecution(receipt, c.values, first.ChainId, first.ManifestRoot, first.ExecutionProfileVer, 28)
			newErr := verify(len(c.values), func(i int) (core.PipelineInvocation, error) { return c.values[i], nil })
			if fmt.Sprint(oldErr) != fmt.Sprint(newErr) {
				t.Fatalf("%s old=%v new=%v", c.name, oldErr, newErr)
			}
			if c.name != "valid" && newErr == nil {
				t.Fatal("accepted", c.name)
			}
			t.Logf("comparison %s old=%v new=%v", c.name, oldErr, newErr)
		}
	}
	// Exercise durable submission, recovery and streaming audit package assembly.
	queue, err := auditqueue.Open(dir + "/audit")
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Done(receipt.RequestId)
	meta := []byte(`{"kind":"pipeline","openings":null,"invocations":[{}],"failovers":null}`)
	h := sha256.New()
	if err = queue.PutStream(receipt.RequestId, func(w io.Writer) error { return WritePipelineAudit(io.MultiWriter(w, h), meta, source) }); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("EMBER_PIPELINE_COMPARE") == "1" {
		var rows []invocationJSON
		var openings, failovers json.RawMessage
		if err = source.Decode("invocations", &rows); err != nil {
			t.Fatal(err)
		}
		if err = source.Decode("openings", &openings); err != nil {
			t.Fatal(err)
		}
		if err = source.Decode("failovers", &failovers); err != nil {
			t.Fatal(err)
		}
		legacy, err := json.Marshal(struct {
			Kind        string           `json:"kind"`
			Openings    json.RawMessage  `json:"openings"`
			Invocations []invocationJSON `json:"invocations"`
			Failovers   json.RawMessage  `json:"failovers"`
		}{"pipeline", openings, rows, failovers})
		if err != nil {
			t.Fatal(err)
		}
		expected := sha256.Sum256(legacy)
		if !bytes.Equal(expected[:], h.Sum(nil)) {
			t.Fatal("legacy audit package bytes changed")
		}
		t.Logf("legacy audit bytes identical sha256=%x", expected)
	}
	stored, err := queue.OpenJob(receipt.RequestId)
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	recovered, err := jsonspool.Index(stored)
	if err != nil {
		t.Fatal(err)
	}
	h2 := sha256.New()
	if err = WritePipelineAudit(h2, meta, recovered); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.Sum(nil), h2.Sum(nil)) {
		t.Fatal("audit recovery changed bytes")
	}
	t.Logf("golden=%s input=%d output=%d invocations=%d OK audit_sha256=%x", path, receipt.InputTokens, receipt.OutputTokens, e.InvocationCount(), h.Sum(nil))
}

func TestPipelineSpoolSchemaParity(t *testing.T) {
	cases := []string{
		`{"TYPE":"result","Receipt":"ab","receipt":null,"invocations":[]}`,
		`{"type":"result","receipt":123}`,
		`{"type":"result","invocations":[{"stage_ms":"bad"}]}`,
		`{"type":"result","invocations":{}}`,
		`{"type":"result","invocations":[{"request":123}],"invocations":[]}`,
		`{"type":"result","invocations":[],"invocations":null}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			var old pipelineEvent
			oldErr := json.Unmarshal([]byte(raw), &old)
			f, err := os.CreateTemp(t.TempDir(), "frame")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			f.WriteString(raw)
			source, err := jsonspool.Index(f)
			if err != nil {
				t.Fatal(err)
			}
			got, newErr := decodeSpoolEvent(source)
			if fmt.Sprint(oldErr) != fmt.Sprint(newErr) {
				t.Fatalf("old=%v new=%v", oldErr, newErr)
			}
			if oldErr == nil && (old.Type != got.Type || old.Receipt != got.Receipt || old.Text != got.Text) {
				t.Fatal("header changed")
			}
		})
	}
}
