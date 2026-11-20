//go:build linux && dqlite

package app

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestMaterialProgressConnRenewsDeadlineAndStopsStalledStream(t *testing.T) {
	reader, writer := net.Pipe()
	defer reader.Close()
	defer writer.Close()
	bounded := &materialProgressConn{Conn: reader, idle: 500 * time.Millisecond}
	go func() {
		for range 3 {
			time.Sleep(250 * time.Millisecond)
			if _, err := writer.Write([]byte("x")); err != nil {
				return
			}
		}
	}()
	var byteBuf [1]byte
	for range 3 {
		if _, err := bounded.Read(byteBuf[:]); err != nil || byteBuf[0] != 'x' {
			t.Fatalf("stream with progress was interrupted: byte=%q err=%v", byteBuf, err)
		}
	}
	if _, err := bounded.Read(byteBuf[:]); err == nil || !isMaterialTimeout(err) {
		t.Fatalf("stalled read did not time out: %v", err)
	}
	writeSide, idlePeer := net.Pipe()
	defer writeSide.Close()
	defer idlePeer.Close()
	if _, err := (&materialProgressConn{Conn: writeSide, idle: 30 * time.Millisecond}).Write([]byte("x")); err == nil || !isMaterialTimeout(err) {
		t.Fatalf("stalled write did not time out: %v", err)
	}
}

func isMaterialTimeout(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}
