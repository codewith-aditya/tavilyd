package rerank

import "testing"

func TestTokenizeDropsStopwords(t *testing.T) {
	tokens := tokenize("how to make paneer at home")
	for _, bad := range []string{"how", "to", "make", "at", "home"} {
		for _, tok := range tokens {
			if tok == bad {
				t.Fatalf("stopword %q survived: %v", bad, tokens)
			}
		}
	}
	found := false
	for _, tok := range tokens {
		if tok == "paneer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("paneer missing: %v", tokens)
	}
}

// Regression for the paneer query: make.com must not outrank a paneer recipe.
func TestBM25PaneerRegression(t *testing.T) {
	b := NewBM25()
	docs := []string{
		"Make | Automation platform. Build workflows and integrations with make.com.",
		"Make (English word) - encyclopedia entry about the verb.",
		"How to make paneer at home: step-by-step recipe with milk and lemon.",
	}
	scores := b.Score("how to make paneer at home", docs)
	if !(scores[2] > scores[0] && scores[2] > scores[1]) {
		t.Fatalf("paneer recipe did not win: %v", scores)
	}
}
