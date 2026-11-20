package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/pkg/catalogasset"
)

func catalogMember(t *testing.T, f *fixture) ([]byte, []byte) {
	t.Helper()
	key, err := identity.NewMemberKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := f.issuer.IssueMember(csr, fp, "127.0.0.1:9910", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func TestCatalogIndexClientRejectsIncompleteOrInvalidResponses(t *testing.T) {
	for _, name := range []string{"valid", "empty", "cluster", "protocol", "null", "unsorted", "duplicate", "uppercase", "path", "unknown", "trailing", "oversize", "redirect", "remote error", "canceled", "truncated"} {
		t.Run(name, func(t *testing.T) {
			var payload []byte
			f := endpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != CatalogPackagesPath || r.ContentLength != 0 || r.Header.Get("Authorization") != "" {
					t.Error("unexpected catalog index request")
				}
				if name == "redirect" {
					w.Header().Set("Location", "https://example.invalid/private")
					w.WriteHeader(307)
					return
				}
				if name == "remote error" {
					w.WriteHeader(403)
					_, _ = w.Write([]byte("private-response-marker"))
					return
				}
				if name == "truncated" {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)+10))
				}
				_, _ = w.Write(payload)
			}))
			result := CatalogPackageIndex{ClusterID: f.cluster, Protocol: proto.ClusterProtocolVersion, Digests: []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}}
			switch name {
			case "empty":
				result.Digests = []string{}
			case "cluster":
				result.ClusterID = strings.Repeat("b", 32)
			case "protocol":
				result.Protocol++
			case "null":
				result.Digests = nil
			case "unsorted":
				result.Digests[0], result.Digests[1] = result.Digests[1], result.Digests[0]
			case "duplicate":
				result.Digests[1] = result.Digests[0]
			case "uppercase":
				result.Digests[0] = strings.Repeat("A", 64)
			case "path":
				result.Digests[0] = "../secret"
			}
			payload, _ = json.Marshal(result)
			switch name {
			case "unknown":
				payload = append(payload[:len(payload)-1], []byte(`,"extra":true}`)...)
			case "trailing":
				payload = append(payload, []byte(" {}")...)
			case "oversize":
				payload = []byte(strings.Repeat("x", (1<<20)+1))
			}
			cert, key := catalogMember(t, f)
			ctx := t.Context()
			if name == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := f.client(t).FetchCatalogIndex(ctx, cert, key)
			if name == "valid" || name == "empty" {
				if err != nil || !reflect.DeepEqual(got, result) {
					t.Fatal("valid index rejected", err)
				}
				return
			}
			if err == nil || !reflect.DeepEqual(got, CatalogPackageIndex{}) {
				t.Fatal("invalid response returned partial index")
			}
			if strings.Contains(err.Error(), "private-response-marker") {
				t.Fatal("remote error leaked")
			}
		})
	}
}

func TestCatalogPackageClientChecksWireAndCanonicalDigests(t *testing.T) {
	archive, err := seed.Archive()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	canonicalDigest := hex.EncodeToString(sum[:])
	for _, name := range []string{"valid", "wrong digest", "trailing", "malformed", "oversize", "truncated", "redirect", "remote error", "canceled", "invalid digest", "wrong key"} {
		t.Run(name, func(t *testing.T) {
			payload := archive
			digest := canonicalDigest
			switch name {
			case "wrong digest":
				digest = strings.Repeat("a", 64)
			case "trailing":
				payload = append(append([]byte{}, archive...), []byte("trailing")...)
			case "malformed":
				payload = []byte("invalid gzip")
			case "oversize":
				payload = []byte(strings.Repeat("x", int(catalogasset.MaxPackageBytes)+1))
			case "invalid digest":
				digest = "../secret"
			}
			if name == "trailing" || name == "malformed" || name == "oversize" {
				sum := sha256.Sum256(payload)
				digest = hex.EncodeToString(sum[:])
			}
			f := endpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if name == "invalid digest" || name == "wrong key" || name == "canceled" {
					t.Error("invalid input sent request")
				}
				if r.Method != http.MethodPost || r.URL.Path != CatalogPackagesPath+"/"+digest || r.ContentLength != 0 {
					t.Error("unexpected package request")
				}
				if name == "redirect" {
					w.Header().Set("Location", "https://example.invalid/private")
					w.WriteHeader(307)
					return
				}
				if name == "remote error" {
					w.WriteHeader(403)
					_, _ = w.Write([]byte("private-response-marker"))
					return
				}
				if name == "truncated" {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)+10))
				}
				_, _ = w.Write(payload)
			}))
			cert, key := catalogMember(t, f)
			if name == "wrong key" {
				key, err = identity.NewMemberKey()
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			if name == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := f.client(t).FetchCatalogPackage(ctx, cert, key, digest)
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				actual, err := inferencecatalog.PackageDigest(got.FS)
				if err != nil || actual != digest {
					t.Fatal("wrong package", err)
				}
				return
			}
			if err == nil || !reflect.DeepEqual(got, inferencecatalog.Archive{}) {
				t.Fatal("invalid response returned archive")
			}
			if strings.Contains(err.Error(), "private-response-marker") {
				t.Fatal("remote response leaked")
			}
		})
	}
}

