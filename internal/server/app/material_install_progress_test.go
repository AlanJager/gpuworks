//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Each callback follows an actual filesystem operation in the production
// installer. The real pinned TLS client must survive the aggregate work while
// retaining its short idle window and requiring the final bound receipt.
func TestMaterialInstallationCompletedFileProgress(t *testing.T) {
	for _, mode := range []string{"complete", "corrupt-stage", "cancel", "owner-close", "write-failure", "data-root-replaced"} {
		t.Run(mode, func(t *testing.T) {
			dataDir, receiver, manifest, files, digest := receivedPackageRootFixture(t)
			requests := newMaterialRequestTracker()
			t.Cleanup(requests.closeAdmissions)
			var completed, received atomic.Int32
			var installErr error
			done := make(chan struct{})
			moved := dataDir + "-original"
			t.Cleanup(func() { _ = os.RemoveAll(moved) })
			client, origin := materialManifestProgressPeer(t, requests.wrap(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				r = r.WithContext(ctx)
				report := materialManifestProgressReporter(w, r, requests, 50*time.Millisecond)
				progress := func() error {
					n := completed.Add(1)
					if n == 1 {
						// At least one complete owned package file must exist before progress.
						var copied string
						_ = filepath.WalkDir(filepath.Join(dataDir, "catalog-packages"), func(name string, e fs.DirEntry, err error) error {
							if err == nil && !e.IsDir() && strings.Contains(name, ".material-package-") {
								copied = name
							}
							return err
						})
						if copied == "" {
							t.Error("progress preceded a completed package file")
							return errors.New("no completed file")
						}
						body, err := os.ReadFile(copied)
						if err != nil || len(body) == 0 {
							t.Error("completed copy has no bytes")
							return errors.New("no copied bytes")
						}
						switch mode {
						case "corrupt-stage":
							if err := os.WriteFile(copied, []byte("changed after copy"), 0600); err != nil {
								t.Error(err)
							}
						case "cancel":
							cancel()
						case "owner-close":
							requests.closeAdmissions()
						case "write-failure":
							return errors.New("progress write stopped")
						case "data-root-replaced":
							if err := os.Rename(dataDir, moved); err != nil {
								t.Error(err)
								return err
							}
							if err := os.Mkdir(dataDir, 0700); err != nil {
								t.Error(err)
								return err
							}
							if err := os.WriteFile(filepath.Join(dataDir, "foreign-owner"), []byte("retain"), 0600); err != nil {
								t.Error(err)
								return err
							}
						}
					}
					// This models bounded work after a completed unit, not an independent
					// timer heartbeat: there is no goroutine sending progress during a stall.
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(60 * time.Millisecond):
					}
					return report()
				}
				installErr = materializeReceivedManifestWithProgress(ctx, receiver, manifest, nil, nil, progress)
				if installErr != nil {
					http.Error(w, "installation stopped", http.StatusServiceUnavailable)
					return
				}
				if err := checkInstalledMaterialManifestWithProgress(ctx, receiver, manifest, func() error { return writeMaterialManifestProgress(w, r, requests) }); err != nil {
					installErr = err
					http.Error(w, "installed bytes changed", http.StatusServiceUnavailable)
					return
				}
				writeMaterialManifestReceipt(w, r, requests, 1, 2, digest, func() error {
					return checkInstalledMaterialManifestWithProgress(ctx, receiver, manifest, func() error { return writeMaterialManifestProgress(w, r, requests) })
				}, "installed bytes changed")
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
				if code != http.StatusProcessing || h.Get("X-Gpuworks-Material-Digest") != "" || h.Get("X-Gpuworks-Material-Source") != "" || h.Get("X-Gpuworks-Material-Member") != "" {
					t.Error("progress carried a premature receipt")
				}
				received.Add(1)
				return nil
			}})
			started := time.Now()
			err := materializeRemoteMaterialManifest(ctx, client, origin, digest, 1, 2)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("installer did not stop")
			}
			if mode == "complete" {
				if err != nil || installErr != nil {
					t.Fatalf("completed-file progress failed: client=%v install=%v", err, installErr)
				}
				if completed.Load() < int32(len(files)) || received.Load() < completed.Load() || time.Since(started) <= 200*time.Millisecond {
					t.Fatalf("whole package did not cross idle window with completed units: completed=%d received=%d", completed.Load(), received.Load())
				}
				for name, body := range files {
					got, e := os.ReadFile(filepath.Join(dataDir, "catalog-packages", digest, name))
					if e != nil || string(got) != string(body) {
						t.Fatalf("consumer did not receive complete bytes: %v", e)
					}
				}
			} else {
				if err == nil || installErr == nil {
					t.Fatalf("failed installation yielded a receipt: client=%v install=%v", err, installErr)
				}
				if mode == "data-root-replaced" {
					got, e := os.ReadFile(filepath.Join(dataDir, "foreign-owner"))
					if e != nil || string(got) != "retain" {
						t.Fatalf("replacement root changed: %v", e)
					}
				}
				if mode == "cancel" || mode == "owner-close" || mode == "write-failure" {
					if received.Load() != 0 {
						t.Fatal("stopped work sent progress")
					}
				}
			}
		})
	}
}

