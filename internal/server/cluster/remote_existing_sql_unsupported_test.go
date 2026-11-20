//go:build !linux || !dqlite

package cluster

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestRemoteExistingReadSQLUnavailableReturnsNoResources(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "live context"
		if canceled {
			name = "canceled context"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				cancel()
			}
			calls := 0
			pool, cleanup, err := OpenRemoteExistingReadSQL(ctx, Member{ID: 1, Address: "127.0.0.1:9001", Role: "voter"}, "cluster.db", func(context.Context, string) (net.Conn, error) {
				calls++
				return nil, errors.New("unavailable backend reached network")
			})
			if !errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) != canceled || pool != nil || cleanup != nil || calls != 0 {
				t.Fatalf("unavailable backend escaped: pool=%v cleanup=%t err=%v dials=%d", pool, cleanup != nil, err, calls)
			}
		})
	}
}
