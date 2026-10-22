package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"

	"gpuworks.ai/gpuworks/internal/agent/download"
)

func main() {
	data := make([]byte, 4<<20)
	for i := range data {
		data[i] = byte(i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		_, _ = w.Write(data)
	}))
	defer server.Close()

	var latest atomic.Int64
	err := download.Download(context.Background(), download.Options{
		URL: server.URL, Dest: "/models/enospc.bin", Concurrency: 1,
	}, func(progress download.Progress) { latest.Store(progress.Done) })
	if err == nil {
		fmt.Fprintln(os.Stderr, "download unexpectedly fit in 1MiB tmpfs")
		os.Exit(1)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "no space left on device") {
		fmt.Fprintf(os.Stderr, "download error did not preserve ENOSPC: %v\n", err)
		os.Exit(2)
	}
	if latest.Load() == 0 {
		fmt.Fprintln(os.Stderr, "download reported zero bytes after partial ENOSPC write")
		os.Exit(3)
	}
	fmt.Printf("enospc-harness-ok bytes_done=%d error=%v\n", latest.Load(), err)
}
