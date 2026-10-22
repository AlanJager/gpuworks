package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestResume200FallsBackToFullRequest(t *testing.T) {
	for _, suffix := range []bool{true, false} {
		t.Run(fmt.Sprintf("suffix=%v", suffix), func(t *testing.T) {
			data := pattern(1671853)
			const offset = 904709
			var mu sync.Mutex
			var ranges []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				rg := r.Header.Get("Range")
				mu.Lock()
				ranges = append(ranges, rg)
				mu.Unlock()
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				w.WriteHeader(http.StatusOK)
				// Faithful live response: 200, full Content-Length, no Content-Range,
				// but only the requested suffix is sent. Also retain lawful ignore-Range.
				if suffix && rg == "bytes=904709-" {
					_, _ = w.Write(data[offset:])
					return
				}
				_, _ = w.Write(data)
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "merges.txt")
			if err := os.WriteFile(dest+".part", data[:offset], 0600); err != nil {
				t.Fatal(err)
			}
			if err := Download(context.Background(), Options{URL: srv.URL, Dest: dest, Concurrency: 4, Headers: map[string]string{"Authorization": "Bearer fixture", "rAnGe": "bytes=1-"}}, nil); err != nil {
				t.Fatalf("Download: %v", err)
			}
			got, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(data) || sum(got) != sum(data) {
				t.Fatalf("full content mismatch: %d bytes", len(got))
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(ranges, []string{"bytes=0-0", "bytes=904709-", ""}) {
				t.Fatalf("requests=%q", ranges)
			}
			assertCleanedUp(t, dest)
		})
	}
}

func TestResume200FullFallbackRejectsBadResponse(t *testing.T) {
	for _, mode := range []string{"partial-status", "content-range", "short-body", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			data := pattern(8192)
			dest := filepath.Join(t.TempDir(), "model.bin")
			seed := data[:2048]
			if err := os.WriteFile(dest+".part", seed, 0600); err != nil {
				t.Fatal(err)
			}
			var count int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				if r.Header.Get("Range") != "" {
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					_, _ = w.Write(data)
					return
				}
				switch mode {
				case "forbidden":
					w.WriteHeader(http.StatusForbidden)
				case "partial-status":
					w.Header().Set("Content-Range", "bytes 2048-8191/8192")
					w.WriteHeader(206)
					_, _ = w.Write(data[2048:])
				case "content-range":
					w.Header().Set("Content-Range", "bytes 2048-8191/8192")
					_, _ = w.Write(data[2048:])
				case "short-body":
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					_, _ = w.Write(data[2048:])
				}
			}))
			defer srv.Close()
			err := Download(context.Background(), Options{URL: srv.URL, Dest: dest, Concurrency: 1}, nil)
			if err == nil {
				t.Fatal("invalid fallback accepted")
			}
			if mode == "short-body" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("err=%v", err)
			}
			if mode == "forbidden" {
				var status *StatusError
				if !errors.As(err, &status) || status.Code != http.StatusForbidden {
					t.Fatalf("lost permanent HTTP status: %v", err)
				}
			}
			if _, e := os.Stat(dest); !os.IsNotExist(e) {
				t.Fatalf("invalid response finalized: %v", e)
			}
			if mode != "short-body" {
				got, e := os.ReadFile(dest + ".part")
				if e != nil || !bytes.Equal(got, seed) {
					t.Fatal("rejected response modified seed")
				}
			}
			if count != 3 {
				t.Fatalf("requests=%d, want bounded3", count)
			}
		})
	}
}

func TestResume200FallbackCancellationPreservesPart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requested := make(chan struct{})
	ended := make(chan struct{})
	data := pattern(8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			_, _ = w.Write(data)
			return
		}
		close(requested)
		<-r.Context().Done()
		close(ended)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "model.bin")
	seed := data[:2048]
	if err := os.WriteFile(dest+".part", seed, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Download(ctx, Options{URL: srv.URL, Dest: dest}, nil) }()
	<-requested
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	<-ended
	got, err := os.ReadFile(dest + ".part")
	if err != nil || !bytes.Equal(got, seed) {
		t.Fatal("cancelled fallback changed old part")
	}
}
