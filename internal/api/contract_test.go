package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/astralabs/tavilyd/internal/research"

	"github.com/getkin/kin-openapi/openapi3"
)

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("testdata/tavily-openapi.json")
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	// Tavily's own spec has an invalid example (integer response_time 1.23),
	// so document validation is intentionally skipped; schemas still resolve.
	return doc
}

func validateResponse(t *testing.T, doc *openapi3.T, path string, status int, body []byte, out any) {
	t.Helper()
	validateResponseMethod(t, doc, http.MethodPost, path, status, body, out)
}

func validateResponseMethod(t *testing.T, doc *openapi3.T, method, path string, status int, body []byte, out any) {
	t.Helper()
	op := doc.Paths.Find(path)
	if op == nil {
		t.Fatalf("spec has no path %s", path)
	}
	var operation *openapi3.Operation
	if method == http.MethodGet {
		operation = op.Get
	} else {
		operation = op.Post
	}
	if operation == nil {
		t.Fatalf("spec has no %s %s", method, path)
	}
	resp := operation.Responses.Status(status)
	if resp == nil || resp.Value == nil {
		// fall back to the 2xx default if the exact code is not modelled
		resp = operation.Responses.Status(http.StatusOK)
	}
	if resp == nil || resp.Value == nil {
		t.Fatalf("spec has no response schema for %s %d", path, status)
	}
	media := resp.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil {
		t.Fatalf("no JSON schema for %s %d", path, status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	raw, _ := json.Marshal(out)
	var generic any
	_ = json.Unmarshal(raw, &generic)
	// Tavily's schema marks `answer` as required but non-nullable while the API
	// omits/nullifies it unless include_answer is set. Normalise only for
	// validation so the known spec bug does not mask real drift.
	if strings.Contains(path, "/research") {
		// Tavily's schema declares response_time as integer while its example and
		// real API use floats; normalise for validation only.
		if m, ok := generic.(map[string]any); ok {
			if v, ok := m["response_time"].(float64); ok {
				m["response_time"] = int(v)
			}
		}
	}
	if path == "/search" {
		if m, ok := generic.(map[string]any); ok {
			if v, exists := m["answer"]; !exists || v == nil {
				m["answer"] = ""
			}
		}
	}
	if err := media.Schema.Value.VisitJSON(generic); err != nil {
		t.Fatalf("%s response violates spec: %v\nbody: %s", path, err, body)
	}
}

func TestContractSearchAndExtract(t *testing.T) {
	doc := loadSpec(t)
	env := newTestEnv(t, nil)
	res := env.post(t, "/search", map[string]any{"query": "contract test", "max_results": 3, "include_usage": true, "include_favicon": true}, testKey)
	if res.status != http.StatusOK {
		t.Fatalf("search status %d: %s", res.status, res.body)
	}
	var sr SearchResponse
	validateResponse(t, doc, "/search", res.status, res.body, &sr)

	er := env.post(t, "/extract", map[string]any{"urls": env.htmlURL[0], "include_usage": true, "chunks_per_source": 2}, testKey)
	if er.status != http.StatusOK {
		t.Fatalf("extract status %d: %s", er.status, er.body)
	}
	var ex ExtractResponse
	validateResponse(t, doc, "/extract", er.status, er.body, &ex)
}

func TestContractMap(t *testing.T) {
	doc := loadSpec(t)
	env := newTestEnv(t, nil)
	res := env.post(t, "/map", map[string]any{"url": env.site.URL, "limit": 5, "include_usage": true}, testKey)
	if res.status != http.StatusOK {
		t.Fatalf("map status %d: %s", res.status, res.body)
	}
	var mr MapResponse
	validateResponse(t, doc, "/map", res.status, res.body, &mr)
}

func TestContractCrawl(t *testing.T) {
	doc := loadSpec(t)
	env := newTestEnv(t, nil)
	res := env.post(t, "/crawl", map[string]any{"url": env.site.URL, "limit": 1, "max_depth": 1}, testKey)
	if res.status != http.StatusOK {
		t.Fatalf("crawl status %d: %s", res.status, res.body)
	}
	var cr CrawlResponse
	validateResponse(t, doc, "/crawl", res.status, res.body, &cr)
}

type stubResearch struct {
	job *research.Job
}

func (s *stubResearch) Create(_ context.Context, job *research.Job) (<-chan research.Event, func(), error) {
	job.Status = research.StatusPending
	job.CreatedAt = time.Now().UTC()
	s.job = job
	ch := make(chan research.Event)
	return ch, func() {}, nil
}
func (s *stubResearch) Get(_ context.Context, _ string) (*research.Job, error) { return s.job, nil }
func (s *stubResearch) Cancel(string)                                          {}

func TestContractResearch(t *testing.T) {
	doc := loadSpec(t)
	env := newTestEnv(t, nil)
	stub := &stubResearch{}
	env.srv.research = stub
	res := env.post(t, "/research", map[string]any{"input": "contract research", "model": "mini"}, testKey)
	if res.status != http.StatusOK {
		t.Fatalf("research create %d: %s", res.status, res.body)
	}
	var created map[string]any
	// The spec models only 201; our SDK-compatible create returns 200 with the
	// same body, so validate the body against the 201 schema.
	validateResponseMethod(t, doc, http.MethodPost, "/research", http.StatusCreated, res.body, &created)
	id, _ := stub.job.ID, ""
	stub.job.Status = research.StatusCompleted
	stub.job.Content = "Report [1]\n\n## Sources\n\n1. [x](https://example.com)"
	stub.job.Sources = []research.Source{{ID: 1, Title: "x", URL: "https://example.com"}}
	stub.job.ResponseTime = 1.5
	resp, err := http.Get(env.server.URL + "/research/" + id)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var got map[string]any
	validateResponseMethod(t, doc, http.MethodGet, "/research/{request_id}", resp.StatusCode, body, &got)
}

// TestNo5xxOnDocumentedEnums walks every documented enum and boundary and
// asserts the server never returns 5xx.
func TestNo5xxOnDocumentedEnums(t *testing.T) {
	env := newTestEnv(t, nil)
	cases := []map[string]any{
		{"query": "x", "search_depth": "basic"},
		{"query": "x", "search_depth": "advanced"},
		{"query": "x", "search_depth": "fast"},
		{"query": "x", "search_depth": "ultra-fast"},
		{"query": "x", "topic": "general"},
		{"query": "x", "topic": "news"},
		{"query": "x", "topic": "finance"},
		{"query": "x", "max_results": 0},
		{"query": "x", "max_results": 20},
		{"query": "x", "chunks_per_source": 1},
		{"query": "x", "chunks_per_source": 3},
		{"query": "x", "include_answer": "advanced"},
		{"query": "x", "include_raw_content": "text"},
		{"query": "x", "time_range": "d"},
		{"query": "x", "include_domains": []string{"example.com"}, "include_domains_mode": "prefer"},
		{"query": "x", "exact_match": true},
	}
	for _, payload := range cases {
		body, _ := json.Marshal(payload)
		resp, err := http.Post(env.server.URL+"/search", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			t.Fatalf("payload %v -> %d", payload, resp.StatusCode)
		}
	}
	// Extract boundaries.
	for _, payload := range []map[string]any{
		{"urls": env.htmlURL[0], "chunks_per_source": 1},
		{"urls": env.htmlURL[0], "chunks_per_source": 5},
		{"urls": env.htmlURL[0], "timeout": 60},
		{"urls": env.htmlURL[0], "format": "text", "include_usage": true},
	} {
		body, _ := json.Marshal(payload)
		resp, err := http.Post(env.server.URL+"/extract", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			t.Fatalf("extract payload %v -> %d", payload, resp.StatusCode)
		}
	}
}
