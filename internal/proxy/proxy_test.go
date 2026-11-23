package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAndRedaction(t *testing.T) {
	p, err := Parse("http://user:secret@gateway.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	if p.Redacted() != "http://gateway.example:8080" {
		t.Fatalf("redacted = %q", p.Redacted())
	}
	if strings.Contains(p.Redacted(), "secret") || strings.Contains(p.Redacted(), "user") {
		t.Fatal("credentials leaked in redacted form")
	}
	if _, err := Parse("ftp://host:21"); err == nil {
		t.Fatal("unsupported scheme accepted")
	}
	if _, err := Parse("http://host"); err == nil {
		t.Fatal("missing port accepted")
	}
	if _, err := Parse("http://host:8080?x=1"); err == nil {
		t.Fatal("query accepted")
	}
}

func TestStickyTemplate(t *testing.T) {
	p, _ := Parse("http://user:pass@gateway:8080")
	out, err := RenderSticky("{user}-session-{id}", p, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	user := out.URL.User.Username()
	if user != "user-session-abc123" {
		t.Fatalf("username = %q", user)
	}
	pass, _ := out.URL.User.Password()
	if pass != "pass" {
		t.Fatalf("password changed: %q", pass)
	}
	if !strings.Contains(out.Redacted(), "gateway:8080") || strings.Contains(out.Redacted(), "abc123") {
		t.Fatalf("redacted sticky = %q", out.Redacted())
	}
}

func TestParseRulesAndMatch(t *testing.T) {
	rules, err := ParseRules([]byte(`
# comment
docs.python.org: direct
*.google.com: proxy
`))
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := MatchRule(rules, "docs.python.org"); !ok || a != RuleDirect {
		t.Fatalf("exact rule: %v %v", a, ok)
	}
	if a, ok := MatchRule(rules, "www.google.com"); !ok || a != RuleProxy {
		t.Fatalf("wildcard rule: %v %v", a, ok)
	}
	if _, ok := MatchRule(rules, "example.com"); ok {
		t.Fatal("unexpected match")
	}
	jsonRules, err := ParseRules([]byte(`{"hosts":{"example.com":"proxy"}}`))
	if err != nil || len(jsonRules) != 1 {
		t.Fatalf("json rules: %v %v", jsonRules, err)
	}
}

type fakePool struct {
	picked  int
	reports int
	lastOut Outcome
	fail    bool
}

func (f *fakePool) Pick(context.Context, string, string) (*Proxy, error) {
	if f.fail {
		return nil, ErrNoHealthyProxy
	}
	f.picked++
	p, _ := Parse("http://gw.example:8080")
	return p, nil
}
func (f *fakePool) Report(_ *Proxy, _ string, o Outcome, _ time.Duration) {
	f.reports++
	f.lastOut = o
}
func (f *fakePool) Snapshot() Snapshot {
	return Snapshot{Total: 1, Healthy: 1}
}
func (f *fakePool) Close() error { return nil }

func TestPolicyDirectFirstAndMemory(t *testing.T) {
	pool := &fakePool{}
	eng := NewEngine("rotating", "direct_first", pool, nil, time.Hour, nil)
	now := time.Unix(1700000000, 0)
	eng.now = func() time.Time { return now }

	if r := eng.Decide(context.Background(), "example.com", "s"); r.UseProxy {
		t.Fatalf("direct_first sent a fresh host via proxy: %+v", r)
	}
	// A blocked direct request moves the host to proxy for the TTL.
	eng.Report("example.com", nil, OutcomeBlocked, 100*time.Millisecond, true)
	if r := eng.Decide(context.Background(), "example.com", "s"); !r.UseProxy {
		t.Fatalf("blocked host should use proxy: %+v", r)
	}
	now = now.Add(2 * time.Hour)
	if r := eng.Decide(context.Background(), "example.com", "s"); r.UseProxy {
		t.Fatalf("host memory did not expire: %+v", r)
	}
}

func TestPolicyModesAndRules(t *testing.T) {
	pool := &fakePool{}
	eng := NewEngine("rotating", "always", pool, []Rule{{Pattern: "docs.python.org", Action: RuleDirect}}, time.Hour, nil)
	if r := eng.Decide(context.Background(), "example.com", ""); !r.UseProxy {
		t.Fatalf("always should proxy: %+v", r)
	}
	if r := eng.Decide(context.Background(), "docs.python.org", ""); r.UseProxy {
		t.Fatalf("host rule should force direct: %+v", r)
	}
	eng2 := NewEngine("rotating", "proxy_first", pool, nil, time.Hour, nil)
	if r := eng2.Decide(context.Background(), "example.com", ""); !r.UseProxy {
		t.Fatalf("proxy_first should proxy: %+v", r)
	}
	eng3 := NewEngine("off", "always", pool, nil, time.Hour, nil)
	if r := eng3.Decide(context.Background(), "example.com", ""); r.UseProxy {
		t.Fatalf("off mode proxied: %+v", r)
	}
}

func TestPoolUnhealthyDegradesToDirect(t *testing.T) {
	pool := &fakePool{fail: true}
	eng := NewEngine("rotating", "always", pool, nil, time.Hour, nil)
	r := eng.Decide(context.Background(), "example.com", "")
	if r.UseProxy {
		t.Fatal("unhealthy pool still proxied")
	}
	if !eng.Degraded() {
		t.Fatal("degraded flag not set")
	}
}

func TestListHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")
	if err := os.WriteFile(path, []byte("# one\nhttp://a.example:8080\nhttp://b.example:8081\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := NewList(path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if s := l.Snapshot(); s.Total != 2 {
		t.Fatalf("total = %d", s.Total)
	}
	if err := os.WriteFile(path, []byte("http://c.example:8082\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	if s := l.Snapshot(); s.Total != 1 {
		t.Fatalf("after reload total = %d", s.Total)
	}
}

func TestScoringEjectsAfterThreeFailures(t *testing.T) {
	l := &List{path: "x", metrics: nopMetrics{}, stop: make(chan struct{})}
	p, _ := Parse("http://p1:8080")
	l.entries = []*entry{{proxy: p}}
	for i := 0; i < 3; i++ {
		l.Report(p, "example.com", OutcomeFailure, 100*time.Millisecond)
	}
	if _, err := l.Pick(context.Background(), "example.com", ""); !errors.Is(err, ErrNoHealthyProxy) {
		t.Fatalf("expected ejection, got %v", err)
	}
	// Re-probe after the backoff window.
	l.mu.Lock()
	l.entries[0].st.mu.Lock()
	l.entries[0].st.ejectedUntil = time.Now().Add(-time.Second)
	l.entries[0].st.mu.Unlock()
	l.mu.Unlock()
	if _, err := l.Pick(context.Background(), "example.com", ""); err != nil {
		t.Fatalf("re-probe failed: %v", err)
	}
}
