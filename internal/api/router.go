package api

import "net/http"

// Router wires the public endpoints and the middleware chain.
func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /search", s.HandleSearch)
	mux.HandleFunc("POST /extract", s.HandleExtract)
	mux.HandleFunc("POST /exa/search", s.HandleExaSearch)
	mux.HandleFunc("POST /exa/contents", s.HandleExaContents)
	mux.HandleFunc("GET /health", s.HandleHealth)
	mux.HandleFunc("GET /livez", s.HandleLivez)
	mux.HandleFunc("GET /readyz", s.HandleReadyz)
	mux.HandleFunc("POST /map", s.HandleMap)
	mux.HandleFunc("POST /feedback", s.HandleFeedback)
	mux.HandleFunc("GET /usage", s.HandleUsage)
	mux.HandleFunc("POST /crawl", s.HandleCrawl)
	mux.HandleFunc("POST /research", s.HandleResearchCreate)
	mux.HandleFunc("GET /research/{request_id}", s.HandleResearchGet)
	mux.HandleFunc("GET /{$}", s.HandleRoot)
	mux.HandleFunc("/", s.HandleNotFound)

	return Chain(mux,
		RequestIDMiddleware,
		RecoverMiddleware(s.log),
		APIKeyContextMiddleware(s.auth),
		UsageMiddleware(s.usage, s.auth),
		BodyLimitMiddleware(s.cfg.MaxBodyBytes),
		AccessLogMiddleware(s.log, s.metrics),
		RateLimitMiddleware(s.limiter, s.metrics),
	)
}
