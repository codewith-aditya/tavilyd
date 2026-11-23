package extract

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astralabs/tavilyd/internal/proxy"
	"github.com/astralabs/tavilyd/internal/security"
)

// fakeConnectProxy forwards every CONNECT to forwardTo, ignoring the requested
// address (enough to prove the fetcher tunnelled through it).
func fakeConnectProxy(t *testing.T, forwardTo string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if !strings.HasPrefix(line, "CONNECT ") {
					return
				}
				for {
					h, err := br.ReadString('\n')
					if err != nil || h == "\r\n" || h == "\n" {
						break
					}
				}
				up, err := net.Dial("tcp", forwardTo)
				if err != nil {
					_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				go io.Copy(up, br)
				_, _ = io.Copy(c, up)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func newProxyEngine(t *testing.T, endpoint, policy string) *proxy.Engine {
	t.Helper()
	pool, err := proxy.NewRotating("http://"+endpoint, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return proxy.NewEngine("rotating", policy, pool, nil, time.Hour, nil)
}

func TestFetcherUsesProxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html><body><article><p>PROXIED content from the target page.</p></article></body></html>")
	}))
	defer target.Close()
	proxyAddr := fakeConnectProxy(t, strings.TrimPrefix(target.URL, "http://"))

	guard := security.NewGuard(true, 2*time.Second)
	f := NewHTTPFetcher(guard, 1<<20, "ua", 3*time.Second, 3, false)
	f.SetProxyPolicy(newProxyEngine(t, proxyAddr, "always"), 1)

	u, _ := target.URL, 0
	tgt, err := guard.ValidateURL(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.Fetch(context.Background(), tgt)
	if err != nil {
		t.Fatalf("fetch via proxy: %v", err)
	}
	if !strings.Contains(string(page.Body), "PROXIED") {
		t.Fatalf("unexpected body: %s", page.Body)
	}
}

func TestFetcherRetriesBlockedDirectViaProxy(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "blocked")
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html><body><article><p>RETRIED through proxy successfully.</p></article></body></html>")
	}))
	defer good.Close()
	proxyAddr := fakeConnectProxy(t, strings.TrimPrefix(good.URL, "http://"))

	guard := security.NewGuard(true, 2*time.Second)
	f := NewHTTPFetcher(guard, 1<<20, "ua", 5*time.Second, 3, false)
	f.SetProxyPolicy(newProxyEngine(t, proxyAddr, "direct_first"), 1)
	tgt, err := guard.ValidateURL(context.Background(), bad.URL)
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.Fetch(context.Background(), tgt)
	if err != nil {
		t.Fatalf("retry fetch: %v", err)
	}
	if !strings.Contains(string(page.Body), "RETRIED") {
		t.Fatalf("expected proxied retry body, got %s", page.Body)
	}
}

var _ = fmt.Sprintf
