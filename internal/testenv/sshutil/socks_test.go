package sshutil

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// fakeSOCKS5 is an RFC 1928 proxy for the CONNECT command with no
// authentication. It records what it was asked to connect to, answers with
// the status it was told to, and then echoes bytes.
func fakeSOCKS5(t *testing.T, status byte) (addr string, target <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	targets := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		greeting := make([]byte, 3)
		if _, err := io.ReadFull(conn, greeting); err != nil || greeting[0] != 5 || greeting[1] != 1 || greeting[2] != 0 {
			return
		}
		conn.Write([]byte{5, 0})
		head := make([]byte, 4)
		if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 || head[1] != 1 {
			return
		}
		var host string
		switch head[3] {
		case 1:
			ip := make([]byte, 4)
			io.ReadFull(conn, ip)
			host = net.IP(ip).String()
		case 3:
			n := make([]byte, 1)
			io.ReadFull(conn, n)
			name := make([]byte, n[0])
			io.ReadFull(conn, name)
			host = string(name)
		}
		port := make([]byte, 2)
		io.ReadFull(conn, port)
		targets <- net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1])))
		conn.Write([]byte{5, status, 0, 1, 0, 0, 0, 0, 0, 0})
		if status == 0 {
			io.Copy(conn, conn)
		}
	}()
	return listener.Addr().String(), targets
}

func TestDialTCPGoesThroughTheConfiguredSOCKS5Proxy(t *testing.T) {
	proxy, target := fakeSOCKS5(t, 0)
	t.Setenv(proxyEnv, "socks5://"+proxy)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialTCP(ctx, "203.0.113.7:22")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := <-target; got != "203.0.113.7:22" {
		t.Fatalf("proxy was asked for %q", got)
	}
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("tunnel carried %q, %v", buf, err)
	}
}

func TestDialTCPReportsAProxyRefusalAndABadProxyURL(t *testing.T) {
	proxy, _ := fakeSOCKS5(t, 5) // connection refused
	t.Setenv(proxyEnv, "socks5://"+proxy)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dialTCP(ctx, "203.0.113.7:22"); err == nil {
		t.Fatal("a refused CONNECT must be an error")
	}
	t.Setenv(proxyEnv, "http://proxy.invalid:8080")
	if _, err := dialTCP(ctx, "203.0.113.7:22"); err == nil {
		t.Fatal("only socks5 proxies are supported; an http URL must be refused, not ignored")
	}
}

func TestDialTCPIsDirectWithoutAProxy(t *testing.T) {
	t.Setenv(proxyEnv, "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if c, err := listener.Accept(); err == nil {
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialTCP(ctx, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}
