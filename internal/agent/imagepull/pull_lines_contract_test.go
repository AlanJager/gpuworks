package imagepull

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const contractDigest = "sha256:a171f301df7b8d27e72eb53099ab1a7b548ec5b3ac8baf546ea16d2fa64c46aa"

// Every line a driver builds with runtime.PullLine* must read back here as
// what it says: its layer, its bytes, its phase, whether it finishes the
// layer. A change to either side that breaks the other fails here first.
func TestEveryRuntimePullLineReadsBackAsItSays(t *testing.T) {
	const key = "a171f301df7b8d27e72eb53099ab1a7b548ec5b3ac8baf546ea16d2fa64c46aa"
	for _, c := range []struct {
		name      string
		line      string
		phase     string
		layer     pullLayerProgress
		armsWatch bool
	}{
		{"downloading", runtime.PullLineDownloading(contractDigest, 10, 100),
			proto.PhaseImagePull, pullLayerProgress{done: 10, total: 100, phase: "downloading"}, true},
		{"downloading, bare hex", runtime.PullLineDownloading(key, 10, 100),
			proto.PhaseImagePull, pullLayerProgress{done: 10, total: 100, phase: "downloading"}, true},
		{"downloading all of it", runtime.PullLineDownloading(contractDigest, 100, 100),
			proto.PhaseImagePull, pullLayerProgress{done: 100, total: 100, phase: "downloading"}, false},
		{"downloading in steps", runtime.PullLineDownloadingInSteps(contractDigest, 1181116006, 1610612736, 107374182),
			proto.PhaseImagePull, pullLayerProgress{done: 1181116006, total: 1610612736, step: 107374182, phase: "downloading"}, true},
		{"extracting", runtime.PullLineExtracting(contractDigest, 10, 100),
			proto.PhaseImageExtract, pullLayerProgress{done: 10, total: 100, phase: "extracting"}, false},
		// A finishing line leaves the phase as it was (here: none yet).
		{"download complete", runtime.PullLineDownloadComplete(contractDigest),
			"", pullLayerProgress{phase: "complete"}, false},
		{"already exists", runtime.PullLineAlreadyExists(contractDigest),
			"", pullLayerProgress{phase: "complete"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := NewProgress()
			phase, _, _, _, _, _ := p.Observe(c.line, time.Unix(100, 0))
			if phase != c.phase {
				t.Errorf("phase = %q, want %q", phase, c.phase)
			}
			if got := p.layers[key]; got != c.layer || len(p.layers) != 1 {
				t.Errorf("layers = %+v, want only %s = %+v", p.layers, key, c.layer)
			}
			if _, downloading := p.Fingerprint(); downloading != c.armsWatch {
				t.Errorf("stall watch armed = %v, want %v", downloading, c.armsWatch)
			}
		})
	}

	// The whole-image extracting line names no layer and stops judging.
	for _, seconds := range []int{0, 11} {
		p := NewProgress()
		t0 := time.Unix(100, 0)
		p.Observe(runtime.PullLineDownloading(contractDigest, 10, 100), t0)
		phase, _, _, _, _, _ := p.Observe(runtime.PullLineExtractingLayers(seconds), t0.Add(time.Second))
		if phase != proto.PhaseImageExtract {
			t.Errorf("ExtractingLayers(%d): phase = %q", seconds, phase)
		}
		if _, downloading := p.Fingerprint(); downloading {
			t.Errorf("ExtractingLayers(%d) left the stall watch armed", seconds)
		}
	}

	// A finishing line completes what a counting line started.
	for _, finish := range []string{runtime.PullLineDownloadComplete(contractDigest), runtime.PullLineDownloading(contractDigest, 100, 100)} {
		p := NewProgress()
		t0 := time.Unix(100, 0)
		p.Observe(runtime.PullLineDownloadingInSteps(contractDigest, 99, 100, 10), t0)
		p.Observe(finish, t0.Add(time.Second))
		if _, downloading := p.Fingerprint(); downloading {
			t.Errorf("%q did not finish the layer", finish)
		}
	}
}

