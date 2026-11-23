// Package llm is a small OpenAI-compatible chat client for the self-hosted
// Qwen model, with concurrency limiting, retries, a circuit breaker, thinking
// control and strict JSON validation.
package llm

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker"
	"golang.org/x/sync/semaphore"

	"github.com/astralabs/tavilyd/internal/config"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Options controls one completion.
type Options struct {
	Thinking    *bool // nil = server default
	Temperature float64
	MaxTokens   int
	JSONSchema  map[string]any
}

// Client talks to the OpenAI-compatible endpoint.
type Client struct {
	base     string
	apiKey   string
	model    string
	maxOut   int
	thinking string
	http     *http.Client
	sem      *semaphore.Weighted
	breaker  *gobreaker.CircuitBreaker
	log      *slog.Logger
	now      func() time.Time

	healthMu  sync.Mutex
	healthAt  time.Time
	healthErr error
	healthy   atomic.Bool
}

// New builds the client from config.
func New(cfg *config.Config, log *slog.Logger) (*Client, error) {
	if cfg.LLMBaseURL == "" {
		return nil, errors.New("LLM_BASE_URL is empty")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.LLMCAFile != "" {
		pem, err := os.ReadFile(cfg.LLMCAFile)
		if err != nil {
			return nil, fmt.Errorf("LLM_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("LLM_CA_FILE contains no certificates")
		}
		tlsCfg.RootCAs = pool
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: cfg.LLMConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        cfg.LLMMaxConcurrency * 4,
		MaxIdleConnsPerHost: cfg.LLMMaxConcurrency * 2,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     tlsCfg,
		ForceAttemptHTTP2:   true,
	}
	c := &Client{
		base:     strings.TrimRight(cfg.LLMBaseURL, "/"),
		apiKey:   cfg.LLMAPIKey,
		model:    cfg.LLMModel,
		maxOut:   cfg.LLMMaxOutputTokens,
		thinking: strings.ToLower(cfg.LLMThinking),
		http:     &http.Client{Transport: tr, Timeout: cfg.LLMTimeout},
		sem:      semaphore.NewWeighted(int64(cfg.LLMMaxConcurrency)),
		log:      log,
		now:      time.Now,
		breaker: gobreaker.NewCircuitBreaker(gobreaker.Settings{
			Name:        "llm",
			MaxRequests: 2,
			Interval:    30 * time.Second,
			Timeout:     20 * time.Second,
			ReadyToTrip: func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= 3 },
		}),
	}
	return c, nil
}

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []Message      `json:"messages"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	Temperature    float64        `json:"temperature,omitempty"`
	Stream         bool           `json:"stream"`
	ChatTemplateKw map[string]any `json:"chat_template_kwargs,omitempty"`
	ResponseFormat map[string]any `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat returns the visible content with  thinking blocks stripped.
func (c *Client) Chat(ctx context.Context, messages []Message, opts Options) (string, int, error) {
	req := chatRequest{
		Model:       c.model,
		Messages:    messages,
		MaxTokens:   opts.MaxTokens,
		Temperature: opts.Temperature,
		Stream:      false,
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = c.maxOut
	}
	enable := false
	switch c.thinking {
	case "on":
		enable = true
	case "off":
		enable = false
	default:
		if opts.Thinking != nil {
			enable = *opts.Thinking
		} else {
			enable = true
		}
	}
	if opts.Thinking != nil {
		enable = *opts.Thinking
	}
	req.ChatTemplateKw = map[string]any{"enable_thinking": enable}
	if opts.JSONSchema != nil {
		req.ResponseFormat = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "response",
				"schema": opts.JSONSchema,
				"strict": true,
			},
		}
	}

	if err := c.sem.Acquire(ctx, 1); err != nil {
		return "", 0, err
	}
	defer c.sem.Release(1)

	body, err := json.Marshal(req)
	if err != nil {
		return "", 0, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(150+rand.Intn(250)) * time.Millisecond * time.Duration(attempt)):
			case <-ctx.Done():
				return "", 0, ctx.Err()
			}
		}
		content, tokens, err := c.once(ctx, body)
		if err == nil {
			c.healthy.Store(true)
			return content, tokens, nil
		}
		lastErr = err
		if !retryable(err) {
			break
		}
	}
	c.healthy.Store(false)
	return "", 0, lastErr
}

