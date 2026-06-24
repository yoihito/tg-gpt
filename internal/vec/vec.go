package vec

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Encode serializes a float32 slice as little-endian bytes for BLOB storage.
func Encode(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// Decode reverses Encode. Returns an error if the byte slice length isn't a multiple of 4.
func Decode(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("vec.Decode: byte length %d is not a multiple of 4", len(b))
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

// Normalize returns v scaled to unit L2 length. Zero vectors are returned unchanged.
func Normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1.0 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = f * inv
	}
	return out
}

// Cosine returns the cosine similarity of two equal-length L2-normalized vectors.
// For normalized vectors, cosine collapses to the dot product.
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}
