package outbound

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

// SOCKS receives a numeric address from the checked DNS snapshot. Keeping the
// original request URL lets the shared HTTP transport retain Host, TLS SNI,
// connection reuse, and fallback across all checked destination addresses.
func dialSOCKSTunnel(ctx context.Context, proxy *url.URL, target string) (net.Conn, error) {
	port := proxy.Port()
	if port == "" {
		port = "1080"
	}
	conn, err := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), port))
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := socksHandshake(conn, proxy, target); err != nil {
		return nil, err
	}
	if !stop() && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	success = true
	return conn, nil
}

func socksHandshake(conn net.Conn, proxy *url.URL, target string) error {
	greeting := []byte{5, 1, 0}
	if proxy.User != nil {
		greeting = []byte{5, 2, 0, 2}
	}
	if _, err := conn.Write(greeting); err != nil {
		return err
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 5 || (reply[1] != 0 && (reply[1] != 2 || proxy.User == nil)) {
		return fmt.Errorf("SOCKS authentication method refused")
	}
	if reply[1] == 2 {
		username := proxy.User.Username()
		password, _ := proxy.User.Password()
		if len(username) > 255 || len(password) > 255 {
			return fmt.Errorf("SOCKS credentials too long")
		}
		auth := append([]byte{1, byte(len(username))}, username...)
		auth = append(append(auth, byte(len(password))), password...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply[:]); err != nil {
			return err
		}
		if reply[0] != 1 || reply[1] != 0 {
			return fmt.Errorf("SOCKS authentication refused")
		}
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return err
	}
	atyp := byte(4)
	if ip.Is4() {
		atyp = 1
	}
	request := append([]byte{5, 1, 0, atyp}, ip.AsSlice()...)
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return err
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	if header[0] != 5 || header[1] != 0 || header[2] != 0 {
		return fmt.Errorf("SOCKS tunnel refused")
	}
	length := 0
	switch header[3] {
	case 1:
		length = 4
	case 4:
		length = 16
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return err
		}
		length = int(size[0])
	default:
		return fmt.Errorf("invalid SOCKS bound address")
	}
	_, err = io.CopyN(io.Discard, conn, int64(length+2))
	return err
}
