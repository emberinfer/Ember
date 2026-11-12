package core

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"

	"google.golang.org/protobuf/proto"
)

const Spark int64 = 1_000_000

func NodeID(public []byte) string { h := sha256.Sum256(public); return hex.EncodeToString(h[:]) }

func JoinChallenge(nonce []byte) []byte { return append([]byte("ember/tunnel-join/v1\x00"), nonce...) }

var signedTypes = map[string]bool{
	"ChainGenesis": true, "ModelManifest": true, "ResourceProfile": true, "ExecutionCommitment": true,
	"ChunkRoot": true, "ExecutionReceipt": true, "AuditChallenge": true, "AuditVerdict": true, "LedgerTx": true, "ShardStepReceipt": true,
	"TunnelFrame": true, "ShardForwardRequest": true, "StageLatencyWitness": true,
}

func field(m proto.Message, name string) (reflect.Value, error) {
	v := reflect.ValueOf(m)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return reflect.Value{}, errors.New("nil signed object")
	}
	f := v.Elem().FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.Slice {
		return reflect.Value{}, fmt.Errorf("unsupported signed type %T", m)
	}
	if !signedTypes[string(m.ProtoReflect().Descriptor().Name())] {
		return reflect.Value{}, fmt.Errorf("unsigned type %T", m)
	}
	return f, nil
}

func payload(m proto.Message) ([]byte, error) {
	clone := proto.Clone(m)
	f, err := field(clone, "Signature")
	if err != nil {
		return nil, err
	}
	f.SetBytes(nil)
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return nil, err
	}
	domain := []byte("ember/signed/v1/" + string(m.ProtoReflect().Descriptor().Name()) + "\x00")
	return append(domain, raw...), nil
}

func Digest(m proto.Message) ([]byte, error) {
	b, e := payload(m)
	if e != nil {
		return nil, e
	}
	h := sha256.Sum256(b)
	return h[:], nil
}

func Sign(m proto.Message, private ed25519.PrivateKey) error {
	if len(private) != ed25519.PrivateKeySize {
		return errors.New("invalid private key")
	}
	pub, _ := field(m, "Signer")
	if !pub.IsValid() {
		return errors.New("invalid signer")
	}
	pub.SetBytes(append([]byte(nil), private.Public().(ed25519.PublicKey)...))
	b, err := payload(m)
	if err != nil {
		return err
	}
	sig, _ := field(m, "Signature")
	sig.SetBytes(ed25519.Sign(private, b))
	return nil
}

func Verify(m proto.Message) error {
	pub, err := field(m, "Signer")
	if err != nil {
		return err
	}
	sig, err := field(m, "Signature")
	if err != nil {
		return err
	}
	if len(pub.Bytes()) != ed25519.PublicKeySize || len(sig.Bytes()) != ed25519.SignatureSize {
		return errors.New("invalid signature length")
	}
	b, err := payload(m)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub.Bytes(), b, sig.Bytes()) {
		return errors.New("signature mismatch")
	}
	return nil
}

// VerifyChunks binds each chunk to its predecessor's signed digest and the final token count.
func VerifyChunks(chunks []proto.Message, requestID string, total uint32, get func(proto.Message) (string, uint32, uint32, []byte, []byte)) error {
	var previous []byte
	var step uint32
	for i, c := range chunks {
		id, seq, upto, prev, root := get(c)
		if err := Verify(c); err != nil {
			return err
		}
		if id != requestID || seq != uint32(i) || upto <= step || upto-step > 16 || !reflect.DeepEqual(prev, previous) || len(root) != 32 {
			return fmt.Errorf("invalid chunk %d", i)
		}
		d, err := Digest(c)
		if err != nil {
			return err
		}
		previous = d
		step = upto
	}
	if step != total {
		return fmt.Errorf("chunk coverage %d != %d", step, total)
	}
	return nil
}

func WithinTolerance(a, b []float64, tolerance float64) bool {
	if len(a) != len(b) || len(a) == 0 || math.IsNaN(tolerance) || tolerance < 0 {
		return false
	}
	var diff, base float64
	for i := range a {
		if math.IsNaN(a[i]) || math.IsNaN(b[i]) || math.IsInf(a[i], 0) || math.IsInf(b[i], 0) {
			return false
		}
		d := a[i] - b[i]
		diff += d * d
		base += b[i] * b[i]
	}
	return math.Sqrt(diff)/math.Max(math.Sqrt(base), 1e-12) <= tolerance
}
