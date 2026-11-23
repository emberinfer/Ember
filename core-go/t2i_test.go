package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

type t2iVector struct {
	Seed      string    `json:"seed"`
	Shape     [4]uint64 `json:"shape"`
	SHA       string    `json:"sha256"`
	RawSHA    string    `json:"raw_sha256"`
	FirstBits []string  `json:"first_bits"`
}

func t2iVectors(t *testing.T) []t2iVector {
	t.Helper()
	raw, err := os.ReadFile("../testdata/vectors/t2i-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []t2iVector `json:"vectors"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Vectors) != 15 {
		t.Fatal("expected 15 frozen vectors")
	}
	return fixture.Vectors
}
func TestT2IVectorsAndPythonBytes(t *testing.T) {
	var all bytes.Buffer
	for _, v := range t2iVectors(t) {
		seed, err := strconv.ParseUint(v.Seed, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		values, err := T2IInitLatent(seed, v.Shape)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := T2ISerializeLatent(values, v.Shape)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			raw  []byte
			hash string
		}{{wire, v.SHA}, {wire[38:], v.RawSHA}} {
			sum := sha256.Sum256(test.raw)
			if hex.EncodeToString(sum[:]) != test.hash {
				t.Fatalf("seed=%s shape=%v hash mismatch", v.Seed, v.Shape)
			}
		}
		for i, bits := range v.FirstBits {
			u, err := strconv.ParseUint(bits, 16, 32)
			if err != nil || math.Float32bits(values[i]) != uint32(u) {
				t.Fatalf("first bits seed=%s i=%d", v.Seed, i)
			}
		}
		all.Write(wire)
	}
	python := os.Getenv("EMBER_AUDIT_TEST_PYTHON")
	if python == "" {
		python = "python3"
	}
	cmd := exec.Command(python, "../testdata/vectors/generate_t2i.py", "--binary")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	actual, err := cmd.Output()
	if err != nil {
		t.Fatalf("Python reference: %v %s", err, stderr.String())
	}
	if !bytes.Equal(actual, all.Bytes()) {
		t.Fatal("Go/Python full serialized bytes differ")
	}
	t.Logf("all %d serialized bytes identical across Go and Python", len(actual))
}
func TestT2IPhilox(t *testing.T) {
	if T2IPhilox(0, 0) != [4]uint32{0x6627e8d5, 0xe169c58d, 0xbc57ac4c, 0x9b00dbd8} {
		t.Fatal("Random123 known answer")
	}
	if T2IPhilox(0, 1) == T2IPhilox(0, 1<<32) {
		t.Fatal("counter high word ignored")
	}
}
func TestT2IPromptBoundaries(t *testing.T) {
	var calls []string
	tokenize := func(text string) ([]uint32, error) {
		calls = append(calls, text)
		out := make([]uint32, len(text))
		for i := range out {
			out[i] = 12
		}
		return out, nil
	}
	ids, err := T2IEncodePrompts(strings.Repeat("a", 75), "", tokenize)
	if err != nil || len(calls) != 2 || calls[1] != "" {
		t.Fatal("empty negative must be encoded", err, calls)
	}
	if len(ids[0]) != 77 || ids[0][0] != 49406 || ids[0][75] != 12 || ids[0][76] != 49407 {
		t.Fatal("exactly full prompt")
	}
	if ids[1][0] != 49406 {
		t.Fatal("negative BOS")
	}
	for _, id := range ids[1][1:] {
		if id != 49407 {
			t.Fatal("empty negative padding")
		}
	}
	if _, err = T2IEncodePrompt(strings.Repeat("a", 76), tokenize); err == nil {
		t.Fatal("must reject overlength")
	}
	if _, err = T2IEncodePrompt(string([]byte{255}), tokenize); err == nil {
		t.Fatal("invalid UTF8")
	}
	if _, err = T2IEncodePrompt("x", func(string) ([]uint32, error) { return []uint32{49407}, nil }); err == nil {
		t.Fatal("special content ID")
	}
}
func TestT2ISerialization(t *testing.T) {
	shape := [4]uint64{1, 1, 1, 4}
	wire, err := T2ISerializeLatent([]float32{0, math.Float32frombits(0x80000000), 1, -2}, shape)
	if err != nil || hex.EncodeToString(wire[38:]) != "00000000000000800000803f000000c0" {
		t.Fatal("little endian payload", err)
	}
	if !bytes.Equal(wire[:6], []byte{'E', 'T', 'V', '1', 1, 4}) {
		t.Fatal("ETV1 header")
	}
	if _, err = T2ISerializeLatent([]float32{1}, shape); err == nil {
		t.Fatal("length mismatch")
	}
	if _, err = T2ISerializeLatent([]float32{float32(math.NaN())}, [4]uint64{1, 1, 1, 1}); err == nil {
		t.Fatal("nonfinite")
	}
	for _, s := range [][4]uint64{{2, 4, 64, 64}, {1, 4, 0, 64}, {1, math.MaxUint64, 2, 2}} {
		if _, err = T2IInitLatent(0, s); err == nil {
			t.Fatal("invalid shape")
		}
	}
}
