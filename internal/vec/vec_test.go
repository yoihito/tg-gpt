package vec

import (
	"math"
	"testing"
)

func TestEncodeDecodeRoundtrip(t *testing.T) {
	in := []float32{0, 1, -1, 0.5, math.MaxFloat32, math.SmallestNonzeroFloat32}
	out, err := Decode(Encode(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len mismatch: %d vs %d", len(out), len(in))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("idx %d: in=%v out=%v", i, in[i], out[i])
		}
	}
}

func TestDecodeInvalidLength(t *testing.T) {
	_, err := Decode([]byte{0, 1, 2})
	if err == nil {
		t.Fatal("expected error for non-multiple-of-4 length")
	}
}

func TestNormalizeUnit(t *testing.T) {
	v := Normalize([]float32{3, 4})
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if math.Abs(sum-1.0) > 1e-6 {
		t.Errorf("not unit length: sum=%v", sum)
	}
}

func TestNormalizeZero(t *testing.T) {
	v := Normalize([]float32{0, 0, 0})
	for _, f := range v {
		if f != 0 {
			t.Errorf("zero vector should remain zero, got %v", f)
		}
	}
}

func TestCosineIdentity(t *testing.T) {
	a := Normalize([]float32{1, 2, 3})
	if got := Cosine(a, a); math.Abs(float64(got)-1.0) > 1e-6 {
		t.Errorf("self-cosine should be 1, got %v", got)
	}
}

func TestCosineOrthogonal(t *testing.T) {
	a := Normalize([]float32{1, 0})
	b := Normalize([]float32{0, 1})
	if got := Cosine(a, b); math.Abs(float64(got)) > 1e-6 {
		t.Errorf("orthogonal cosine should be 0, got %v", got)
	}
}

func TestCosineLengthMismatch(t *testing.T) {
	if got := Cosine([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Errorf("mismatched length should return 0, got %v", got)
	}
}
