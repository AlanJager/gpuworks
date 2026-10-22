package proto

import (
	"bufio"
	"crypto/tls"
	"net"
	"strings"
	"testing"
)

func TestServingForwardTokenIsPerNodeAndInstance(t *testing.T) {
	a := ServingForwardToken("node-a", "inst-1")
	if len(a) != 64 || a == ServingForwardToken("node-a", "inst-2") || a == ServingForwardToken("node-b", "inst-1") {
		t.Fatalf("token %q is not bound to both node and instance", a)
	}
	if a != ServingForwardToken("node-a", "inst-1") {
		t.Fatal("token is not deterministic")
	}
	if !ValidServingForwardToken("node-a", "inst-1", a) || ValidServingForwardToken("node-a", "inst-1", a[:63]) || ValidServingForwardToken("node-b", "inst-1", a) {
		t.Fatal("token validation is wrong")
	}
}

// handshake runs a TLS server with serverToken's certificate and a client
// pinned to clientToken, returning the client's handshake error.
func handshake(t *testing.T, serverToken, clientToken string) error {
	t.Helper()
	cert, err := ServingForwardCertificate(serverToken)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		if strings.HasPrefix(line, ServingForwardMagic) {
			_, _ = c.Write([]byte("OK\n"))
		}
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := tls.Client(raw, ServingForwardClientTLS(clientToken))
	defer c.Close()
	if err := c.Handshake(); err != nil {
		return err
	}
	_, _ = c.Write([]byte(ServingForwardMagic + " x y\n"))
	reply, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || reply != "OK\n" {
		t.Fatalf("reply %q, %v", reply, err)
	}
	return nil
}

func TestServingForwardTLSPinsTheNodeKey(t *testing.T) {
	if err := handshake(t, "node-token", "node-token"); err != nil {
		t.Fatalf("same node: %v", err)
	}
	// A certificate generated again (new serial) still carries the same key.
	if err := handshake(t, "node-token", "node-token"); err != nil {
		t.Fatalf("regenerated certificate: %v", err)
	}
	if err := handshake(t, "other-node", "node-token"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("foreign node key err = %v", err)
	}
}
