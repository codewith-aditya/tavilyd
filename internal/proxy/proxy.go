// Package proxy implements the outbound proxy pool, health scoring and the
// direct-vs-proxy policy engine. Credentials are never logged: only
// scheme://host:port is ever rendered.
package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Outcome classifies one proxied (or direct) fetch for scoring and metrics.
type Outcome int

const (
	OutcomeSuccess Outcome = iota
	OutcomeBlocked
	OutcomeTimeout
	OutcomeFailure
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeBlocked:
		return "blocked"
	case OutcomeTimeout:
		return "timeout"
	default:
		return "failure"
	}
}

// Metrics is the observability surface used by the pool.
type Metrics interface {
	ProxyRequest(outcome string)
	ProxyLatency(d time.Duration)
	ProxyPoolSize(state string, n int)
}

type nopMetrics struct{}

func (nopMetrics) ProxyRequest(string)        {}
func (nopMetrics) ProxyLatency(time.Duration) {}
func (nopMetrics) ProxyPoolSize(string, int)  {}

// Proxy is one upstream proxy endpoint.
type Proxy struct {
	URL *url.URL
	// Insecure skips TLS verification of an HTTPS proxy endpoint (many
	// commercial gateways use self-signed certs). Never affects the target.
	Insecure bool
}

// Parse validates a proxy URL: http, https, socks5 or socks5h with host:port.
func Parse(raw string) (*Proxy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty proxy url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy url: %w", err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, errors.New("proxy url must include host and port")
	}
	if u.Fragment != "" || u.RawQuery != "" {
		return nil, errors.New("proxy url must not carry query or fragment")
	}
	return &Proxy{URL: u}, nil
}

// HostPort returns host:port without credentials.
func (p *Proxy) HostPort() string {
	if p == nil || p.URL == nil {
		return ""
	}
	return p.URL.Host
}

// Redacted renders scheme://host:port (no userinfo), safe for logs and errors.
func (p *Proxy) Redacted() string {
	if p == nil || p.URL == nil {
		return ""
	}
	return p.URL.Scheme + "://" + p.URL.Host
}

// credentials returns the decoded userinfo.
func (p *Proxy) credentials() (user, pass string, ok bool) {
	if p == nil || p.URL == nil || p.URL.User == nil {
		return "", "", false
	}
	user = p.URL.User.Username()
	pass, _ = p.URL.User.Password()
	return user, pass, true
}

// RenderSticky applies a username template such as "{user}-session-{id}" to
// the proxy credentials, preserving the password. Used for rotating gateways.
func RenderSticky(tmpl string, p *Proxy, session string) (*Proxy, error) {
	if p == nil || tmpl == "" || session == "" {
		return p, nil
	}
	user, pass, ok := p.credentials()
	if !ok {
		return nil, errors.New("sticky sessions require credentials with a username")
	}
	newUser := strings.ReplaceAll(tmpl, "{user}", user)
	newUser = strings.ReplaceAll(newUser, "{id}", session)
	if newUser == "" {
		return nil, errors.New("sticky template rendered an empty username")
	}
	u := *p.URL
	u.User = url.UserPassword(newUser, pass)
	return &Proxy{URL: &u}, nil
}

// RuleAction is a per-host override.
type RuleAction int

const (
	RuleDirect RuleAction = iota
	RuleProxy
)

// Rule matches an exact host or a "*.suffix" pattern.
type Rule struct {
	Pattern string
	Action  RuleAction
}

// ParseRules accepts JSON ({"hosts":{"docs.python.org":"direct"}}) or a small
// YAML-like file of "host: direct|proxy" lines with # comments.
func ParseRules(data []byte) ([]Rule, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var flat map[string]string
		if err := json.Unmarshal([]byte(trimmed), &flat); err == nil {
			return rulesFromMap(flat)
		}
		var wrapped struct {
			Hosts map[string]string `json:"hosts"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapped); err != nil {
			return nil, fmt.Errorf("host rules JSON: %w", err)
		}
		return rulesFromMap(wrapped.Hosts)
	}
	out := make([]Rule, 0, 8)
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		host, action, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("host rules line %q must be host: direct|proxy", line)
		}
		r, err := newRule(strings.TrimSpace(host), strings.TrimSpace(action))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func rulesFromMap(m map[string]string) ([]Rule, error) {
	out := make([]Rule, 0, len(m))
	for host, action := range m {
		r, err := newRule(host, action)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out, nil
}

func newRule(host, action string) (Rule, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return Rule{}, errors.New("host rule has an empty host")
	}
	switch strings.ToLower(action) {
	case "direct":
		return Rule{Pattern: host, Action: RuleDirect}, nil
	case "proxy":
		return Rule{Pattern: host, Action: RuleProxy}, nil
	default:
		return Rule{}, fmt.Errorf("host rule %q: action must be direct or proxy", host)
	}
}

// MatchRule returns the first matching rule for host.
func MatchRule(rules []Rule, host string) (RuleAction, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, r := range rules {
		if r.Pattern == host {
			return r.Action, true
		}
		if strings.HasPrefix(r.Pattern, "*.") {
			suffix := strings.TrimPrefix(r.Pattern, "*")
			if strings.HasSuffix(host, suffix) {
				return r.Action, true
			}
		}
	}
	return RuleDirect, false
}
