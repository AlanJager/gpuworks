//go:build linux && dqlite

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Pace the real receiver and read route rather than substituting a successful
// response. Each direction lasts longer than the client's socket idle limit.
func assertLargeMaterialStreamProgress(t *testing.T, sender, target *enrollmentBundle, st *store.Store, targetKey string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	handler := enrollmentMaterialHandlerWithReceiver(target, st, http.NotFoundHandler(), nil, receiver)
	var posts atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pace := materialStreamPacer{ctx: r.Context()}
		switch r.Method {
		case http.MethodPost:
			posts.Add(1)
			r.Body = &pacedMaterialBody{ReadCloser: r.Body, pace: pace}
		case http.MethodGet:
			w = &pacedMaterialResponse{ResponseWriter: w, pace: pace}
		}
		handler.ServeHTTP(w, r)
	}))
	server.TLS = target.tls.Clone()
	server.StartTLS()
	defer server.Close()
	client, err := memberMaterialClient(sender, server.URL, targetKey)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	source := filepath.Join(t.TempDir(), "large-material")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	block := make([]byte, 256<<10)
	for i := range block {
		block[i] = byte(i)
	}
	const size = 16 << 20
	for range size / len(block) {
		if _, err := io.MultiWriter(file, hash).Write(block); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	started := time.Now()
	ack, err := sendMaterialReplica(ctx, client, server.URL, source, digest, 7)
	uploadTime := time.Since(started)
	if err != nil || ack.Digest != digest || ack.Bytes != size || ack.MemberID != 7 {
		t.Fatalf("paced large upload failed after %s: receipt=%+v err=%v", uploadTime, ack, err)
	}
	var local store.MaterialReplica
	if err := st.DB().WithContext(ctx).Where("digest = ? AND member_id = ?", digest, int64(7)).Take(&local).Error; err != nil || local.Path != filepath.Join(root, digest[:2], digest) {
		t.Fatalf("large upload did not commit its canonical local index: %+v err=%v", local, err)
	}
	started = time.Now()
	if err := verifyRemoteMaterialReplica(ctx, client, server.URL, digest, size, 7); err != nil {
		t.Fatalf("paced large authenticated readback failed: %v", err)
	}
	readTime := time.Since(started)
	if uploadTime <= 15*time.Second || readTime <= 15*time.Second {
		t.Fatalf("large stream did not cross socket idle limit: upload=%s read=%s", uploadTime, readTime)
	}
	if err := st.RecordMaterialRemoteReceipt(ctx, digest, 7, size); err != nil {
		t.Fatal(err)
	}
	transferred, err := ensureRemoteMaterialReplica(ctx, st, client, server.URL, source, digest, size, 7, func() { t.Error("fresh large replica unexpectedly reuploaded") })
	if err != nil || transferred || posts.Load() != 1 {
		t.Fatalf("stable large replica reuploaded: transferred=%t POST=%d err=%v", transferred, posts.Load(), err)
	}
	t.Logf("paced member TLS stream: bytes=%d upload=%s (%.2f MiB/s) read=%s (%.2f MiB/s) stable POST=0", size, uploadTime, 16/uploadTime.Seconds(), readTime, 16/readTime.Seconds())
}

type materialStreamPacer struct {
	ctx   context.Context
	bytes int
}

func (pace *materialStreamPacer) advance(n int) error {
	pace.bytes += n
	if pace.bytes < 256<<10 {
		return nil
	}
	pace.bytes -= 256 << 10
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-pace.ctx.Done():
		return pace.ctx.Err()
	}
}

type pacedMaterialBody struct {
	io.ReadCloser
	pace materialStreamPacer
}

func (body *pacedMaterialBody) Read(p []byte) (int, error) {
	if len(p) > 256<<10 {
		p = p[:256<<10]
	}
	n, err := body.ReadCloser.Read(p)
	if paceErr := body.pace.advance(n); paceErr != nil {
		return n, paceErr
	}
	return n, err
}

type pacedMaterialResponse struct {
	http.ResponseWriter
	pace materialStreamPacer
}

func (response *pacedMaterialResponse) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		next := min(len(p), 256<<10)
		n, err := response.ResponseWriter.Write(p[:next])
		written += n
		if err != nil {
			return written, err
		}
		response.ResponseWriter.(http.Flusher).Flush()
		if err := response.pace.advance(n); err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}
