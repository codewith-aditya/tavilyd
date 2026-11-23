package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// FeedbackRequest is POST /feedback.
type FeedbackRequest struct {
	RequestID         string         `json:"request_id"`
	SessionID         string         `json:"session_id"`
	AgentScore        *float64       `json:"agent_score"`
	HumanScore        *float64       `json:"human_score"`
	ExtraScores       map[string]any `json:"extra_scores"`
	Comment           string         `json:"comment"`
	ResponseDelivered *bool          `json:"response_delivered"`
	UsedIDs           []string       `json:"used_ids"`
	UsedURLs          []string       `json:"used_urls"`
	UsedCitations     []string       `json:"used_citations"`
	URLsScores        map[string]any `json:"urls_scores"`
	APIKey            string         `json:"api_key"`
}

// HandleFeedback implements POST /feedback (stored for later ranking work).
func (s *Server) HandleFeedback(w http.ResponseWriter, r *http.Request) {
	var req FeedbackRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.authenticate(r, req.APIKey); err != nil {
		writeError(w, r, err)
		return
	}
	if len(req.Comment) > 4000 {
		writeError(w, r, Errorf(http.StatusUnprocessableEntity, "comment must be at most 4000 characters"))
		return
	}
	if len(req.UsedURLs) > 100 || len(req.UsedIDs) > 100 || len(req.UsedCitations) > 100 {
		writeError(w, r, Errorf(http.StatusUnprocessableEntity, "at most 100 used ids/urls/citations"))
		return
	}
	if s.usage == nil {
		writeError(w, r, Errorf(http.StatusServiceUnavailable, "feedback storage unavailable"))
		return
	}
	id := uuid.NewString()
	payload, _ := json.Marshal(req)
	if err := s.usage.SaveFeedback(r.Context(), id, payload, 30*24*time.Hour); err != nil {
		writeError(w, r, Errorf(http.StatusServiceUnavailable, "feedback storage unavailable"))
		return
	}
	if s.metrics != nil {
		s.metrics.FeedbackTotal.Inc()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "feedback_id": id})
}

// HandleUsage implements GET /usage (current key's emulated credits).
func (s *Server) HandleUsage(w http.ResponseWriter, r *http.Request) {
	if err := s.authenticate(r, ""); err != nil {
		writeError(w, r, err)
		return
	}
	resp := map[string]any{
		"period": "all-time", "credits": 0, "requests": 0, "searches": 0,
		"extracts": 0, "maps": 0, "crawls": 0, "research": 0,
		"tokens_in": 0, "tokens_out": 0,
		"rate_limit": map[string]any{
			"rps": s.cfg.RateLimitRPS, "burst": s.cfg.RateLimitBurst,
			"rpm": s.cfg.RateLimitRPS * 60,
		},
	}
	if s.usage != nil {
		snap, err := s.usage.Snapshot(r.Context(), APIKeyFromContext(r.Context()))
		if err != nil {
			writeError(w, r, Errorf(http.StatusServiceUnavailable, "usage storage unavailable"))
			return
		}
		resp["credits"], resp["requests"], resp["searches"] = snap.Credits, snap.Requests, snap.Searches
		resp["extracts"], resp["maps"], resp["crawls"] = snap.Extracts, snap.Maps, snap.Crawls
		resp["research"], resp["tokens_in"], resp["tokens_out"] = snap.Research, snap.TokensIn, snap.TokensOut
		limit := int64(s.cfg.CreditLimit)
		resp["credit_limit"] = limit
		if limit > 0 {
			remaining := limit - snap.Credits
			if remaining < 0 {
				remaining = 0
			}
			resp["credits_remaining"] = remaining
		} else {
			resp["credits_remaining"] = nil
		}
		if snap.LastUsed > 0 {
			resp["last_used"] = time.Unix(snap.LastUsed, 0).UTC().Format(time.RFC3339)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
