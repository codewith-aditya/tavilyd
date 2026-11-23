// Command mcp exposes tavilyd as a Model Context Protocol server.
//
//	stdin/stdout JSON-RPC (stdio mode):  MCP_API_URL=http://127.0.0.1:8080 MCP_API_KEY=... mcp
//	streamable HTTP:                     mcp -http :8090
//
// Tools: tavily-search, tavily-extract, tavily-map, tavily-crawl,
// tavily-research. Requests are forwarded to the local HTTP API, so auth,
// rate limits, proxying and caching all apply unchanged.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var tools = []toolDef{
	{Name: "tavily-search", Description: "Search the web (Tavily-compatible /search).",
		InputSchema: schema(map[string]any{
			"query":           map[string]any{"type": "string"},
			"search_depth":    map[string]any{"type": "string", "enum": []string{"basic", "advanced", "fast", "ultra-fast"}},
			"max_results":     map[string]any{"type": "integer"},
			"include_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"exclude_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"include_answer":  map[string]any{"type": "boolean"},
			"topic":           map[string]any{"type": "string"},
			"time_range":      map[string]any{"type": "string"},
		}, []string{"query"})},
	{Name: "tavily-extract", Description: "Extract cleaned Markdown from URLs.",
		InputSchema: schema(map[string]any{
			"urls":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"query":             map[string]any{"type": "string"},
			"chunks_per_source": map[string]any{"type": "integer"},
			"extract_depth":     map[string]any{"type": "string", "enum": []string{"basic", "advanced"}},
			"format":            map[string]any{"type": "string", "enum": []string{"markdown", "text"}},
			"include_images":    map[string]any{"type": "boolean"},
			"include_favicon":   map[string]any{"type": "boolean"},
		}, []string{"urls"})},
	{Name: "tavily-map", Description: "Map a site's URLs (bounded crawl).",
		InputSchema: schema(map[string]any{
			"url":          map[string]any{"type": "string"},
			"max_depth":    map[string]any{"type": "integer"},
			"max_breadth":  map[string]any{"type": "integer"},
			"limit":        map[string]any{"type": "integer"},
			"instructions": map[string]any{"type": "string"},
		}, []string{"url"})},
	{Name: "tavily-crawl", Description: "Crawl a site and extract cleaned content.",
		InputSchema: schema(map[string]any{
			"url":           map[string]any{"type": "string"},
			"limit":         map[string]any{"type": "integer"},
			"max_depth":     map[string]any{"type": "integer"},
			"instructions":  map[string]any{"type": "string"},
			"extract_depth": map[string]any{"type": "string", "enum": []string{"basic", "advanced"}},
			"format":        map[string]any{"type": "string", "enum": []string{"markdown", "text"}},
		}, []string{"url"})},
	{Name: "tavily-research", Description: "Start a deep-research job and return its request_id.",
		InputSchema: schema(map[string]any{
			"input":           map[string]any{"type": "string"},
			"model":           map[string]any{"type": "string", "enum": []string{"mini", "pro", "auto"}},
			"output_length":   map[string]any{"type": "string", "enum": []string{"short", "standard", "long"}},
			"citation_format": map[string]any{"type": "string"},
		}, []string{"input"})},
}

func schema(props map[string]any, required []string) map[string]any {
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return map[string]any{"type": "object", "properties": props, "required": req}
}

func main() {
	httpAddr := flag.String("http", "", "serve streamable HTTP on this address instead of stdio")
	flag.Parse()
	if *httpAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/mcp", handleHTTP)
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
		log.Printf("mcp http listening on %s", *httpAddr)
		log.Fatal(http.ListenAndServe(*httpAddr, mux))
		return
	}
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return
			}
			log.Printf("decode: %v", err)
			return
		}
		if req.ID == nil && strings.HasPrefix(req.Method, "notifications/") {
			continue
		}
		resp := dispatch(req)
		if err := enc.Encode(resp); err != nil {
			log.Printf("encode: %v", err)
			return
		}
	}
}

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dispatch(req))
}

func dispatch(req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "tavilyd", "version": "1.0.0"},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		result, err := callTool(req.Params)
		if err != nil {
			resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func callTool(raw json.RawMessage) (map[string]any, error) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	endpoint := map[string]string{
		"tavily-search": "/search", "tavily-extract": "/extract",
		"tavily-map": "/map", "tavily-crawl": "/crawl", "tavily-research": "/research",
	}[params.Name]
	if endpoint == "" {
		return nil, fmt.Errorf("unknown tool %q", params.Name)
	}
	body, err := json.Marshal(params.Arguments)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(env("MCP_API_URL", "http://127.0.0.1:8080"), "/")
	req, err := http.NewRequest(http.MethodPost, base+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := env("MCP_API_KEY", ""); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("api status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(data)}},
		"isError": false,
	}, nil
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
