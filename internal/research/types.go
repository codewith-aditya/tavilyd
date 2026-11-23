// Package research implements the Tavily-compatible deep-research endpoint:
// plan, search, read, reflect and write a cited report with a self-hosted LLM.
package research

import (
	"encoding/json"
	"errors"
	"time"
)

// Status values for a research job.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// ErrNotFound is returned for unknown job ids.
var ErrNotFound = errors.New("research job not found")

// ErrBusy means the queue or the per-key concurrency limit is exhausted.
var ErrBusy = errors.New("research queue is full")

// ErrUnavailable means job state cannot be persisted (Redis down).
var ErrUnavailable = errors.New("research store unavailable")

// Source is one fetched page in the source table.
type Source struct {
	ID      int     `json:"id"`
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Snippet string  `json:"-"`
	Notes   string  `json:"-"`
	Score   float64 `json:"score,omitempty"`
	Favicon *string `json:"favicon,omitempty"`
	Read    bool    `json:"-"`
}

// Job is the stored research request/result.
type Job struct {
	ID             string          `json:"request_id"`
	Input          string          `json:"input"`
	Model          string          `json:"model"`
	CitationFormat string          `json:"citation_format"`
	Stream         bool            `json:"-"`
	Schema         map[string]any  `json:"-"`
	OutputLength   string          `json:"-"`
	Files          []File          `json:"-"`
	IncludeDomains []string        `json:"-"`
	ExcludeDomains []string        `json:"-"`
	CreatedAt      time.Time       `json:"created_at"`
	Status         string          `json:"status"`
	Content        string          `json:"content,omitempty"`
	ContentJSON    json.RawMessage `json:"content_json,omitempty"`
	Sources        []Source        `json:"sources,omitempty"`
	Error          string          `json:"error,omitempty"`
	ResponseTime   float64         `json:"response_time"`
	Key            string          `json:"-"`
	Round          int             `json:"round,omitempty"`
	LLMCalls       int             `json:"llm_calls,omitempty"`
	PagesRead      int             `json:"pages_read,omitempty"`
	Tokens         int             `json:"tokens,omitempty"`
}

// File is an uploaded text source.
type File struct {
	Name    string
	Content string
}

// Event is one SSE event.
type Event struct {
	Type         string   `json:"type"` // progress | source | delta | done | error
	Stage        string   `json:"stage,omitempty"`
	Detail       string   `json:"detail,omitempty"`
	Source       *Source  `json:"source,omitempty"`
	Delta        string   `json:"content,omitempty"`
	Sources      []Source `json:"sources,omitempty"`
	ResponseTime float64  `json:"response_time,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// Tier budgets.
type Tier struct {
	Name        string
	Rounds      int
	Queries     int
	Pages       int
	Deadline    time.Duration
	MaxLLM      int
	MaxTokens   int
	ReportMin   int
	ReportMax   int
	SearchDepth string
}
