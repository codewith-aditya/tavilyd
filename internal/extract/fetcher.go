package extract

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/astralabs/tavilyd/internal/proxy"
	"github.com/astralabs/tavilyd/internal/security"
)

var (
	// ErrTooLarge means the body exceeded FETCH_MAX_BYTES.
	ErrTooLarge = errors.New("response body exceeds size limit")
	// ErrUnsupportedType means the response was not HTML or text.
	ErrUnsupportedType = errors.New("unsupported content type")
	// ErrNotHTML is returned when a page is served as HTML but contains no markup.
	ErrNotHTML = errors.New("response is not html")
)

var errTooManyRedirects = errors.New("too many redirects")

// HTTPFetcher is tier 1: a raw HTTP/1.1 client that speaks with Chrome's TLS
// fingerprint via uTLS and dials only SSRF-approved IPs.
type HTTPFetcher struct {
	policy       *proxy.Engine
	proxyRetries int
	proxyExtra   time.Duration
	guard        *security.Guard
	maxBytes     int64
	userAgent    string
	timeout      time.Duration
	maxRedirects int
	fingerprint  bool
}

// NewHTTPFetcher builds tier 1.
func NewHTTPFetcher(guard *security.Guard, maxBytes int64, userAgent string, timeout time.Duration, maxRedirects int, fingerprint bool) *HTTPFetcher {
	return &HTTPFetcher{
		guard: guard, maxBytes: maxBytes, userAgent: userAgent,
		timeout: timeout, maxRedirects: maxRedirects, fingerprint: fingerprint,
	}
}

// SetProxyPolicy wires the policy engine (nil keeps direct-only behaviour).
func (f *HTTPFetcher) SetProxyPolicy(engine *proxy.Engine, retries int, extra ...time.Duration) {
	f.policy = engine
	f.proxyRetries = retries
	if len(extra) > 0 {
		f.proxyExtra = extra[0]
	}
}

// Name implements Fetcher.
func (f *HTTPFetcher) Name() string { return "http" }

type fetchResult struct {
	page      *Page
	location  string
	usedProxy *proxy.Proxy
	blocked   bool
	err       error
}

var internalTargets = map[string]struct{}{
	"searxng": {}, "redis": {}, "browser": {}, "reranker": {}, "localhost": {},
	"metadata": {}, "metadata.google.internal": {},
}

// Fetch performs the request with manual redirect handling so every hop is
// re-validated against the SSRF guard.
func (f *HTTPFetcher) Fetch(ctx context.Context, target *securityTarget) (*Page, error) {
	if target == nil {
		return nil, errors.New("nil target")
	}
	timeout := f.timeout
	if f.policy != nil && f.policy.Enabled() {
		timeout += f.proxyExtra
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cur := target
	for hop := 0; ; hop++ {
		// The direct attempt gets the normal per-page budget; a proxy retry may
		// use the extra budget that Fetch reserved for proxied requests.
		firstCtx, firstCancel := context.WithTimeout(ctx, f.timeout)
		res := f.do(firstCtx, cur, false)
		firstCancel()
		shouldRetry := res.blocked
		if !shouldRetry && res.err != nil && remaining(ctx) > 3*time.Second {
			shouldRetry = true
		}
		if shouldRetry && f.policy != nil && f.policy.Enabled() &&
			res.usedProxy == nil && f.proxyRetries > 0 && remaining(ctx) > 1200*time.Millisecond {
			retry := f.do(ctx, cur, true)
			if retry.err == nil && retry.page != nil {
				res = retry
			}
		}
		if res.err != nil {
			return nil, res.err
		}
		if res.location == "" {
			return res.page, nil
		}
		if hop >= f.maxRedirects {
			return nil, errTooManyRedirects
		}
		next, err := resolveRedirect(cur.URL, res.location)
		if err != nil {
			return nil, err
		}
		// Re-validate and re-pin: a redirect must not smuggle us inward.
		cur, err = f.guard.ValidateURL(ctx, next)
		if err != nil {
			return nil, err
		}
	}
}

func (f *HTTPFetcher) do(ctx context.Context, target *security.Target, forceProxy bool) (res fetchResult) {
	started := time.Now()
	defer func() {
		if f.policy == nil || !f.policy.Enabled() {
			return
		}
		outcome := proxy.OutcomeSuccess
		switch {
		case res.err != nil:
			if errors.Is(res.err, context.DeadlineExceeded) {
				outcome = proxy.OutcomeTimeout
			} else {
				outcome = proxy.OutcomeFailure
			}
		case res.blocked:
			outcome = proxy.OutcomeBlocked
		}
		f.policy.Report(target.Host, res.usedProxy, outcome, time.Since(started), res.blocked)
	}()

	route := proxy.Route{}
	if f.policy != nil {
		if forceProxy {
			route = f.policy.DecideProxy(ctx, target.Host, target.Host, "retry_blocked")
		} else {
			route = f.policy.Decide(ctx, target.Host, target.Host)
		}
	}
	if _, internal := internalTargets[strings.ToLower(target.Host)]; internal && route.UseProxy {
		res.err = fmt.Errorf("%w: internal host must not go through a proxy", security.ErrBlockedAddress)
		return res
	}
	if route.UseProxy && route.Proxy != nil && strings.EqualFold(route.Proxy.HostPort(), net.JoinHostPort(target.Host, target.Port)) {
		res.err = fmt.Errorf("%w: target is the proxy endpoint", security.ErrBlockedAddress)
		return res
	}

	var (
		conn net.Conn
		err  error
	)
	if route.UseProxy && route.Proxy != nil {
		if aerr := f.policy.Acquire(ctx, route.Proxy); aerr != nil {
			res.err = aerr
			return res
		}
		defer f.policy.Release(route.Proxy)
		addr := net.JoinHostPort(target.IPs[0].String(), target.Port)
		conn, err = proxy.Dial(ctx, route.Proxy, addr, 3*time.Second)
		if err == nil {
			res.usedProxy = route.Proxy
		}
	} else {
		conn, err = f.guard.DialTarget(ctx, "tcp", target)
	}
	if err != nil {
		res.err = err
		return res
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	var rw io.Writer = conn
	var rr io.Reader = conn
	if target.URL.Scheme == "https" {
		cfg := &utls.Config{
			ServerName:         target.Host,
			NextProtos:         []string{"http/1.1"},
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: false,
		}
		uconn, err := f.tlsClient(conn, cfg)
		if err != nil {
			res.err = err
			return res
		}
		if err := uconn.HandshakeContext(ctx); err != nil {
			res.err = fmt.Errorf("tls handshake: %w", err)
			return res
		}
		rw, rr = uconn, uconn
	}

	req := &http.Request{
		Method:     http.MethodGet,
		URL:        target.URL,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Host:       target.Host,
		Header:     f.headers(target.URL),
	}
	if err := req.Write(rw); err != nil {
		res.err = fmt.Errorf("write request: %w", err)
		return res
	}

	br := bufio.NewReaderSize(rr, 32<<10)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		res.err = fmt.Errorf("read response: %w", err)
		return res
	}
	defer resp.Body.Close()

	if loc := resp.Header.Get("Location"); isRedirect(resp.StatusCode) && loc != "" {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		res.location = loc
		return res
	}

	body, err := readCapped(resp.Body, f.maxBytes)
	if err != nil {
		res.err = err
		return res
	}
	if enc := strings.ToLower(resp.Header.Get("Content-Encoding")); enc == "gzip" {
		zr, zerr := gzip.NewReader(bytes.NewReader(body))
		if zerr != nil {
			res.err = fmt.Errorf("gzip: %w", zerr)
			return res
		}
		body, err = readCapped(zr, f.maxBytes)
		_ = zr.Close()
		if err != nil {
			res.err = err
			return res
		}
	}

	ct := resp.Header.Get("Content-Type")
	if !isSupportedContentType(ct) {
		res.err = fmt.Errorf("%w: %q", ErrUnsupportedType, ct)
		return res
	}
	res.page = &Page{
		URL: target.URL.String(), FinalURL: target.URL.String(), Status: resp.StatusCode,
		ContentType: ct, Body: body, Tier: 1, FetchedAt: time.Now(),
	}
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		res.blocked = true
	default:
		res.blocked = IsChallenge(resp.StatusCode, body)
	}
	return res
}

