package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/astralabs/tavilyd/internal/research"
	"github.com/astralabs/tavilyd/internal/usage"
)

// ResearchRequest is the POST /research body (Tavily-compatible).
type ResearchRequest struct {
	Input          string         `json:"input"`
	Model          string         `json:"model"`
	Stream         bool           `json:"stream"`
	OutputSchema   map[string]any `json:"output_schema"`
	OutputLength   string         `json:"output_length"`
	CitationFormat string         `json:"citation_format"`
	Files          []string       `json:"files"`
	IncludeDomains []string       `json:"include_domains"`
	ExcludeDomains []string       `json:"exclude_domains"`
	APIKey         string         `json:"api_key"`
}

func (r *ResearchRequest) validate() error {
	r.Input = strings.TrimSpace(r.Input)
	if r.Input == "" {
		return Errorf(http.StatusUnprocessableEntity, "input is required")
	}
	if len([]rune(r.Input)) > 2000 {
		return Errorf(http.StatusUnprocessableEntity, "input must be at most 2000 characters")
	}
	if r.Model == "" {
		r.Model = "auto"
	}
	switch r.Model {
	case "mini", "pro", "auto":
	default:
		return Errorf(http.StatusUnprocessableEntity, "model must be one of: mini, pro, auto")
	}
	if r.CitationFormat == "" {
		r.CitationFormat = "numbered"
	}
	switch r.CitationFormat {
	case "numbered", "mla", "apa", "chicago":
	default:
		return Errorf(http.StatusUnprocessableEntity, "citation_format must be one of: numbered, mla, apa, chicago")
	}
	switch strings.ToLower(strings.TrimSpace(r.OutputLength)) {
	case "", "standard", "short", "long":
	default:
		return Errorf(http.StatusUnprocessableEntity, "output_length must be one of: short, standard, long")
	}
	if len(r.Files) > 5 {
		return Errorf(http.StatusUnprocessableEntity, "files must contain at most 5 entries")
	}
	if len(r.IncludeDomains) > 20 || len(r.ExcludeDomains) > 20 {
		return Errorf(http.StatusUnprocessableEntity, "domains must contain at most 20 entries")
	}
	if r.OutputSchema != nil {
		if _, ok := r.OutputSchema["properties"]; !ok {
			return Errorf(http.StatusUnprocessableEntity, "output_schema must contain properties")
		}
	}
	return nil
}

// HandleResearchCreate implements POST /research.
func (s *Server) HandleResearchCreate(w http.ResponseWriter, r *http.Request) {
	if s.research == nil {
		writeError(w, r, Errorf(http.StatusServiceUnavailable, "research is not configured"))
		return
	}
	var req ResearchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.authenticate(r, req.APIKey); err != nil {
		writeError(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, r, err)
		return
	}
	quotaCost := int64(4)
	if req.Model == "pro" {
		quotaCost = 15
	}
	if !s.enforceQuota(r.Context(), w, r, quotaCost) {
		return
	}
	var files []research.File
	words := 0
	for i, raw := range req.Files {
		data, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
		if derr != nil {
			writeError(w, r, Errorf(http.StatusUnprocessableEntity, "files[%d] is not valid base64", i))
			return
		}
		text := string(data)
		words += len(strings.Fields(text))
		if words > 80000 {
			writeError(w, r, Errorf(http.StatusUnprocessableEntity, "files exceed 80000 words combined"))
			return
		}
		files = append(files, research.File{Name: fmt.Sprintf("file-%d.txt", i+1), Content: text})
	}
	job := &research.Job{
		ID:             uuid.NewString(),
		Input:          req.Input,
		Model:          req.Model,
		CitationFormat: req.CitationFormat,
		Stream:         req.Stream,
		Schema:         req.OutputSchema,
		OutputLength:   strings.ToLower(strings.TrimSpace(req.OutputLength)),
		Files:          files,
		IncludeDomains: req.IncludeDomains,
		ExcludeDomains: req.ExcludeDomains,
		Key:            APIKeyFromContext(r.Context()),
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestDeadline)
	defer cancel()
	_ = ctx
	ch, unsub, err := s.research.Create(r.Context(), job)
	if err != nil {
		status := http.StatusServiceUnavailable
		if err == research.ErrBusy {
			status = http.StatusTooManyRequests
		}
		writeError(w, r, Errorf(status, "%s", research.JobErrorText(err)))
		return
	}
	credits := 4
	if job.Model == "pro" {
		credits = 15
	}
	s.recordUsage(r.Context(), usage.Delta{Credits: int64(credits), TokensIn: approxTokens(job.Input)})
	if !req.Stream {
		unsub()
		writeJSON(w, s.cfg.ResearchCreateStatus, map[string]any{
			"request_id": job.ID, "created_at": job.CreatedAt, "status": job.Status,
			"input": job.Input, "model": job.Model, "response_time": 0,
		})
		return
	}
	defer unsub()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, Errorf(http.StatusInternalServerError, "streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			s.research.Cancel(job.ID)
			return
		case <-heartbeat.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, _ := json.Marshal(ev)
			if _, err := w.Write([]byte("event: " + ev.Type + "\ndata: " + string(payload) + "\n\n")); err != nil {
				s.research.Cancel(job.ID)
				return
			}
			flusher.Flush()
			if ev.Type == "done" || ev.Type == "error" {
				return
			}
		}
	}
}

// HandleResearchGet implements GET /research/{request_id}.
func (s *Server) HandleResearchGet(w http.ResponseWriter, r *http.Request) {
	if s.research == nil {
		writeError(w, r, Errorf(http.StatusServiceUnavailable, "research is not configured"))
		return
	}
	id := strings.TrimSpace(r.PathValue("request_id"))
	if id == "" {
		writeError(w, r, Errorf(http.StatusNotFound, "research job not found"))
		return
	}
	job, err := s.research.Get(r.Context(), id)
	if err != nil {
		if err == research.ErrNotFound {
			writeError(w, r, Errorf(http.StatusNotFound, "Research task not found"))
			return
		}
		writeError(w, r, Errorf(http.StatusServiceUnavailable, "research storage unavailable"))
		return
	}
	resp := map[string]any{
		"request_id": job.ID, "created_at": job.CreatedAt, "status": job.Status, "model": job.Model,
	}
	status := http.StatusOK
	if job.Status == research.StatusPending || job.Status == research.StatusInProgress {
		status = http.StatusAccepted
	}
	switch job.Status {
	case research.StatusCompleted:
		if len(job.ContentJSON) > 0 {
			resp["content"] = json.RawMessage(job.ContentJSON)
		} else {
			resp["content"] = job.Content
		}
		resp["sources"] = job.Sources
		resp["response_time"] = job.ResponseTime
		if r.URL.Query().Get("include_usage") == "true" {
			credits := 4
			if job.Model == "pro" {
				credits = 15
			}
			resp["usage"] = map[string]any{"credits": credits}
		}
	case research.StatusFailed:
		resp["error"] = job.Error
	}
	writeJSON(w, status, resp)
}
