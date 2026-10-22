package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// servingForwarder is the agent end of the serving network: a TLS listener on
// the node's serving IP that, after a valid per-instance token, splices the
// connection to that instance's loopback port. It is deliberately thin — no
// HTTP parsing — so streaming responses and upgrades pass through untouched.
// See proto.ServingForwardMagic for the protocol.
type servingForwarder struct {
	log        *slog.Logger
	nodeToken  func() string
	port       func(instanceUUID string) (int, bool)
	listen     func(network, address string) (net.Listener, error)
	dial       func(ctx context.Context, network, address string) (net.Conn, error)
	listenPort int

	mu       sync.Mutex
	addr     string // current listen address, "" when stopped
	token    string // node token the current certificate derives from
	listener net.Listener
}

func newServingForwarder(log *slog.Logger, nodeToken func() string, port func(string) (int, bool)) *servingForwarder {
	return &servingForwarder{
		log: log, nodeToken: nodeToken, port: port,
		listen: net.Listen, dial: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		listenPort: proto.ServingForwardPort,
	}
}

// apply (re)starts the listener for the node's serving IP; an empty IP stops
// it. It is idempotent, so every state sync may call it.
func (f *servingForwarder) apply(servingIP string) {
	token := f.nodeToken()
	addr := ""
	if ip := strings.TrimSpace(servingIP); ip != "" && token != "" {
		addr = net.JoinHostPort(ip, strconv.Itoa(f.listenPort))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if addr == f.addr && token == f.token {
		return
	}
	if f.listener != nil {
		_ = f.listener.Close()
		f.listener, f.addr, f.token = nil, "", ""
	}
	if addr == "" {
		return
	}
	cert, err := proto.ServingForwardCertificate(token)
	if err != nil {
		f.log.Warn("serving forwarder certificate failed", "err", err)
		return
	}
	raw, err := f.listen("tcp", addr)
	if err != nil {
		f.log.Warn("serving forwarder cannot listen", "addr", addr, "err", err)
		return
	}
	ln := tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	f.listener, f.addr, f.token = ln, addr, token
	f.log.Info("serving forwarder listening", "addr", addr)
	go f.serve(ln, token)
}

func (f *servingForwarder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listener != nil {
		_ = f.listener.Close()
		f.listener, f.addr, f.token = nil, "", ""
	}
}

func (f *servingForwarder) serve(ln net.Listener, token string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				f.log.Warn("serving forwarder accept failed", "err", err)
			}
			return
		}
		go f.handle(conn, token)
	}
}

// handle authenticates one connection and splices it to the instance.
func (f *servingForwarder) handle(client net.Conn, token string) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReaderSize(client, 512)
	line, err := readPreamble(reader)
	if err != nil {
		_, _ = fmt.Fprintf(client, "ERR %s\n", err)
		return
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != proto.ServingForwardMagic {
		_, _ = io.WriteString(client, "ERR bad preamble\n")
		return
	}
	instanceUUID, presented := fields[1], fields[2]
	if !proto.ValidServingForwardToken(token, instanceUUID, presented) {
		_, _ = io.WriteString(client, "ERR rejected authentication\n")
		return
	}
	port, ok := f.port(instanceUUID)
	if !ok {
		_, _ = io.WriteString(client, "ERR instance is not running here\n")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	upstream, err := f.dial(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	cancel()
	if err != nil {
		_, _ = fmt.Fprintf(client, "ERR instance port: %v\n", err)
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(client, "OK\n"); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() {
		// Bytes the client sent right after the preamble sit in reader.
		_, _ = io.Copy(upstream, reader)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if tlsConn, ok := client.(*tls.Conn); ok {
			_ = tlsConn.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// readPreamble reads one bounded line so a client cannot make the agent buffer
// an unbounded header.
func readPreamble(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for b.Len() <= 256 {
		c, err := r.ReadByte()
		if err != nil {
			return "", errors.New("no preamble")
		}
		if c == '\n' {
			return strings.TrimSpace(b.String()), nil
		}
		b.WriteByte(c)
	}
	return "", errors.New("preamble too long")
}
