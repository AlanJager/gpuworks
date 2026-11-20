//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type materialReadbackRoundTrip func(*http.Request) (*http.Response, error)

func (fn materialReadbackRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

type materialReadbackReader func([]byte) (int, error)

func (fn materialReadbackReader) Read(p []byte) (int, error) { return fn(p) }

func materialReadbackFrame(item materialBatchItem, body []byte) []byte {
	var frame bytes.Buffer
	frame.Write(materialBatchMagic)
	_ = binary.Write(&frame, binary.BigEndian, uint16(1))
	frame.WriteString(item.Digest)
	_ = binary.Write(&frame, binary.BigEndian, uint64(item.Bytes))
	frame.Write(body)
	return frame.Bytes()
}

func TestMaterialReadbackTLSBodyCancellationPreservesCause(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			body := []byte(strings.Repeat("canonical bytes", 16))
			item := f.put(body)
			wire := body
			if batch {
				wire = materialReadbackFrame(item, body)
			}
			prefix := len(wire) - len(body) + 1
			f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
				w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
				w.Header().Set("X-Gpuworks-Material-Member", "7")
				w.Header().Set("Cache-Control", "no-store")
				if batch {
					w.Header().Set("Content-Type", materialBatchType)
				}
				_, _ = w.Write(wire[:prefix])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return true
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// Delegate to the real pinned mTLS transport. The read hook cancels
			// only after the verifier has consumed framing and a payload byte,
			// then returns the actual HTTP body's cancellation read error.
			original := f.client.Transport.(*http.Transport)
			transport := original.Clone()
			transport.RegisterProtocol("https", materialReadbackRoundTrip(func(r *http.Request) (*http.Response, error) {
				response, err := original.RoundTrip(r)
				if err != nil {
					return nil, err
				}
				source := response.Body
				t.Cleanup(func() { _ = source.Close() })
				remaining := prefix
				response.Body = io.NopCloser(materialReadbackReader(func(p []byte) (int, error) {
					if remaining == 0 {
						cancel()
					} else if len(p) > remaining {
						p = p[:remaining]
					}
					n, readErr := source.Read(p)
					remaining -= n
					return n, readErr
				}))
				return response, nil
			}))
			client := *f.client
			client.Transport = transport
			var err error
			if batch {
				err = verifyRemoteMaterialBatch(ctx, &client, f.server.URL, []materialBatchItem{item}, 7)
			} else {
				err = verifyRemoteMaterialReplica(ctx, &client, f.server.URL, item.Digest, item.Bytes, 7)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("partial TLS payload lost cancellation cause: %v", err)
			}
		})
	}
}

func TestMaterialReadbackTLSWrongAndTruncatedBytesRemainFailures(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, truncated := range []bool{false, true} {
			name := "single"
			if batch {
				name = "batch"
			}
			if truncated {
				name += "/truncated"
			} else {
				name += "/wrong-bytes"
			}
			t.Run(name, func(t *testing.T) {
				f := newCandidateRemoteSetFixture(t, 7)
				body := []byte("canonical payload")
				item := f.put(body)
				wire := bytes.Clone(body)
				if batch {
					wire = materialReadbackFrame(item, body)
				}
				length := len(wire)
				if truncated {
					wire = wire[:length-1]
				} else {
					wire[length-1] ^= 1
				}
				f.beforeServe = func(w http.ResponseWriter, _ *http.Request) bool {
					w.Header().Set("Content-Length", strconv.Itoa(length))
					w.Header().Set("X-Gpuworks-Material-Member", "7")
					w.Header().Set("Cache-Control", "no-store")
					if batch {
						w.Header().Set("Content-Type", materialBatchType)
					}
					_, _ = w.Write(wire)
					return true
				}
				var err error
				if batch {
					err = verifyRemoteMaterialBatch(t.Context(), f.client, f.server.URL, []materialBatchItem{item}, 7)
				} else {
					err = verifyRemoteMaterialReplica(t.Context(), f.client, f.server.URL, item.Digest, item.Bytes, 7)
				}
				if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("invalid bytes were accepted or misclassified as canceled: %v", err)
				}
				if truncated && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("truncated TLS body lost its transport error: %v", err)
				}
			})
		}
	}
}

func TestMaterialReadbackBatchPreservesActualReadErrorWhenContextAlsoCancels(t *testing.T) {
	body := []byte("canonical payload")
	item := materialBatchItem{Digest: materialTestDigest(body), Bytes: int64(len(body))}
	wire := materialReadbackFrame(item, body)
	for stage, offset := range map[string]int{
		"framing": 1,
		"count":   len(materialBatchMagic) + 1,
		"digest":  len(materialBatchMagic) + 2 + 1,
		"length":  len(materialBatchMagic) + 2 + 64 + 1,
		"payload": len(wire) - len(body) + 1,
		"end":     len(wire),
	} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			readFailure := errors.New("actual fixture read failure")
			reader := io.MultiReader(bytes.NewReader(wire[:offset]), materialReadbackReader(func([]byte) (int, error) {
				cancel()
				return 0, readFailure
			}))
			err := verifyMaterialBatchBody(ctx, reader, []materialBatchItem{item})
			if !errors.Is(err, readFailure) || errors.Is(err, context.Canceled) {
				t.Fatalf("context replaced the actual %s read error: %v", stage, err)
			}
		})
	}
}

func TestMaterialReadbackBatchIntegrityFailureSurvivesConcurrentCancel(t *testing.T) {
	body := []byte("canonical payload")
	item := materialBatchItem{Digest: materialTestDigest(body), Bytes: int64(len(body))}
	wire := materialReadbackFrame(item, body)
	for stage, end := range map[string]int{
		"framing": len(materialBatchMagic),
		"count":   len(materialBatchMagic) + 2,
		"digest":  len(materialBatchMagic) + 2 + 64,
		"length":  len(materialBatchMagic) + 2 + 64 + 8,
		"payload": len(wire),
	} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wrong := bytes.Clone(wire[:end])
			wrong[end-1] ^= 1
			reader := bytes.NewReader(wrong)
			err := verifyMaterialBatchBody(ctx, materialReadbackReader(func(p []byte) (int, error) {
				n, err := reader.Read(p)
				if n > 0 && reader.Len() == 0 {
					cancel()
				}
				return n, err
			}), []materialBatchItem{item})
			if err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("%s integrity failure became cancellation: %v", stage, err)
			}
		})
	}
}
