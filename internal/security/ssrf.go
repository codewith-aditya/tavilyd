package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned when a URL or DNS answer points at a range we
// refuse to reach (private, loopback, link-local, CGNAT, metadata, ...).
var ErrBlockedAddress = errors.New("destination address is not allowed")

// Resolver is the subset of net.Resolver used by the guard (injectable in tests).
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DialFunc is the dialing primitive; tests inject a fake one.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Guard validates outbound destinations and dials only addresses it approved,
// which defeats DNS rebinding (resolution and connection use the same IPs).
type Guard struct {
	resolver     Resolver
	allowPrivate bool
	timeout      time.Duration
	dial         DialFunc
	allow        []*net.IPNet
	allowHosts   map[string]struct{}
}

// NewGuard builds a guard using the system resolver.
func NewGuard(allowPrivate bool, timeout time.Duration) *Guard {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	g := &Guard{resolver: net.DefaultResolver, allowPrivate: allowPrivate, timeout: timeout, allowHosts: map[string]struct{}{}}
	// Control re-checks the kernel-level destination of every connection.
	d.Control = g.Control
	g.dial = d.DialContext
	return g
}

// SetAllowlist permits specific IPs, CIDRs or hostnames in addition to public
// addresses (used for private test targets without opening every private range).
func (g *Guard) SetAllowlist(entries []string) error {
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			_, n, err := net.ParseCIDR(e)
			if err != nil {
				return fmt.Errorf("SSRF_ALLOWLIST %q: %w", raw, err)
			}
			g.allow = append(g.allow, n)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			g.allow = append(g.allow, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		g.allowHosts[strings.ToLower(e)] = struct{}{}
	}
	return nil
}

func (g *Guard) allowed(ip net.IP, host string) bool {
	if g.allowPrivate {
		return true
	}
	if host != "" {
		if _, ok := g.allowHosts[strings.ToLower(host)]; ok {
			return true
		}
	}
	for _, n := range g.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// NewGuardWithResolver builds a guard around a custom resolver/fdialer.
func NewGuardWithResolver(resolver Resolver, allowPrivate bool, timeout time.Duration, dial DialFunc) *Guard {
	g := NewGuard(allowPrivate, timeout)
	if resolver != nil {
		g.resolver = resolver
	}
	if dial != nil {
		g.dial = dial
	}
	return g
}

// Target is a validated URL with the exact IPs approved for dialing.
type Target struct {
	URL  *url.URL
	Host string
	Port string
	IPs  []net.IP
}

var blockedNets = func() []*net.IPNet {
	cidrs := []string{
		// IPv4
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"255.255.255.255/32",
		// IPv6
		"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "fc00::/7",
		"fe80::/10", "ff00::/8", "2001:10::/28", "2001:20::/28",
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// BlockedIP reports whether ip must not be dialed, with the reason.
func BlockedIP(ip net.IP) (bool, string) {
	if ip == nil {
		return true, "unparsable address"
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if ip.To16() == nil {
		return true, "unparsable address"
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true, "address range " + n.String()
		}
	}
	// Cloud metadata endpoints that live outside the link-local range.
	for _, meta := range []string{"100.100.100.200", "168.63.129.16", "169.254.170.2"} {
		if ip.Equal(net.ParseIP(meta)) {
			return true, "cloud metadata endpoint"
		}
	}
	return false, ""
}

// ValidateURL parses raw, resolves it once and returns the approved target.
func (g *Guard) ValidateURL(ctx context.Context, raw string) (*Target, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q", ErrBlockedAddress, u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: credentials in url", ErrBlockedAddress)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return nil, fmt.Errorf("%w: empty host", ErrBlockedAddress)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return g.resolve(ctx, u, host, port)
}

func (g *Guard) resolve(ctx context.Context, u *url.URL, host, port string) (*Target, error) {
	var ips []net.IP
	if lit := net.ParseIP(host); lit != nil {
		ips = []net.IP{lit}
	} else {
		addrs, err := g.resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	allowed := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if bad, why := BlockedIP(ip); bad && !g.allowed(ip, host) {
			return nil, fmt.Errorf("%w: %s (%s)", ErrBlockedAddress, ip, why)
		}
		allowed = append(allowed, ip)
	}
	if len(allowed) == 0 {
		return nil, ErrBlockedAddress
	}
	return &Target{URL: u, Host: host, Port: port, IPs: allowed}, nil
}

// DialContext validates addr then dials a pre-approved IP, never re-resolving.
func (g *Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	t, err := g.resolve(ctx, nil, strings.ToLower(host), port)
	if err != nil {
		return nil, err
	}
	return g.DialTarget(ctx, network, t)
}

// DialTarget dials the pinned IPs of t in order, with per-attempt deadlines.
func (g *Guard) DialTarget(ctx context.Context, network string, t *Target) (net.Conn, error) {
	var lastErr error
	for _, ip := range t.IPs {
		if bad, why := BlockedIP(ip); bad && !g.allowed(ip, t.Host) {
			lastErr = fmt.Errorf("%w: %s (%s)", ErrBlockedAddress, ip, why)
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, g.timeout)
		conn, err := g.dial(dctx, network, net.JoinHostPort(ip.String(), t.Port))
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = ErrBlockedAddress
	}
	return nil, lastErr
}

// Control is a net.Dialer.Control hook providing defence in depth: even if a
// caller bypasses DialContext the kernel-level address is re-checked.
func (g *Guard) Control(_ string, address string, _ syscall.RawConn) error {
	if g.allowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if bad, why := BlockedIP(ip); bad && !g.allowed(ip, "") {
		return fmt.Errorf("%w: %s (%s)", ErrBlockedAddress, ip, why)
	}
	return nil
}

// CheckRedirect re-validates every hop of a redirect chain.
func (g *Guard) CheckRedirect(max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if max > 0 && len(via) >= max {
			return fmt.Errorf("stopped after %d redirects", max)
		}
		_, err := g.ValidateURL(req.Context(), req.URL.String())
		return err
	}
}
