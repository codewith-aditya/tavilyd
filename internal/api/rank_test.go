package api

import (
	"reflect"
	"testing"
)

func TestPreselectKeepsLexicallyStrongestPerDoc(t *testing.T) {
	refs := []chunkRef{
		{doc: 0, text: "a"}, {doc: 0, text: "b"}, {doc: 0, text: "c"},
		{doc: 1, text: "d"}, {doc: 1, text: "e"},
	}
	lexical := []float64{0.1, 0.9, 0.5, 0.2, 0.8}

	basic := preselect(lexical, refs, 2, "basic") // k = 1 per doc
	if want := []int{1, 4}; !reflect.DeepEqual(basic, want) {
		t.Fatalf("basic preselect = %v, want %v", basic, want)
	}

	advanced := preselect(lexical, refs, 2, "advanced") // k = 3 per doc
	if want := []int{0, 1, 2, 3, 4}; !reflect.DeepEqual(advanced, want) {
		t.Fatalf("advanced preselect = %v, want %v", advanced, want)
	}
}

func TestPreselectHandlesZeroScores(t *testing.T) {
	refs := []chunkRef{{doc: 0}, {doc: 0}, {doc: 0}}
	got := preselect([]float64{0, 0, 0}, refs, 1, "basic")
	if len(got) != 1 {
		t.Fatalf("expected 1 chunk, got %v", got)
	}
}