func TestPullLineStepIsReadOnlyFromItsOwnLines(t *testing.T) {
	for line, want := range map[string]int64{
		runtime.PullLineDownloadingInSteps(contractDigest, 1, 2, 107374182): 107374182,
		runtime.PullLineDownloadingInSteps(contractDigest, 1, 2, 0):         0,
		runtime.PullLineDownloading(contractDigest, 1, 2):                   0,
		"a171f301df7b: Downloading  123.4MB/1.2GB":                          0,
	} {
		if got := runtime.PullLineStep(line); got != want {
			t.Errorf("PullLineStep(%q) = %d, want %d", line, got, want)
		}
	}
}

// A layer shown in 0.1 GiB steps cannot prove a stall in three minutes once
// it is slower than ~600 KB/s: the window stretches until one step fits at the
// minimum rate, and only while such a layer is still downloading.
func TestStallWindowStretchesForCoarseCountsOnly(t *testing.T) {
	const base, minRate = 3 * time.Minute, int64(100 << 10)
	t0 := time.Unix(100, 0)

	exact := NewProgress()
	exact.Observe("a171f301df7b: Downloading  123.4MB/1.2GB", t0)
	exact.Observe(runtime.PullLineDownloading(contractDigest, 10, 100), t0)
	if w := exact.StallWindow(base, minRate); w != base {
		t.Fatalf("exact counts: window = %s, want %s", w, base)
	}

	fine := NewProgress()
	fine.Observe(runtime.PullLineDownloadingInSteps(contractDigest, 1<<20, 1<<30, 104857), t0)
	if w := fine.StallWindow(base, minRate); w != base {
		t.Fatalf("0.1 MiB steps: window = %s, want %s", w, base)
	}

	coarse := NewProgress()
	coarse.Observe(runtime.PullLineDownloadingInSteps(contractDigest, 1181116006, 1610612736, 107374182), t0)
	// 107374182 B at 102400 B/s is 1048.6 s.
	if w := coarse.StallWindow(base, minRate); w != 1049*time.Second {
		t.Fatalf("0.1 GiB steps: window = %s, want 17m29s", w)
	}
	if w := coarse.StallWindow(base, 0); w != base {
		t.Fatalf("no minimum rate: window = %s, want %s", w, base)
	}
	coarse.Observe(runtime.PullLineDownloadComplete(contractDigest), t0.Add(time.Minute))
	if w := coarse.StallWindow(base, minRate); w != base {
		t.Fatalf("finished coarse layer: window = %s, want %s", w, base)
	}
}

// Once the driver says it cannot read the runtime's output, the pull is not
// judged again, whatever counts follow; the notice itself moves no phase and
// carries no bytes.
func TestAnUnreadableNoticeTurnsTheStallWatchOffForTheRest(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	p.Observe(runtime.PullLineDownloading(contractDigest, 10, 100), t0)
	phase, _, _, _, _, _ := p.Observe(runtime.PullLineUnreadable(`nerdctl blob status "unpacking"`), t0.Add(time.Second))
	if phase != proto.PhaseImagePull {
		t.Fatalf("phase after the notice = %q", phase)
	}
	if _, downloading := p.Fingerprint(); downloading {
		t.Fatal("still judged after the notice")
	}
	p.Observe(runtime.PullLineDownloading(contractDigest, 20, 100), t0.Add(2*time.Second))
	if _, downloading := p.Fingerprint(); downloading {
		t.Fatal("a later count armed the watch again")
	}
	if _, _, done, total, _, _ := p.Snapshot(t0.Add(3 * time.Second)); done != 20 || total != 100 {
		t.Fatalf("bytes still shown = %d/%d, want 20/100", done, total)
	}
}