func TestCatalogClientCancellationDuringResponseReturnsNoPartialData(t *testing.T) {
	for _, index := range []bool{true, false} {
		t.Run(fmt.Sprint("index=", index), func(t *testing.T) {
			started := make(chan struct{})
			f := endpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "10000")
				_, _ = w.Write([]byte("{"))
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			}))
			cert, key := catalogMember(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				select {
				case <-started:
					cancel()
				case <-ctx.Done():
				}
			}()
			client := f.client(t)
			if index {
				got, err := client.FetchCatalogIndex(ctx, cert, key)
				if err == nil || !reflect.DeepEqual(got, CatalogPackageIndex{}) {
					t.Fatal("cancellation returned index")
				}
			} else {
				got, err := client.FetchCatalogPackage(ctx, cert, key, strings.Repeat("a", 64))
				if err == nil || !reflect.DeepEqual(got, inferencecatalog.Archive{}) {
					t.Fatal("cancellation returned archive")
				}
			}
			<-done
		})
	}
}

// A negotiated test protocol supplies a complete HTTP response whose final EOF
// cancels the operation. This hits the boundary after successful body reading,
// without races between a timer and CPU-only archive validation.
type catalogResponseTransport struct{ body io.ReadCloser }

func (r catalogResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: r.body}, nil
}

type catalogCancelAtEOF struct {
	*bytes.Reader
	cancel context.CancelFunc
}

func (r catalogCancelAtEOF) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		r.cancel()
	}
	return n, err
}
func (r catalogCancelAtEOF) Close() error { return nil }

func TestCatalogClientCancellationAfterCompleteResponseReturnsZero(t *testing.T) {
	for _, index := range []bool{true, false} {
		t.Run(fmt.Sprint("index=", index), func(t *testing.T) {
			f := endpoint(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("test protocol unexpectedly used HTTP handler") }))
			const protocol = "catalog-test"
			f.server.TLS.NextProtos = []string{protocol}
			cert, key := catalogMember(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var data []byte
			var digest string
			if index {
				data, _ = json.Marshal(CatalogPackageIndex{ClusterID: f.cluster, Protocol: proto.ClusterProtocolVersion, Digests: []string{}})
			} else {
				var err error
				data, err = seed.Archive()
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(data)
				digest = hex.EncodeToString(sum[:])
			}
			client := f.client(t)
			transport := client.http.Transport.(*http.Transport)
			transport.TLSClientConfig.NextProtos = []string{protocol}
			transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{
				protocol: func(_ string, connection *tls.Conn) http.RoundTripper {
					t.Cleanup(func() { _ = connection.Close() })
					return catalogResponseTransport{body: catalogCancelAtEOF{Reader: bytes.NewReader(data), cancel: cancel}}
				},
			}
			if index {
				got, err := client.FetchCatalogIndex(ctx, cert, key)
				if err != context.Canceled || !reflect.DeepEqual(got, CatalogPackageIndex{}) {
					t.Fatal("post-read cancellation returned index", err)
				}
			} else {
				got, err := client.FetchCatalogPackage(ctx, cert, key, digest)
				if err != context.Canceled || !reflect.DeepEqual(got, inferencecatalog.Archive{}) {
					t.Fatal("post-read cancellation returned archive", err)
				}
			}
			if ctx.Err() != context.Canceled {
				t.Fatal("response did not reach EOF cancellation boundary")
			}
		})
	}
}