// tlsClient builds a uTLS connection. The Chrome preset advertises h2 in its
// ALPN extension, so the spec is rewritten to http/1.1 (this tier speaks HTTP/1.1).
func (f *HTTPFetcher) tlsClient(conn net.Conn, cfg *utls.Config) (*utls.UConn, error) {
	if !f.fingerprint {
		return utls.UClient(conn, cfg, utls.HelloGolang), nil
	}
	spec, err := chromeHTTP1Spec()
	if err != nil {
		return nil, err
	}
	uconn := utls.UClient(conn, cfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(spec); err != nil {
		return nil, fmt.Errorf("utls preset: %w", err)
	}
	return uconn, nil
}

// chromeHTTP1Spec returns the Chrome fingerprint restricted to HTTP/1.1.
func chromeHTTP1Spec() (*utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		return nil, fmt.Errorf("utls spec: %w", err)
	}
	for i, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
			spec.Extensions[i] = alpn
		}
	}
	return &spec, nil
}

func (f *HTTPFetcher) headers(u *url.URL) http.Header {
	h := http.Header{}
	h.Set("User-Agent", f.userAgent)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Accept-Encoding", "gzip")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("Upgrade-Insecure-Requests", "1")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Sec-Fetch-User", "?1")
	h.Set("Sec-CH-UA", `"Chromium";v="131", "Not_A Brand";v="24", "Google Chrome";v="131"`)
	h.Set("Sec-CH-UA-Mobile", "?0")
	h.Set("Sec-CH-UA-Platform", `"Windows"`)
	h.Set("Referer", u.Scheme+"://"+u.Host+"/")
	return h
}

func remaining(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl)
	}
	return time.Hour
}

func readCapped(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = 5 << 20
	}
	buf, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > max {
		return nil, ErrTooLarge
	}
	return buf, nil
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

func resolveRedirect(base *url.URL, location string) (string, error) {
	ref, err := url.Parse(strings.TrimSpace(location))
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

func isSupportedContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return true // many servers omit it; sniffing happens later
	}
	mt := strings.SplitN(ct, ";", 2)[0]
	switch {
	case strings.HasPrefix(mt, "text/html"),
		strings.HasPrefix(mt, "application/xhtml"),
		strings.HasPrefix(mt, "application/xml"),
		strings.HasPrefix(mt, "text/xml"),
		strings.HasPrefix(mt, "text/plain"):
		return true
	}
	return false
}