func TestMaterialInstalledProgressStillRechecksBytes(t *testing.T) {
	for _, mode := range []string{"corrupt-next", "cancel", "callback-error"} {
		t.Run(mode, func(t *testing.T) {
			_, receiver, manifest, _, _ := receivedPackageRootFixture(t)
			if err := materializeReceivedManifest(t.Context(), receiver, manifest); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			err := checkInstalledMaterialManifestWithProgress(ctx, receiver, manifest, func() error {
				calls++
				if calls == 1 {
					switch mode {
					case "corrupt-next":
						// Alter a later consumer while the proof is in progress; no caching
						// may replace the next byte read or the package's full validation.
						f := manifest.Files[len(manifest.Files)-1]
						return os.WriteFile(filepath.Join(filepath.Dir(receiver.root), f.Path), []byte("bad consumer"), 0600)
					case "cancel":
						cancel()
					case "callback-error":
						return errors.New("progress failed")
					}
				}
				return nil
			})
			if err == nil {
				t.Fatal("interrupted/changed installed proof succeeded")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
			// Each fault owns its fixture; the corrupt-next subcase alters bytes.
		})
	}
}

func TestMaterialManifestCompletedProgressSampling(t *testing.T) {
	for _, mode := range []string{"complete", "cancel-suppressed", "owner-close-suppressed", "stall"} {
		t.Run(mode, func(t *testing.T) {
			_, receiver, manifest, _, digest := receivedPackageRootFixture(t)
			if err := materializeReceivedManifest(t.Context(), receiver, manifest); err != nil {
				t.Fatal(err)
			}
			requests := newMaterialRequestTracker()
			t.Cleanup(requests.closeAdmissions)
			var completed, frames atomic.Int32
			done := make(chan struct{})
			client, origin := materialManifestProgressPeer(t, requests.wrap(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				r = r.WithContext(ctx)
				report := materialManifestProgressReporter(w, r, requests, time.Second)
				progress := func() error {
					n := completed.Add(1)
					if n == 2 {
						switch mode {
						case "cancel-suppressed":
							cancel()
						case "owner-close-suppressed":
							requests.closeAdmissions()
						case "stall":
							<-ctx.Done()
							return ctx.Err()
						}
					}
					return report()
				}
				for range 10 {
					if err := checkInstalledMaterialManifestWithProgress(ctx, receiver, manifest, progress); err != nil {
						http.Error(w, "proof stopped", http.StatusServiceUnavailable)
						return
					}
				}
				writeMaterialManifestReceipt(w, r, requests, 1, 2, digest, func() error { return checkInstalledMaterialManifest(ctx, receiver, manifest) }, "consumer changed")
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
				if code != http.StatusProcessing || h.Get("X-Gpuworks-Material-Digest") != "" {
					t.Error("progress exposed a receipt")
				}
				frames.Add(1)
				return nil
			}})
			err := materializeRemoteMaterialManifest(ctx, client, origin, digest, 1, 2)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("stopped sampler handler did not return")
			}
			if frames.Load() != 1 {
				t.Fatalf("rapid completed work was not sampled: frames=%d completed=%d", frames.Load(), completed.Load())
			}
			if mode == "complete" {
				if err != nil || completed.Load() != int32(10*len(manifest.Files)) {
					t.Fatalf("sampled complete proof failed: err=%v completed=%d", err, completed.Load())
				}
			} else if err == nil {
				t.Fatal("closed/canceled/stalled work produced receipt")
			}
		})
	}
}
