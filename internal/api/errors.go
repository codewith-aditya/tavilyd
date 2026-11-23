package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/astralabs/tavilyd/internal/observability"
)

// APIError is the Tavily-style error envelope.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Message) }

// Errorf builds an APIError.
func Errorf(status int, format string, args ...any) *APIError {
	return &APIError{Status: status, Message: fmt.Sprintf(format, args...)}
}

var (
	// ErrUnauthorized is returned when the API key is missing or wrong.
	ErrUnauthorized = &APIError{Status: http.StatusUnauthorized, Message: "Unauthorized: missing or invalid API key"}
	// ErrRateLimited is returned by the limiter.
	ErrRateLimited = &APIError{Status: http.StatusTooManyRequests, Message: "Rate limit exceeded"}
	// ErrBadGateway signals an upstream dependency failed.
	ErrBadGateway = &APIError{Status: http.StatusBadGateway, Message: "Upstream search service unavailable"}
	// ErrGatewayTimeout signals the request deadline elapsed with no results.
	ErrGatewayTimeout = &APIError{Status: http.StatusGatewayTimeout, Message: "Request timed out"}
)

type errorBody struct {
	Detail struct {
		Error string `json:"error"`
	} `json:"detail"`
}

// writeJSON serializes v with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError renders {"detail":{"error":"..."}} with the right status.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	msg := "Internal server error"
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		status = apiErr.Status
		msg = apiErr.Message
	}
	var body errorBody
	body.Detail.Error = msg
	if status == http.StatusTooManyRequests {
		if ra := w.Header().Get("Retry-After"); ra == "" {
			w.Header().Set("Retry-After", "1")
		}
	}
	if status >= 500 {
		observability.FromContext(r.Context()).Error("request failed", "status", status, "error", err)
	}
	writeJSON(w, status, body)
}

// decodeJSON reads a size-limited JSON body with unknown fields ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return Errorf(http.StatusUnprocessableEntity, "Invalid JSON body: %v", err)
	}
	return nil
}

// setRetryAfter writes a Retry-After header in whole seconds.
func setRetryAfter(w http.ResponseWriter, d int64) {
	if d < 1 {
		d = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(d, 10))
}
