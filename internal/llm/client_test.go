package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStripThink(t *testing.T) {
	cases := map[string]string{
		"  visible":                   "visible",
		"a  thinkingsecret</think> b": "a  b",
		"text\n thinkingunfinished":   "text",
		"no think":                    "no think",
	}
	for in, want := range cases {
		if got := StripThink(in); got != want {
			t.Errorf("StripThink(%q)=%q want %q", in, got, want)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	in := "Here you go:\n```json\n{\"a\": \"x}y\", \"b\": [1,2]}\n```\ntrailing"
	raw, err := ExtractJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil || v["a"] != "x}y" {
		t.Fatalf("bad extraction: %s", raw)
	}
	if _, err := ExtractJSON("no json"); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateJSON(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"queries": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"tier":    map[string]any{"type": "string", "enum": []any{"mini", "pro"}},
		},
		"required": []any{"queries"},
	}
	if err := ValidateJSON(json.RawMessage(`{"queries":["a"],"tier":"mini"}`), schema); err != nil {
		t.Fatal(err)
	}
	if err := ValidateJSON(json.RawMessage(`{"tier":"mini"}`), schema); err == nil {
		t.Fatal("missing required passed")
	}
	if err := ValidateJSON(json.RawMessage(`{"queries":[1]}`), schema); err == nil {
		t.Fatal("wrong item type passed")
	}
	if err := ValidateJSON(json.RawMessage(`{"queries":[],"tier":"huge"}`), schema); err == nil {
		t.Fatal("enum violation passed")
	}
}

func TestChatJSONRepairs(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		content := "sorry, not json"
		if calls > 1 {
			content = `{"tier":"mini"}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
			"usage":   map[string]any{"total_tokens": 10},
		})
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, model: "m", maxOut: 100, thinking: "off",
		http: srv.Client(), sem: newSemForTest(), breaker: newBreakerForTest(), log: testLogger(), now: timeNow}
	c.maxOut = 100
	raw, _, err := c.ChatJSON(context.Background(), []Message{{Role: "user", Content: "hi"}}, map[string]any{
		"type": "object", "properties": map[string]any{"tier": map[string]any{"type": "string"}}, "required": []any{"tier"},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "mini") || calls < 2 {
		t.Fatalf("repair failed: %s calls=%d", raw, calls)
	}
}
