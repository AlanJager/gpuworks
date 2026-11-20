//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNativeMaterialReceiptHintsPreserveFreshGrantsAndRepair(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	var mu sync.Mutex
	var mode string
	var batches []int
	var heads, uploads, gets int
	var stop context.CancelFunc
	var f *candidateGrantFixture
	f = newReplicationInvalidationFixture(t, ctx, func(index int, w http.ResponseWriter, r *http.Request) bool {
		if index != 1 {
			return false
		}
		mu.Lock()
		current := mode
		if r.Method == http.MethodHead {
			heads++
		}
		if r.Method == http.MethodGet {
			gets++
		}
		if r.Method == http.MethodPost && r.URL.Path != materialBatchPath {
			uploads++
		}
		if r.URL.Path != materialBatchPath {
			mu.Unlock()
			return false
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			mu.Unlock()
			t.Error(err)
			return true
		}
		var request materialBatchRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			mu.Unlock()
			t.Error(err)
			return true
		}
		batches = append(batches, len(request.Files))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		mu.Unlock()
		if current == "cancel" {
			stop()
			<-r.Context().Done()
			return true
		}
		if current == "deadline" {
			<-r.Context().Done()
			return true
		}
		if current == "unavailable" {
			http.Error(w, "not serving", 503)
			return true
		}
		if current == "membership" || current == "partial" || current == "badmeta" {
			recorded := httptest.NewRecorder()
			enrollmentMaterialHandlerWithReceiver(f.bundles[1], f.stores[1], http.NotFoundHandler(), nil, f.receivers[1]).ServeHTTP(recorded, r)
			if recorded.Code != 200 {
				t.Errorf("actual batch status=%d", recorded.Code)
			}
			body := bytes.Clone(recorded.Body.Bytes())
			if current == "partial" {
				body = body[:len(body)-1]
			}
			if current == "membership" {
				expected := store.ClusterIdentity{ClusterID: f.bundles[0].info.ClusterID, CAFingerprint: f.bundles[0].info.CAFingerprint, Protocol: store.ClusterProtocolVersion}
				if err := f.stores[0].SetClusterEnrollmentOriginForVerifiedKey(ctx, expected, f.keys[1], "https://127.0.0.1:19099"); err != nil {
					t.Error(err)
				}
			}
			for key, values := range recorded.Header() {
				w.Header()[key] = values
			}
			if current == "badmeta" {
				w.Header().Set("X-Gpuworks-Material-Member", "1")
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(body)
			return true
		}
		return false
	})
	expected := store.ClusterIdentity{ClusterID: f.bundles[0].info.ClusterID, CAFingerprint: f.bundles[0].info.CAFingerprint, Protocol: store.ClusterProtocolVersion}
	client, err := memberMaterialClient(f.bundles[0], f.bundles[1].info.PublicURL, f.keys[1])
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	admission := func() store.ClusterAdmission {
		t.Helper()
		var row store.ClusterAdmission
		if err := f.stores[0].DB().WithContext(ctx).Where("key_fingerprint = ?", f.keys[0]).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		return row
	}
	for _, shape := range []string{"warm33", "mixed", "large", "byte-boundary", "missing", "corrupt", "partial", "badmeta", "unavailable", "cancel", "deadline", "membership"} {
		t.Run(shape, func(t *testing.T) {
			workCtx, workCancel := context.WithCancel(ctx)
			defer workCancel()
			mu.Lock()
			mode = shape
			batches = nil
			heads = 0
			uploads = 0
			gets = 0
			stop = workCancel
			mu.Unlock()
			if err := f.stores[0].SetClusterEnrollmentOriginForVerifiedKey(ctx, expected, f.keys[1], f.bundles[1].info.PublicURL); err != nil {
				t.Fatal(err)
			}
			_, members, err := f.stores[0].GrantClusterMembership(ctx, expected, f.keys[0])
			if err != nil {
				t.Fatal(err)
			}
			var target store.ClusterTransportMember
			for _, member := range members {
				if member.ID == f.databases[1].ID {
					target = member
				}
			}
			if target.ID == 0 {
				t.Fatal("receiver missing from actual native roster")
			}
			count := 4
			if shape == "warm33" {
				count = 33
			}
			if shape == "byte-boundary" {
				count = 3
			}
			var files []MaterialFile
			local := map[string]string{}
			for i := range count {
				body := []byte(fmt.Sprintf("%s complete canonical receipt material %d", shape, i))
				if shape == "large" && i == 2 {
					body = bytes.Repeat([]byte("L"), int(maxMaterialBatchBytes)+1)
				}
				if shape == "byte-boundary" {
					body = bytes.Repeat([]byte{byte(i + 1)}, int(maxMaterialBatchBytes)/2)
				}
				digest := materialTestDigest(body)
				source := filepath.Join(t.TempDir(), "source")
				if err := os.WriteFile(source, body, 0600); err != nil {
					t.Fatal(err)
				}
				files = append(files, MaterialFile{Class: MaterialAgentOverride, Path: source, SHA256: digest})
				local[digest] = source
				old := shape != "mixed" || i != 1
				if old {
					if _, err := f.receivers[1].receive(ctx, digest, bytes.NewReader(body)); err != nil {
						t.Fatal(err)
					}
					if err := f.stores[0].RecordMaterialRemoteReceipt(ctx, digest, target.ID, int64(len(body))); err != nil {
						t.Fatal(err)
					}
				}
			}
			if shape == "warm33" || shape == "mixed" {
				files = append(files, files[0])
			}
			if shape == "missing" {
				digest := files[2].SHA256
				if err := os.Remove(filepath.Join(f.receivers[1].root, digest[:2], digest)); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "corrupt" {
				digest := files[2].SHA256
				info, err := os.Stat(local[digest])
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.receivers[1].root, digest[:2], digest), bytes.Repeat([]byte("x"), int(info.Size())), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "deadline" {
				var childCancel context.CancelFunc
				workCtx, childCancel = context.WithTimeout(workCtx, 40*time.Millisecond)
				defer childCancel()
			}
			if shape == "warm33" {
				if err := f.stores[0].DB().WithContext(ctx).Exec("CREATE TRIGGER receipt_hint_no_rewrite BEFORE INSERT ON material_remote_receipts BEGIN SELECT RAISE(ABORT, 'warm hints must not write receipts'); END").Error; err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := f.stores[0].DB().WithContext(ctx).Exec("DROP TRIGGER receipt_hint_no_rewrite").Error; err != nil {
						t.Error(err)
					}
				}()
			}
			before := admission()
			notices := 0
			changed, err := replicateMaterialInventoryToPeer(workCtx, f.stores[0], client, target, expected, f.keys[0], files, local, func() { notices++ })
			success := shape != "corrupt" && shape != "cancel" && shape != "deadline" && shape != "membership"
			if (err == nil) != success {
				t.Fatalf("changed=%v notices=%d success=%v err=%v", changed, notices, success, err)
			}
			if shape == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancel: %v", err)
			}
			if shape == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost deadline: %v", err)
			}
			mu.Lock()
			gotBatches := slices.Clone(batches)
			gotHeads, gotUploads, gotGets := heads, uploads, gets
			mu.Unlock()
			after := admission()
			switch shape {
			case "warm33":
				if !slices.Equal(gotBatches, []int{32, 1}) || gotHeads != 0 || gotUploads != 0 || gotGets != 0 || changed || notices != 0 {
					t.Errorf("warm counts batches=%v HEAD/POST/GET=%d/%d/%d notices=%d changed=%v", gotBatches, gotHeads, gotUploads, gotGets, notices, changed)
				}
				if delta := after.MaterialGrantSequence - before.MaterialGrantSequence; delta != 35 {
					t.Errorf("warm native grants=%d want 2 receiver + 33 fresh sender checks", delta)
				}
			case "mixed":
				if !slices.Equal(gotBatches, []int{1, 2}) || gotHeads != 0 || gotUploads != 1 || gotGets != 1 || !changed || notices != 1 {
					t.Fatalf("mixed counts batches=%v HEAD/POST/GET=%d/%d/%d notices=%d changed=%v", gotBatches, gotHeads, gotUploads, gotGets, notices, changed)
				}
			case "large":
				if !slices.Equal(gotBatches, []int{2, 1}) || gotHeads != 1 || gotUploads != 0 || changed || notices != 0 {
					t.Fatalf("large counts batches=%v HEAD/POST=%d/%d", gotBatches, gotHeads, gotUploads)
				}
			case "byte-boundary":
				if !slices.Equal(gotBatches, []int{2, 1}) || gotHeads != 0 || gotUploads != 0 || changed {
					t.Fatalf("byte-boundary batches=%v HEAD/POST=%d/%d", gotBatches, gotHeads, gotUploads)
				}
			case "missing":
				if !slices.Equal(gotBatches, []int{4}) || gotHeads != 4 || gotUploads != 1 || gotGets != 1 || !changed || notices != 1 {
					t.Fatalf("missing counts batches=%v HEAD/POST/GET=%d/%d/%d changed=%v", gotBatches, gotHeads, gotUploads, gotGets, changed)
				}
			case "corrupt":
				if gotHeads != 3 || gotUploads != 1 {
					t.Fatalf("corruption didn't use fail-closed canonical repair: HEAD=%d POST=%d", gotHeads, gotUploads)
				}
			case "partial", "badmeta", "unavailable":
				if !slices.Equal(gotBatches, []int{4}) || gotHeads != 4 || gotUploads != 0 || changed || notices != 0 {
					t.Fatalf("failed batch falsely skipped original HEAD: batches=%v HEAD/POST=%d/%d changed=%v", gotBatches, gotHeads, gotUploads, changed)
				}
			case "cancel", "deadline", "membership":
				if shape == "membership" && !strings.Contains(err.Error(), "changed during transfer") {
					t.Fatalf("full readback did not reject the changed exact peer: %v", err)
				}
				if gotHeads != 0 || gotUploads != 0 || gotGets != 0 || notices != 0 || changed {
					t.Fatalf("terminated proof entered repair: HEAD/POST/GET=%d/%d/%d notices=%d", gotHeads, gotUploads, gotGets, notices)
				}
			}
			if success {
				for _, file := range files {
					info, err := os.Stat(local[file.SHA256])
					if err != nil {
						t.Fatal(err)
					}
					receipt, err := f.stores[0].HasMaterialRemoteReceipt(ctx, file.SHA256, target.ID, info.Size())
					if err != nil || !receipt {
						t.Fatalf("receipt=%v err=%v", receipt, err)
					}
					remote, err := os.ReadFile(filepath.Join(f.receivers[1].root, file.SHA256[:2], file.SHA256))
					if err != nil || materialTestDigest(remote) != file.SHA256 {
						t.Fatalf("canonical receiver differs: %v", err)
					}
				}
			}
		})
	}
}
