package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func startConnectProxy(t *testing.T) (addr string, gotAuth chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gotAuth = make(chan string, 1)
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		line, _ := br.ReadString('\n')
		auth := ""
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(strings.ToLower(h), "proxy-authorization:") {
				auth = strings.TrimSpace(h)
			}
			if h == "\r\n" || h == "\n" {
				break
			}
		}
		gotAuth <- line + auth
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		_, _ = io.Copy(conn, conn)
	}()
	return ln.Addr().String(), gotAuth
}

func TestDialHTTPConnect(t *testing.T) {
	addr, authCh := startConnectProxy(t)
	p, err := Parse("http://user:secret@" + addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := Dial(context.Background(), p, "example.com:443", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
	got := <-authCh
	if !strings.Contains(got, "CONNECT example.com:443") {
		t.Fatalf("CONNECT line = %q", got)
	}
	if !strings.Contains(got, "Proxy-Authorization: Basic") {
		t.Fatalf("missing proxy auth: %q", got)
	}
}

func startSOCKS5Proxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		head := make([]byte, 2)
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		methods := make([]byte, int(head[1]))
		_, _ = io.ReadFull(conn, methods)
		_, _ = conn.Write([]byte{0x05, 0x00})
		req := make([]byte, 4)
		if _, err := io.ReadFull(conn, req); err != nil {
			return
		}
		switch req[3] {
		case 0x01:
			buf := make([]byte, 4+2)
			_, _ = io.ReadFull(conn, buf)
		case 0x03:
			l := make([]byte, 1)
			_, _ = io.ReadFull(conn, l)
			buf := make([]byte, int(l[0])+2)
			_, _ = io.ReadFull(conn, buf)
		case 0x04:
			buf := make([]byte, 16+2)
			_, _ = io.ReadFull(conn, buf)
		}
		_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		_, _ = io.Copy(conn, conn)
	}()
	return ln.Addr().String()
}

func TestDialSOCKS5(t *testing.T) {
	addr := startSOCKS5Proxy(t)
	p, err := Parse("socks5://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := Dial(context.Background(), p, "93.184.216.34:80", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("pong"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "pong" {
		t.Fatalf("echo = %q", buf)
	}
}

func TestDialRejectsProxyError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		_, _ = conn.Read(buf)
		_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
	}()
	p, _ := Parse("http://" + ln.Addr().String())
	if _, err := Dial(context.Background(), p, "example.com:80", 2*time.Second); err == nil {
		t.Fatal("expected CONNECT failure")
	} else if strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "pass") {
		t.Fatalf("credentials in error: %v", err)
	}
}

var _ = fmt.Sprintf
