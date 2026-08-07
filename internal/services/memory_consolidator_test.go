package services

import "testing"

func TestNormalizeContentCollapsesWhitespace(t *testing.T) {
	got := normalizeContent("  Hello\tworld\n\n  ")
	want := "hello world"
	if got != want {
		t.Errorf("normalize: got %q want %q", got, want)
	}
}

func TestContentHashStability(t *testing.T) {
	a := contentHash("Hello World")
	b := contentHash("hello   world")
	if a != b {
		t.Errorf("hash should be invariant to case/whitespace: %v vs %v", a, b)
	}
	c := contentHash("Hello Wo")
	if a == c {
		t.Errorf("different content should produce different hash")
	}
}
