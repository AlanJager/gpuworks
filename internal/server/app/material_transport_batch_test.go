//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMaterialBatchFramesRejectIncompleteAndChangedBytes(t *testing.T) {
	root := t.TempDir()
	receiver, err := newMaterialReceiver(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	contents := [][]byte{[]byte("first canonical replica"), []byte("second canonical replica")}
	items := make([]materialBatchItem, 0, len(contents))
	for _, content := range contents {
		digest := materialTestDigest(content)
		if err := os.MkdirAll(filepath.Join(root, digest[:2]), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, digest[:2], digest), content, 0600); err != nil {
			t.Fatal(err)
		}
		items = append(items, materialBatchItem{Digest: digest, Bytes: int64(len(content))})
	}
	raw, err := json.Marshal(materialBatchRequest{Files: items})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, materialBatchPath, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	serveMaterialBatch(response, request, receiver)
	if response.Code != http.StatusOK || response.Header().Get("X-Gpuworks-Material-Member") != "7" {
		t.Fatalf("batch response: code=%d member=%q", response.Code, response.Header().Get("X-Gpuworks-Material-Member"))
	}
	framed := response.Body.Bytes()
	if response.Header().Get("Content-Length") != strconv.Itoa(len(framed)) {
		t.Fatal("batch response length disagrees with its frames")
	}
	if err := verifyMaterialBatchBody(t.Context(), bytes.NewReader(framed), items); err != nil {
		t.Fatalf("canonical batch rejected: %v", err)
	}
	tests := map[string][]byte{
		"missing":      framed[:len(framed)-1],
		"extra":        append(bytes.Clone(framed), 1),
		"corrupt":      bytes.Clone(framed),
		"wrong digest": bytes.Clone(framed),
		"wrong size":   bytes.Clone(framed),
	}
	firstBody := len(materialBatchMagic) + 2 + 64 + 8
	tests["corrupt"][firstBody] ^= 1
	tests["wrong digest"][len(materialBatchMagic)+2] ^= 1
	tests["wrong size"][len(materialBatchMagic)+2+64+7] ^= 1
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if err := verifyMaterialBatchBody(t.Context(), bytes.NewReader(body), items); err == nil {
				t.Fatal("invalid batch bytes were accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := verifyMaterialBatchBody(ctx, bytes.NewReader(framed), items); err != context.Canceled {
		t.Fatalf("canceled batch returned %v", err)
	}
	bad := materialBatchRequest{Files: append(items, items[0])}
	raw, _ = json.Marshal(bad)
	request = httptest.NewRequest(http.MethodPost, materialBatchPath, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	serveMaterialBatch(response, request, receiver)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate request returned HTTP %d", response.Code)
	}
	missing := []materialBatchItem{{Digest: strings.Repeat("a", 64), Bytes: 1}}
	raw, _ = json.Marshal(materialBatchRequest{Files: missing})
	request = httptest.NewRequest(http.MethodPost, materialBatchPath, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	serveMaterialBatch(response, request, receiver)
	if response.Code != http.StatusNotFound || response.Body.Len() == len(framed) {
		t.Fatalf("missing source returned HTTP %d", response.Code)
	}
}

func TestMaterialBatchLimitsRejectDuplicatesCountAndBytes(t *testing.T) {
	files := []materialBatchItem{{Digest: materialTestDigest([]byte("one")), Bytes: 1}}
	if _, err := materialBatchLength(files); err != nil {
		t.Fatal(err)
	}
	if _, err := materialBatchLength([]materialBatchItem{files[0], files[0]}); err == nil {
		t.Fatal("duplicate digest accepted")
	}
	files[0].Bytes = maxMaterialBatchBytes + 1
	if _, err := materialBatchLength(files); err == nil {
		t.Fatal("oversized object accepted")
	}
	files = files[:0]
	for i := range maxMaterialBatchCount + 1 {
		files = append(files, materialBatchItem{Digest: materialTestDigest([]byte(strconv.Itoa(i))), Bytes: 1})
	}
	if _, err := materialBatchLength(files); err == nil {
		t.Fatal("oversized item count accepted")
	}
	files = files[:2]
	files[0].Bytes = maxMaterialBatchBytes
	if _, err := materialBatchLength(files); err == nil {
		t.Fatal("oversized aggregate accepted")
	}
}
