package udocker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// reportStoreProgress sends an udocker CLI pull's progress to lines once a
// second until ctx ends: udocker prints no byte counts, so they are read off
// its layer store. Each layer is reported in the shared pull lines when its
// count changes, so the node's stall watch can judge it layer by layer.
func (d *Driver) reportStoreProgress(ctx context.Context, image string, baselineBytes int64, lines chan<- string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	reported := map[string]int64{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		layers, grown := d.pullStorageProgress(image, baselineBytes)
		if layers == nil {
			// No manifest yet: the growth is shown, but it names no layer
			// and so is never judged a stall.
			if grown > 0 {
				lines <- fmt.Sprintf("udocker storage: %d B / 0 B", grown)
			}
			continue
		}
		for _, layer := range layers {
			if last, ok := reported[layer.digest]; ok && last == layer.done {
				continue
			}
			reported[layer.digest] = layer.done
			lines <- runtime.PullLineDownloading(layer.digest, layer.done, layer.size)
		}
	}
}

// storageLayer is one layer of an udocker CLI pull as its file in the layer
// store shows it.
type storageLayer struct {
	digest     string
	done, size int64
}

// pullStorageProgress reads an udocker CLI pull's progress off its layer
// store, since udocker prints no byte counts. Once the image's manifest is
// written it reports each layer of known size; before that, layers is nil and
// grown is how much the store has grown since the pull began.
func (d *Driver) pullStorageProgress(image string, baselineBytes int64) (layers []storageLayer, grown int64) {
	layersDir := filepath.Join(d.udockerDir, "layers")
	if manifest, ok := pullManifest(filepath.Join(d.udockerDir, "repos"), image); ok {
		layers = []storageLayer{}
		for _, layer := range manifest.Layers {
			if layer.Size <= 0 {
				continue
			}
			l := storageLayer{digest: layer.Digest, size: layer.Size}
			if info, err := os.Stat(filepath.Join(layersDir, layer.Digest)); err == nil {
				l.done = minInt64(info.Size(), layer.Size)
			}
			layers = append(layers, l)
		}
		return layers, 0
	}
	if current := directoryFileBytes(layersDir); current > baselineBytes {
		grown = current - baselineBytes
	}
	return nil, grown
}

func directoryFileBytes(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
