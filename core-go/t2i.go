package core

// Frozen T0 reference semantics; see docs/T2I-SPEC-V1.md.
import (
	"encoding/binary"
	"errors"
	"math"
	"unicode/utf8"
)

// All arithmetic uses explicit round-to-f32 boundaries; no fused operations.
func t2f(x float64) float32      { return float32(x) }
func t2add(a, b float32) float32 { return t2f(float64(a) + float64(b)) }
func t2sub(a, b float32) float32 { return t2f(float64(a) - float64(b)) }
func t2mul(a, b float32) float32 { return t2f(float64(a) * float64(b)) }
func t2div(a, b float32) float32 { return t2f(float64(a) / float64(b)) }

// T2IPhilox maps each element index to one independent four-word block.
func T2IPhilox(seed, index uint64) [4]uint32 {
	c0, c1, c2, c3 := uint32(index), uint32(index>>32), uint32(0), uint32(0)
	k0, k1 := uint32(seed), uint32(seed>>32)
	for r := 0; r < 10; r++ {
		p0, p1 := uint64(0xd2511f53)*uint64(c0), uint64(0xcd9e8d57)*uint64(c2)
		c0, c1, c2, c3 = uint32(p1>>32)^c1^k0, uint32(p1), uint32(p0>>32)^c3^k1, uint32(p0)
		k0 += 0x9e3779b9
		k1 += 0xbb67ae85
	}
	return [4]uint32{c0, c1, c2, c3}
}
func t2log(x float32) float32 {
	b := math.Float32bits(x)
	e := int(b>>23) - 127
	m := math.Float32frombits((b & 0x7fffff) | 0x3f800000)
	z := t2div(t2sub(m, 1), t2add(m, 1))
	z2 := t2mul(z, z)
	p, s := z, z
	for k := 1; k <= 12; k++ {
		p = t2mul(p, z2)
		s = t2add(s, t2div(p, float32(2*k+1)))
	}
	return t2add(t2mul(float32(e), math.Float32frombits(0x3f317218)), t2mul(2, s))
}
func t2sqrt(x float32) float32 {
	if x == 0 {
		return 0
	}
	y := math.Float32frombits((math.Float32bits(x) >> 1) + 0x1fc00000)
	for k := 0; k < 8; k++ {
		y = t2mul(.5, t2add(y, t2div(x, y)))
	}
	return y
}
func t2cos(theta float32) float32 {
	pi, twoPi, halfPi := math.Float32frombits(0x40490fdb), math.Float32frombits(0x40c90fdb), math.Float32frombits(0x3fc90fdb)
	x, sign := theta, float32(1)
	if x > pi {
		x = t2sub(x, twoPi)
	}
	if x > halfPi {
		x, sign = t2sub(pi, x), -1
	} else if x < -halfPi {
		x, sign = t2sub(-pi, x), -1
	}
	x2 := t2mul(x, x)
	term, total := float32(1), float32(1)
	for k := 1; k <= 12; k++ {
		term = t2div(t2mul(-term, x2), float32((2*k-1)*(2*k)))
		total = t2add(total, term)
	}
	return t2mul(sign, total)
}
func t2count(shape [4]uint64) (int, error) {
	if shape[0] != 1 {
		return 0, errors.New("expected batch=1")
	}
	n := uint64(1)
	for _, d := range shape {
		if d == 0 || d > uint64(int(^uint(0)>>1))/n {
			return 0, errors.New("invalid shape")
		}
		n *= d
	}
	return int(n), nil
}

// T2IInitLatent returns full-shape (1,C,h,w) row-major fp32 SD1.5 init.
func T2IInitLatent(seed uint64, shape [4]uint64) ([]float32, error) {
	n, err := t2count(shape)
	if err != nil {
		return nil, err
	}
	out := make([]float32, n)
	for i := range out {
		words := T2IPhilox(seed, uint64(i))
		u0, u1 := float32(words[0]>>8)*0x1p-24, float32(words[1]>>8)*0x1p-24
		radius := t2sqrt(t2mul(-2, t2log(t2sub(1, u0))))
		theta := t2mul(math.Float32frombits(0x40c90fdb), u1)
		out[i] = t2mul(t2mul(radius, t2cos(theta)), math.Float32frombits(0x3e3a8588))
	}
	return out, nil
}

// T2ISerializeLatent uses the existing ETV1 f32 audit format.
// SD1.5 v1 permits f32 only. Future dtype profiles require their own vectors.
func T2ISerializeLatent(values []float32, shape [4]uint64) ([]byte, error) {
	n, err := t2count(shape)
	if err != nil || n != len(values) {
		return nil, errors.New("invalid payload shape")
	}
	out := make([]byte, 38+4*n)
	copy(out, []byte{'E', 'T', 'V', '1', 1, 4})
	for i, d := range shape {
		binary.BigEndian.PutUint64(out[6+8*i:], d)
	}
	for i, v := range values {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, errors.New("nonfinite latent")
		}
		binary.LittleEndian.PutUint32(out[38+4*i:], math.Float32bits(v))
	}
	return out, nil
}

// T2ITokenizer must return pinned SD1.5 content IDs without specials or padding.
type T2ITokenizer func(string) ([]uint32, error)

func T2IEncodePrompt(text string, tokenize T2ITokenizer) ([]uint32, error) {
	if !utf8.ValidString(text) || tokenize == nil {
		return nil, errors.New("invalid prompt/tokenizer")
	}
	tokens, err := tokenize(text)
	if err != nil {
		return nil, err
	}
	if len(tokens) > 75 {
		return nil, errors.New("prompt exceeds sequence length")
	}
	out := make([]uint32, 77)
	out[0] = 49406
	for i := 1; i < 77; i++ {
		out[i] = 49407
	}
	for i, t := range tokens {
		if t >= 49406 {
			return nil, errors.New("invalid SD1.5 content token")
		}
		out[i+1] = t
	}
	return out, nil
}
func T2IEncodePrompts(positive, negative string, tokenize T2ITokenizer) ([2][]uint32, error) {
	var out [2][]uint32
	for i, text := range []string{positive, negative} {
		ids, err := T2IEncodePrompt(text, tokenize)
		if err != nil {
			return out, err
		}
		out[i] = ids
	}
	return out, nil
}
