package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"time"
)

// dialer is injectable for tests.
var dialUpstream = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext

// Dial opens a tunnel to addr (host:port) through p. HTTP CONNECT and SOCKS5
// are both supported; https proxies are TLS-wrapped before CONNECT.
func Dial(ctx context.Context, p *Proxy, addr string, timeout time.Duration) (net.Conn, error) {
	if p == nil || p.URL == nil {
		return nil, errors.New("nil proxy")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dialUpstream(dctx, "tcp", p.HostPort())
	if err != nil {
		return nil, fmt.Errorf("proxy %s: dial: %w", p.Redacted(), err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	if p.URL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: p.URL.Hostname(), MinVersion: tls.VersionTLS12,
			InsecureSkipVerify: p.Insecure, // #nosec G402 -- opt-in for self-signed proxy gateways
		})
		if err := tlsConn.HandshakeContext(dctx); err != nil {
			return nil, fmt.Errorf("proxy %s: tls: %w", p.Redacted(), err)
		}
		conn = tlsConn
	}

	switch p.URL.Scheme {
	case "socks5", "socks5h":
		if err := socks5Connect(conn, p, addr); err != nil {
			return nil, err
		}
	default:
		if err := httpConnect(conn, p, addr); err != nil {
			return nil, err
		}
	}
	ok = true
	return conn, nil
}

func httpConnect(conn net.Conn, p *Proxy, addr string) error {
	var auth string
	if user, pass, has := p.credentials(); has {
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)) + "\r\n"
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n" + auth + "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return fmt.Errorf("proxy %s: connect write: %w", p.Redacted(), err)
	}
	br := bufio.NewReader(conn)
	tp := textproto.NewReader(br)
	statusLine, err := tp.ReadLine()
	if err != nil {
		return fmt.Errorf("proxy %s: connect read: %w", p.Redacted(), err)
	}
	parts := stringsSplitN(statusLine, " ", 3)
	if len(parts) < 2 || parts[1] != "200" {
		return fmt.Errorf("proxy %s: CONNECT failed: %s", p.Redacted(), statusLine)
	}
	if _, err := tp.ReadMIMEHeader(); err != nil {
		return fmt.Errorf("proxy %s: connect headers: %w", p.Redacted(), err)
	}
	return nil
}

// bufferedConn preserves bytes the proxy sent after the CONNECT response.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func socks5Connect(conn net.Conn, p *Proxy, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("socks5: bad target %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("socks5: bad target port %q", portStr)
	}
	user, pass, hasAuth := p.credentials()
	methods := []byte{0x00}
	if hasAuth {
		methods = []byte{0x00, 0x02}
	}
	greeting := []byte{0x05, byte(len(methods))}
	greeting = append(greeting, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("socks5 %s: greeting: %w", p.Redacted(), err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5 %s: method: %w", p.Redacted(), err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("socks5 %s: bad version %d", p.Redacted(), resp[0])
	}
	if resp[1] == 0x02 {
		authReq := []byte{0x01, byte(len(user))}
		authReq = append(authReq, user...)
		authReq = append(authReq, byte(len(pass)))
		authReq = append(authReq, pass...)
		if _, err := conn.Write(authReq); err != nil {
			return fmt.Errorf("socks5 %s: auth write: %w", p.Redacted(), err)
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(conn, ar); err != nil {
			return fmt.Errorf("socks5 %s: auth read: %w", p.Redacted(), err)
		}
		if ar[1] != 0x00 {
			return fmt.Errorf("socks5 %s: auth rejected", p.Redacted())
		}
	} else if resp[1] != 0x00 {
		return fmt.Errorf("socks5 %s: no acceptable auth method", p.Redacted())
	}

	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5 %s: hostname too long", p.Redacted())
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5 %s: connect write: %w", p.Redacted(), err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("socks5 %s: connect read: %w", p.Redacted(), err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5 %s: connect failed code %d", p.Redacted(), head[1])
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return err
		}
		skip = int(l[0])
	default:
		return fmt.Errorf("socks5 %s: bad address type %d", p.Redacted(), head[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip+2)); err != nil {
		return fmt.Errorf("socks5 %s: connect tail: %w", p.Redacted(), err)
	}
	return nil
}

func stringsSplitN(s, sep string, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep[0] {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

var _ = http.StatusOK
