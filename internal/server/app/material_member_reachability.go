//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// dialMemberDatabase is the reachability probe; tests replace it to produce
// a timeout without a real network blackhole.
var dialMemberDatabase = func(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

// memberDatabasePartitioned marks a receiver that answered 503 while its
// database port timed out from this member. It cannot be in this member's
// quorum, so its refusal says nothing about these bytes: it contributes no
// copy, exactly like a peer whose HTTP dial failed.
type memberDatabasePartitioned struct{ err error }

func (e *memberDatabasePartitioned) Error() string { return e.err.Error() }
func (e *memberDatabasePartitioned) Unwrap() error { return e.err }

// explainMemberReachability appends a TCP result for the member's database
// address. A refused or timed-out database port stays on the original error,
// so an HTTP 404 is not reported as a network failure. Only a 503 from a
// receiver whose database port timed out is marked as a partitioned peer.
func explainMemberReachability(ctx context.Context, err error, databaseAddress string) error {
	if err == nil || databaseAddress == "" || ctx.Err() != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, dialErr := dialMemberDatabase(dialCtx, databaseAddress)
	if conn != nil {
		_ = conn.Close()
	}
	switch {
	case dialErr == nil:
		return fmt.Errorf("%w; database %s accepted a TCP connection", err, databaseAddress)
	case errors.Is(dialErr, syscall.ECONNREFUSED):
		return fmt.Errorf("%w; database %s connection refused", err, databaseAddress)
	default:
		var timeout net.Error
		if errors.As(dialErr, &timeout) && timeout.Timeout() {
			explained := fmt.Errorf("%w; database %s timed out", err, databaseAddress)
			var refusal *materialReceiverStatus
			if errors.As(err, &refusal) && refusal.status == http.StatusServiceUnavailable {
				return &memberDatabasePartitioned{err: explained}
			}
			return explained
		}
		return fmt.Errorf("%w; database %s is unreachable", err, databaseAddress)
	}
}
