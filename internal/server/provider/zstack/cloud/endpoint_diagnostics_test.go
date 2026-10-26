package cloud

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
)

func TestCloudFailedAPIUsesIndependentBoundedDiagnostics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := t.Context()
		apiCtx, cancel := context.WithTimeout(parent, time.Second)
		defer cancel()
		<-apiCtx.Done()
		apiErr := errors.Join(&apiclient.APIError{Phase: apiclient.PhaseConnect, Kind: apiclient.ErrorTimeout}, apiCtx.Err())
		d := &Driver{diagnostics: &endpointDiagnostics{
			timeout: 2 * time.Second,
			tcp: func(ctx context.Context, address string) error {
				if address != "[::1]:443" || ctx.Err() != nil {
					t.Error("diagnostic did not receive a fresh context or HTTPS IPv6 endpoint")
				}
				<-ctx.Done()
				return ctx.Err()
			},
			ip: func(ctx context.Context, host string) ipProbeResult {
				if host != "::1" || ctx.Err() != nil {
					t.Error("IP probe received wrong target/context")
				}
				<-ctx.Done()
				return ipNoReply
			},
		}}
		start := time.Now()
		err := d.validationResult(parent, "https://[::1]/zstack", apiErr)
		if got := validationErrorCode(t, err); got != "ZSTACK_NETWORK_UNCONFIRMED" {
			t.Fatal(got)
		}
		if time.Since(start) != 2*time.Second {
			t.Fatal("parallel probes exceeded their shared budget")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("lost original API timeout")
		}
	})
}

func TestCloudCancellationStopsDiagnostics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		d := &Driver{diagnostics: &endpointDiagnostics{
			timeout: time.Hour,
			tcp:     func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() },
			ip:      func(ctx context.Context, _ string) ipProbeResult { <-ctx.Done(); return ipProbeUnavailable },
		}}
		done := make(chan error, 1)
		go func() {
			done <- d.validationResult(ctx, "http://127.0.0.1:8080", &apiclient.APIError{Phase: apiclient.PhaseConnect})
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation became an IP failure: %v", err)
		}
	})
}

func TestCloudSuccessfulAndCanceledValidationNeverProbes(t *testing.T) {
	d := &Driver{diagnostics: &endpointDiagnostics{
		timeout: time.Second,
		tcp:     func(context.Context, string) error { t.Error("unexpected TCP probe"); return nil },
		ip:      func(context.Context, string) ipProbeResult { t.Error("unexpected IP probe"); return ipResponded },
	}}
	if err := d.validationResult(t.Context(), "http://127.0.0.1", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.validationResult(ctx, "http://127.0.0.1", &apiclient.APIError{Phase: apiclient.PhaseConnect}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCloudRecoveredTCPDoesNotValidateCredentials(t *testing.T) {
	d := &Driver{diagnostics: &endpointDiagnostics{
		timeout: time.Second,
		tcp: func(_ context.Context, address string) error {
			if address != "127.0.0.1:80" {
				t.Error(address)
			}
			return nil
		},
		ip: func(context.Context, string) ipProbeResult { return ipNoReply },
	}}
	err := d.validationResult(t.Context(), "http://127.0.0.1/zstack", &apiclient.APIError{Phase: apiclient.PhaseConnect})
	if got := validationErrorCode(t, err); got != "ZSTACK_CONNECTION_RECOVERED" {
		t.Fatal(got)
	}
}

func TestCloudTCPProbeClosesConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			closed <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err = conn.Read(make([]byte, 1))
		closed <- err
	}()
	if err := probeTCP(t.Context(), listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if err := <-closed; !errors.Is(err, io.EOF) {
		t.Fatalf("probe leaked its socket: %v", err)
	}
}

func TestCloudIPProbeDistinguishesNoReplyFromUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         ipProbeResult
	}{
		{"reply", "#!/bin/sh\nexit 0\n", ipResponded},
		{"no executable", "", ipProbeUnavailable},
		{"permission or invalid invocation", "#!/bin/sh\nexit 2\n", ipProbeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(dir, "ping"), []byte(tc.script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if got := probeIP(t.Context(), "127.0.0.1"); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	if runtime.GOOS == "linux" {
		dir := t.TempDir()
		t.Setenv("PATH", dir)
		if err := os.WriteFile(filepath.Join(dir, "ping"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if got := probeIP(t.Context(), "127.0.0.1"); got != ipNoReply {
			t.Fatal(got)
		}
	}
}

func TestCloudIPProbeDeadlineDoesNotBecomeIPUnreachable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "ping"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if got := probeIP(ctx, "127.0.0.1"); got != ipNoReply {
		t.Fatalf("probe timeout = %s", got)
	}
	expired, stop := context.WithCancel(t.Context())
	stop()
	if got := probeIP(expired, "127.0.0.1"); got != ipProbeUnavailable {
		t.Fatalf("unstarted probe = %s", got)
	}
}
