package research

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/llm"
)

func TestVerifyCitations(t *testing.T) {
	sources := []Source{{ID: 1, URL: "https://a"}, {ID: 2, URL: "https://b"}}
	out, cited := VerifyCitations("A [S1] B [S9] C [S2] D [S1]", sources)
	if strings.Contains(out, "S9") {
		t.Fatalf("invalid marker survived: %s", out)
	}
	if len(cited) != 2 || cited[0].ID != 1 || cited[1].ID != 2 {
		t.Fatalf("cited = %+v", cited)
	}
}

func TestRenderCitationsFormats(t *testing.T) {
	sources := []Source{{ID: 3, Title: "Kubernetes Probes", URL: "https://example.com/probes"}}
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	for _, format := range []string{"numbered", "mla", "apa", "chicago"} {
		out := RenderCitations("Body [S3].", sources, format, now)
		if !strings.Contains(out, "https://example.com/probes") || !strings.Contains(out, "Body [1].") {
			t.Fatalf("%s: unexpected output:\n%s", format, out)
		}
		if !strings.Contains(out, "## Sources") {
			t.Fatalf("%s: missing sources section", format)
		}
	}
}

type fakeTools struct {
	slow bool
}

func (f *fakeTools) Search(_ context.Context, q string, max int) ([]Source, error) {
	return []Source{
		{Title: "Source A", URL: "https://a.example/one", Snippet: q, Score: 0.9},
		{Title: "Source B", URL: "https://b.example/two", Snippet: q, Score: 0.7},
	}, nil
}

func (f *fakeTools) Read(ctx context.Context, url, _ string) (string, error) {
	if f.slow {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "Readiness probes decide when a pod receives traffic. " + url, nil
}

func fakeLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			return
		}
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		last := ""
		if len(req.Messages) > 0 {
			last = req.Messages[len(req.Messages)-1].Content
		}
		content := `{"tier":"mini"}`
		switch {
		case strings.Contains(last, "planning web research"):
			content = `{"aspects":["readiness"],"queries":["kubernetes readiness probe","readiness probe criticism"],"include_domains":[]}`
		case strings.Contains(last, "Extract evidence notes"):
			content = "- Readiness probes gate traffic until the app is ready.\n- They differ from liveness probes."
		case strings.Contains(last, "final research report"):
			if strings.Contains(last, "JSON") {
				content = `{"summary":"Readiness probes gate traffic [S1].","claims":["They differ from liveness probes [S2]."]}`
			} else {
				content = "# Kubernetes readiness probes\n\nReadiness probes gate traffic until the application is ready [S1]. They differ from liveness probes [S2].\n\n## Limitations\n\nProbe tuning is workload-specific."
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
			"usage":   map[string]any{"total_tokens": 42},
		})
	}))
}

func testClient(t *testing.T, url string) *llm.Client {
	t.Helper()
	cfg := &config.Config{LLMBaseURL: url, LLMModel: "m", LLMContextTokens: 4096, LLMMaxOutputTokens: 512,
		LLMTimeout: 5 * time.Second, LLMMaxConcurrency: 2, LLMThinking: "off"}
	c, err := llm.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEngineHappyPath(t *testing.T) {
	srv := fakeLLM(t)
	defer srv.Close()
	e := NewEngine(testClient(t, srv.URL), &fakeTools{}, MiniTier(), nil)
	job := &Job{ID: "j1", Input: "kubernetes readiness probe", Model: "mini", CitationFormat: "numbered"}
	if err := e.Run(context.Background(), job, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(job.Content, "[1]") || !strings.Contains(job.Content, "## Sources") {
		t.Fatalf("unexpected report:\n%s", job.Content)
	}
	if len(job.Sources) == 0 || job.Sources[0].URL == "" {
		t.Fatalf("sources missing: %+v", job.Sources)
	}
}

func TestEngineSchemaMode(t *testing.T) {
	srv := fakeLLM(t)
	defer srv.Close()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{"type": "string"},
			"claims":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []any{"summary"},
	}
	e := NewEngine(testClient(t, srv.URL), &fakeTools{}, MiniTier(), nil)
	job := &Job{ID: "j2", Input: "kubernetes readiness probe", Model: "mini", CitationFormat: "numbered", Schema: schema}
	if err := e.Run(context.Background(), job, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(job.ContentJSON, &v); err != nil || v["summary"] == "" {
		t.Fatalf("bad JSON content: %s", job.ContentJSON)
	}
}
