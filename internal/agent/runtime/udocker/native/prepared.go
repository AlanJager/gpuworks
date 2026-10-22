package native

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// A pull made for a launch (runtime.WithLaunchIntent) unpacks the image while
// it downloads: each layer is extracted as soon as its own blob is verified,
// into UDOCKER_DIR/prepared/<digest>.building, published as prepared/<digest>.
// The next Create of that image claims the tree by rename instead of
// unpacking, so the first deployment of an image pays max(download, unpack)
// rather than their sum. Measured on a 19 GB vLLM image the two were 74 s and
// 98-119 s back to back (artifacts/work/2026-09-24-runveil/evidence.md).
//
// The job outlives the pull call: the pull returns once the image is
// registered, and Create waits for a job still in flight.
type preparedJobs struct {
	mu   sync.Mutex
	jobs map[string]*prepareJob
}

type prepareJob struct {
	done chan struct{}
	err  error
}

func (l Layout) preparedDir(manifestID string) string {
	return filepath.Join(l.Dir, "prepared", strings.TrimPrefix(manifestID, "sha256:"))
}

// startPrepare begins unpacking image's layers as their blobs arrive. It
// returns the callbacks the download reports to: fetched for each verified
// blob, and abort when the download fails. It returns nil callbacks when a
// prepared tree or a job for this manifest already exists.
func (e *Engine) startPrepare(image *Image, manifestID string) (fetched func(Descriptor), abort func()) {
	if isCompleteBase(filepath.Join(e.Layout.preparedDir(manifestID), "ROOT")) {
		return nil, nil
	}
	e.prepared.mu.Lock()
	if e.prepared.jobs == nil {
		e.prepared.jobs = map[string]*prepareJob{}
	}
	if job, running := e.prepared.jobs[manifestID]; running {
		select {
		case <-job.done:
		default:
			e.prepared.mu.Unlock()
			return nil, nil
		}
	}
	job := &prepareJob{done: make(chan struct{})}
	e.prepared.jobs[manifestID] = job
	e.prepared.mu.Unlock()

	ready := make(map[string]chan struct{}, len(image.Manifest.Layers))
	var readyMu sync.Mutex
	for _, layer := range image.Manifest.Layers {
		ready[layer.Digest] = make(chan struct{})
	}
	aborted := make(chan struct{})
	var abortOnce sync.Once
	abort = func() { abortOnce.Do(func() { close(aborted) }) }
	fetched = func(desc Descriptor) {
		readyMu.Lock()
		defer readyMu.Unlock()
		if ch, ok := ready[desc.Digest]; ok {
			close(ch)
			delete(ready, desc.Digest)
		}
	}

	layers := make([]LayerSource, 0, len(image.Manifest.Layers))
	for _, desc := range image.Manifest.Layers {
		readyMu.Lock()
		wait := ready[desc.Digest]
		readyMu.Unlock()
		blob := filepath.Join(e.Layout.LayersDir(), desc.Digest)
		layers = append(layers, LayerSource{
			Digest: desc.Digest,
			Size:   desc.Size,
			Open: func(ctx context.Context) (io.ReadCloser, error) {
				select {
				case <-wait:
				case <-aborted:
					return nil, errors.New("image download failed")
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				// The pull may have inflated this layer already (sidecar).
				if e.Sidecars != nil {
					if path, ok := e.Sidecars.Get(desc); ok {
						if file, err := os.Open(path); err == nil {
							return file, nil
						}
					}
				}
				return os.Open(blob)
			},
		})
	}

	e.base.wg.Add(1)
	go func() {
		defer e.base.wg.Done()
		defer close(job.done)
		job.err = e.buildPrepared(manifestID, layers)
		if job.err != nil {
			slog.Warn("rootfs prepared during pull discarded", "manifest", manifestID, "err", job.err)
		}
	}()
	return fetched, abort
}

// buildPrepared unpacks into prepared/<digest>.building and publishes it.
func (e *Engine) buildPrepared(manifestID string, layers []LayerSource) error {
	final := e.Layout.preparedDir(manifestID)
	building := final + ".building"
	if err := os.RemoveAll(building); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(building)
		}
	}()
	unpacker := &Unpacker{Root: filepath.Join(building, "ROOT"), Workers: e.Unpackers, Owner: e.Layout.Owner}
	if err := unpacker.Unpack(context.Background(), layers); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(building, ".complete"), []byte("ok\n"), 0o644); err != nil {
		return err
	}
	// The image may have been removed while this unpacked (it may not even be
	// registered yet, so the blobs are what is checked): never publish a tree
	// nothing can create from.
	for _, layer := range layers {
		if _, err := os.Stat(filepath.Join(e.Layout.LayersDir(), layer.Digest)); err != nil {
			return errors.New("image removed while its rootfs was being prepared")
		}
	}
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.Rename(building, final); err != nil {
		return err
	}
	published = true
	return nil
}

// claimPrepared moves a rootfs prepared for manifestID to root, waiting for a
// job still unpacking it. It reports false when there is none to claim, which
// leaves Create to unpack as usual; each prepared tree is claimed only once.
func (e *Engine) claimPrepared(ctx context.Context, manifestID, root string, onLine func(string)) bool {
	e.prepared.mu.Lock()
	job := e.prepared.jobs[manifestID]
	e.prepared.mu.Unlock()
	if job != nil {
		select {
		case <-job.done:
		default:
			report(onLine, "waiting for the root filesystem being unpacked during the pull")
			select {
			case <-job.done:
			case <-ctx.Done():
				return false
			}
		}
	}
	dir := e.Layout.preparedDir(manifestID)
	if !isCompleteBase(filepath.Join(dir, "ROOT")) {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return false
	}
	if err := os.Rename(filepath.Join(dir, "ROOT"), root); err != nil {
		return false // claimed concurrently, or not on the same filesystem
	}
	_ = os.RemoveAll(dir)
	report(onLine, "used the root filesystem unpacked during the pull")
	return true
}
