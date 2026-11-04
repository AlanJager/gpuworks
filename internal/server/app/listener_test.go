package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunBindsEphemeralAddressAndStops(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{
		Addr: "127.0.0.1:0", DataDir: dir,
		DBPath:    filepath.Join(dir, "gpuworks.db"),
		AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var addr string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		addr = a.Addr()
		if strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasSuffix(addr, ":0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" || strings.HasSuffix(addr, ":0") {
		t.Fatalf("ephemeral listener did not publish concrete address: %q", addr)
	}
	resp, err := http.Get("http://" + addr + "/api/v1/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("healthz status = %d, want unauthorized without JWT", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
}
