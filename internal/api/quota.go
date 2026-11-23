package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/astralabs/tavilyd/internal/usage"
)

// enforceQuota checks the key's all-time credits against CREDIT_LIMIT before a
// request does any expensive work. It fails open if Redis is unavailable: a
// cache outage must not take the API down. On rejection it writes a 402 with
// the same error envelope as the rest of the API plus X-Credits-* headers.
func (s *Server) enforceQuota(ctx context.Context, w http.ResponseWriter, r *http.Request, cost int64) bool {
	if s == nil || s.cfg == nil || s.cfg.CreditLimit <= 0 || s.usage == nil {
		return true
	}
	snap, err := s.usage.Snapshot(ctx, APIKeyFromContext(ctx))
	if err != nil {
		if s.log != nil {
			s.log.Warn("quota check failed; allowing request", "error", err)
		}
		return true
	}
	limit := int64(s.cfg.CreditLimit)
	remaining := limit - snap.Credits
	if remaining < 0 {
		remaining = 0
	}
	w.Header().Set("X-Credits-Limit", strconv.FormatInt(limit, 10))
	w.Header().Set("X-Credits-Used", strconv.FormatInt(snap.Credits, 10))
	w.Header().Set("X-Credits-Remaining", strconv.FormatInt(remaining, 10))
	if !usage.QuotaAllows(snap.Credits, limit, cost) {
		writeError(w, r, Errorf(http.StatusPaymentRequired,
			"Credit limit exceeded: used %d of %d credits (request needs %d)", snap.Credits, limit, cost))
		return false
	}
	return true
}