func (c *Client) once(ctx context.Context, body []byte) (string, int, error) {
	raw, err := c.breaker.Execute(func() (any, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &statusError{code: resp.StatusCode, body: truncate(string(data), 300)}
		}
		return data, nil
	})
	if err != nil {
		return "", 0, err
	}
	var parsed chatResponse
	if err := json.Unmarshal(raw.([]byte), &parsed); err != nil {
		return "", 0, fmt.Errorf("decode chat response: %w", err)
	}
	if parsed.Error != nil {
		return "", 0, errors.New(parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", 0, errors.New("empty choices")
	}
	return StripThink(parsed.Choices[0].Message.Content), parsed.Usage.TotalTokens, nil
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string { return fmt.Sprintf("llm status %d: %s", e.code, e.body) }

func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

var (
	reThinkClosed = regexp.MustCompile(`(?s) thinking.*?</think>`)
	reThinkOpen   = regexp.MustCompile(`(?s) thinking.*$`)
)

// StripThink removes reasoning blocks from visible output.
func StripThink(s string) string {
	s = reThinkClosed.ReplaceAllString(s, "")
	s = reThinkOpen.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// ChatJSON asks for JSON, extracts the first valid object/array, validates it
// against schema when provided, and retries once with the validation error.
func (c *Client) ChatJSON(ctx context.Context, messages []Message, schema map[string]any, opts Options) (json.RawMessage, int, error) {
	opts.JSONSchema = schema
	instruction := Message{Role: "system", Content: "Reply with a single valid JSON value only. No prose, no markdown fences."}
	msgs := append([]Message{instruction}, messages...)
	content, tokens, err := c.Chat(ctx, msgs, opts)
	if err != nil {
		return nil, tokens, err
	}
	raw, jerr := ExtractJSON(content)
	if jerr == nil && schema != nil {
		jerr = ValidateJSON(raw, schema)
	}
	if jerr == nil {
		return raw, tokens, nil
	}
	retryMsgs := append(msgs, Message{Role: "user", Content: "Your previous answer was invalid: " + jerr.Error() + ". Return only corrected JSON."})
	content2, tokens2, err2 := c.Chat(ctx, retryMsgs, opts)
	if err2 != nil {
		return nil, tokens + tokens2, err2
	}
	raw2, jerr2 := ExtractJSON(content2)
	if jerr2 != nil {
		return nil, tokens + tokens2, jerr2
	}
	if schema != nil {
		if verr := ValidateJSON(raw2, schema); verr != nil {
			return nil, tokens + tokens2, verr
		}
	}
	return raw2, tokens + tokens2, nil
}

// ExtractJSON returns the first balanced JSON object/array in s.
func ExtractJSON(s string) (json.RawMessage, error) {
	start := -1
	for i, r := range s {
		if r == '{' || r == '[' {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, errors.New("no JSON found")
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				raw := json.RawMessage(s[start : i+1])
				if !json.Valid(raw) {
					return nil, errors.New("invalid JSON")
				}
				return raw, nil
			}
		}
	}
	return nil, errors.New("unterminated JSON")
}

// ValidateJSON checks a practical subset of JSON Schema: type, properties,
// required, items and enum.
func ValidateJSON(raw json.RawMessage, schema map[string]any) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return validateValue(v, schema, "$")
}

func validateValue(v any, schema map[string]any, path string) error {
	if schema == nil {
		return nil
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: value not in enum", path)
		}
	}
	if typ, ok := schema["type"].(string); ok {
		if err := checkType(v, typ, path); err != nil {
			return err
		}
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object", path)
		}
		for _, req := range toStrings(schema["required"]) {
			if _, exists := obj[req]; !exists {
				return fmt.Errorf("%s: missing required property %q", path, req)
			}
		}
		for name, ps := range props {
			if child, exists := obj[name]; exists {
				if sub, ok := ps.(map[string]any); ok {
					if err := validateValue(child, sub, path+"."+name); err != nil {
						return err
					}
				}
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: expected array", path)
		}
		for i, item := range arr {
			if err := validateValue(item, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkType(v any, typ, path string) error {
	ok := false
	switch typ {
	case "object":
		_, ok = v.(map[string]any)
	case "array":
		_, ok = v.([]any)
	case "string":
		_, ok = v.(string)
	case "number":
		_, ok = v.(float64)
	case "integer":
		if f, isNum := v.(float64); isNum && f == float64(int64(f)) {
			ok = true
		}
	case "boolean":
		_, ok = v.(bool)
	case "null":
		ok = v == nil
	default:
		ok = true
	}
	if !ok {
		return fmt.Errorf("%s: expected %s", path, typ)
	}
	return nil
}

func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Probe performs one GET /models attempt plus one jittered retry on transient
// failures. Callers control the overall deadline (LLM_HEALTH_TIMEOUT).
func (c *Client) Probe(ctx context.Context) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(80+rand.Intn(120)) * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/models", nil)
		if err != nil {
			return err
		}
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if !retryable(err) {
				return err
			}
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("models status %d", resp.StatusCode)
			if resp.StatusCode >= 500 {
				continue
			}
			return lastErr
		}
		return nil
	}
	return lastErr
}

// Healthy probes GET /models with a 30 s cache.
func (c *Client) Healthy(ctx context.Context) (string, error) {
	c.healthMu.Lock()
	if c.now().Sub(c.healthAt) < 30*time.Second {
		err := c.healthErr
		c.healthMu.Unlock()
		if err != nil {
			return "degraded", err
		}
		return "ok", nil
	}
	c.healthMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, c.base+"/models", nil)
	if err == nil {
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		var resp *http.Response
		resp, err = c.http.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("models status %d", resp.StatusCode)
			}
		}
	}
	c.healthMu.Lock()
	c.healthAt = c.now()
	c.healthErr = err
	c.healthMu.Unlock()
	if err != nil {
		return "degraded", err
	}
	return "ok", nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
