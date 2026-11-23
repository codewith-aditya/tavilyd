package security

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type fakeResolver struct {
	ips   []net.IP
	calls atomic.Int32
}

func (f *fakeResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	f.calls.Add(1)
	out := make([]net.IPAddr, 0, len(f.ips))
	for _, ip := range f.ips {
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

type fakeDialer struct {
	got  atomic.Value
	fail error
}

func (f *fakeDialer) dial(_ context.Context, _, addr string) (net.Conn, error) {
	f.got.Store(addr)
	return nil, f.fail
}

func TestBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255", "::1", "fc00::1",
		"fe80::1", "::ffff:127.0.0.1", "100.100.100.200", "168.63.129.16",
	}
	for _, s := range blocked {
		if bad, _ := BlockedIP(net.ParseIP(s)); !bad {
			t.Errorf("expected %s to be blocked", s)
		}
	}
	allowed := []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if bad, why := BlockedIP(net.ParseIP(s)); bad {
			t.Errorf("expected %s to be allowed, blocked by %s", s, why)
		}
	}
}

func TestValidateURLBlocksUnsafeTargets(t *testing.T) {
	g := NewGuardWithResolver(&fakeResolver{ips: []net.IP{net.ParseIP("127.0.0.1")}}, false, time.Second, nil)
	cases := []string{
		"http://127.0.0.1/",
		"http://localhost/",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/admin",
		"http://100.64.0.1/",
		"file:///etc/passwd",
		"gopher://example.com/",
		"http://user:pass@example.com/",
		"http://example.com@127.0.0.1/",
		"http://metadata.google.internal/",
	}
	for _, raw := range cases {
		if _, err := g.ValidateURL(context.Background(), raw); err == nil {
			t.Errorf("expected %s to be rejected", raw)
		}
	}
}

func TestValidateURLAllowsPublicTarget(t *testing.T) {
	r := &fakeResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}}
	g := NewGuardWithResolver(r, false, time.Second, nil)
	target, err := g.ValidateURL(context.Background(), "https://example.com/a?b=1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.Port != "443" || len(target.IPs) != 1 {
		t.Fatalf("unexpected target: %+v", target)
	}
}

// TestDNSRebindingIsPrevented proves that dialing uses the single approved
// resolution: a resolver that flips to 127.0.0.1 after validation must not
// influence the dialed address, and must not be consulted again.
func TestDNSRebindingIsPrevented(t *testing.T) {
	r := &fakeResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}}
	d := &fakeDialer{fail: errors.New("stop")}
	g := NewGuardWithResolver(r, false, time.Second, d.dial)

	target, err := g.ValidateURL(context.Background(), "http://rebind.example.com/")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	r.ips = []net.IP{net.ParseIP("127.0.0.1")} // attacker flips the record
	_, _ = g.DialTarget(context.Background(), "tcp", target)

	if got := r.calls.Load(); got != 1 {
		t.Fatalf("resolver called %d times, want 1 (dial must reuse the pinned IP)", got)
	}
	addr, _ := d.got.Load().(string)
	if addr != "93.184.216.34:80" {
		t.Fatalf("dialed %q, want pinned public IP", addr)
	}
}

func TestDialRejectsPrivateResolution(t *testing.T) {
	r := &fakeResolver{ips: []net.IP{net.ParseIP("10.0.0.5")}}
	g := NewGuardWithResolver(r, false, time.Second, nil)
	if _, err := g.DialContext(context.Background(), "tcp", "internal.example.com:80"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("want ErrBlockedAddress, got %v", err)
	}
}

func TestCheckRedirectValidatesEveryHop(t *testing.T) {
	g := NewGuardWithResolver(&fakeResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}}, false, time.Second, nil)
	check := g.CheckRedirect(3)
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest", nil)
	if err := check(req, []*http.Request{}); err == nil {
		t.Fatal("expected redirect to metadata IP to be rejected")
	}
	req2, _ := http.NewRequest(http.MethodGet, "https://example.com/next", nil)
	if err := check(req2, []*http.Request{}); err != nil {
		t.Fatalf("public redirect rejected: %v", err)
	}
	via := make([]*http.Request, 3)
	for i := range via {
		via[i] = req2
	}
	if err := check(req2, via); err == nil {
		t.Fatal("expected redirect cap to trigger")
	}
}

func TestAllowPrivateOptIn(t *testing.T) {
	g := NewGuardWithResolver(&fakeResolver{ips: []net.IP{net.ParseIP("127.0.0.1")}}, true, time.Second, nil)
	if _, err := g.ValidateURL(context.Background(), "http://localhost:8080/"); err != nil {
		t.Fatalf("allowPrivate should permit localhost: %v", err)
	}
}

func TestAllowlistPermitsOnlyListedPrivateTargets(t *testing.T) {
	r := &fakeResolver{ips: []net.IP{net.ParseIP("127.0.0.1")}}
	g := NewGuardWithResolver(r, false, time.Second, nil)
	if err := g.SetAllowlist([]string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.ValidateURL(context.Background(), "http://localhost:8080/"); err != nil {
		t.Fatalf("allowlisted loopback rejected: %v", err)
	}
	r.ips = []net.IP{net.ParseIP("169.254.169.254")}
	if _, err := g.ValidateURL(context.Background(), "http://metadata.example.com/"); err == nil {
		t.Fatal("metadata endpoint must stay blocked with an allowlist present")
	}
	if err := g.SetAllowlist([]string{"not-a-cidr/33"}); err == nil {
		t.Fatal("invalid CIDR accepted")
	}
}

func TestControlRechecksKernelAddress(t *testing.T) {
	g := NewGuardWithResolver(&fakeResolver{}, false, time.Second, nil)
	if err := g.Control("tcp", "127.0.0.1:80", nil); err == nil {
		t.Fatal("control hook must reject loopback")
	}
	if err := g.Control("tcp", "1.1.1.1:80", nil); err != nil {
		t.Fatalf("control hook rejected public IP: %v", err)
	}
}

// Numeric IP encodings must not bypass the guard when the resolver maps them to
// loopback (the OS resolver may normalise decimal/hex/octal forms).
func TestNumericIPFormsBlocked(t *testing.T) {
	r := &fakeResolver{ips: []net.IP{net.ParseIP("127.0.0.1")}}
	g := NewGuardWithResolver(r, false, time.Second, nil)
	for _, raw := range []string{
		"http://2130706433/", "http://0x7f000001/", "http://0177.0.0.1/",
		"http://127.1/", "http://[::ffff:127.0.0.1]/",
	} {
		if _, err := g.ValidateURL(context.Background(), raw); err == nil {
			t.Errorf("%s was not blocked", raw)
		}
	}
}
