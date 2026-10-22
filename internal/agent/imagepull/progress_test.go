package imagepull

import (
	"testing"
	"time"
)

func TestImagePullProgressAggregatesLayersAndRate(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	phase, done, total, _, fraction, _ := p.Observe("abc12345: Downloading 12MB/100MB", t0)
	if phase != "image_pull" || done != 12*1024*1024 || total != 100*1024*1024 || fraction != 0.12 {
		t.Fatalf("first snapshot = phase %q done %d total %d fraction %v", phase, done, total, fraction)
	}
	phase, done, total, rate, fraction, _ := p.Observe("abc12345: Extracting 100MB/100MB", t0.Add(2*time.Second))
	if phase != "image_extract" || done != 100*1024*1024 || total != 100*1024*1024 || rate != 44*1024*1024 || fraction != 1 {
		t.Fatalf("second snapshot = phase %q done %d total %d rate %d fraction %v", phase, done, total, rate, fraction)
	}
}

func TestImagePullProgressSupportsUnknownUdockerLines(t *testing.T) {
	p := NewProgress()
	phase, done, total, _, fraction, _ := p.Observe("pulling layer from mirror", time.Now())
	if phase != "image_pull" || done != 0 || total != 0 || fraction != 0 {
		t.Fatalf("unknown snapshot = phase %q done %d total %d fraction %v", phase, done, total, fraction)
	}
}

func TestImagePullProgressDoesNotLoseRateToUnchangedLogLines(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	p.Observe("udocker storage: 100 MB / 1 GB", t0)
	for i := 1; i <= 5; i++ {
		p.Observe("layer: extracting", t0.Add(time.Duration(i)*time.Second))
	}
	_, _, _, rate, _, _ := p.Observe("udocker storage: 200 MB / 1 GB", t0.Add(6*time.Second))
	if rate <= 0 {
		t.Fatalf("rate = %d, want positive after unchanged log lines", rate)
	}
}

func TestImagePullProgressMergesPrefetchLayerLinesAndResetsAfterRetry(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	phase, done, total, _, fraction, _ := p.Observe("sha256:abcdef12: Downloading 0 B/33554432 B", t0)
	if phase != "image_pull" || done != 0 || total != 33554432 || fraction != 0 {
		t.Fatalf("initial prefetch snapshot = phase %q done %d total %d fraction %v", phase, done, total, fraction)
	}
	var rate int64
	_, done, total, rate, fraction, _ = p.Observe("sha256:abcdef12: Downloading 16777216 B/33554432 B", t0.Add(2*time.Second))
	if done != 16777216 || total != 33554432 || rate != 8388608 || fraction != 0.5 {
		t.Fatalf("prefetch snapshot = done %d total %d rate %d fraction %v", done, total, rate, fraction)
	}
	// A failed prefetch can restart the same blob from zero through udocker;
	// the parser must keep the rate non-negative and accept the new baseline.
	_, done, total, rate, _, _ = p.Observe("sha256:abcdef12: Downloading 4194304 B/33554432 B", t0.Add(3*time.Second))
	if done != 4194304 || total != 33554432 || rate < 0 {
		t.Fatalf("retry snapshot = done %d total %d rate %d", done, total, rate)
	}
}

func TestImagePullProgressSnapshotExpiresStaleRate(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	p.Observe("sha256:abcdef12: Downloading 0 B/33554432 B", t0)
	p.Observe("sha256:abcdef12: Downloading 16777216 B/33554432 B", t0.Add(2*time.Second))

	message, phase, done, total, rate, fraction := p.Snapshot(t0.Add(3 * time.Second))
	if message == "" || phase != "image_pull" || done != 16777216 || total != 33554432 || rate <= 0 || fraction != 0.5 {
		t.Fatalf("fresh snapshot = message %q phase %q done %d total %d rate %d fraction %v", message, phase, done, total, rate, fraction)
	}
	message, phase, done, total, rate, fraction = p.Snapshot(t0.Add(2*time.Second + imagePullRateStaleAfter))
	if message == "" || phase != "image_pull" || done != 16777216 || total != 33554432 || rate != 0 || fraction != 0.5 {
		t.Fatalf("stale snapshot = message %q phase %q done %d total %d rate %d fraction %v", message, phase, done, total, rate, fraction)
	}
}

// A docker pull prints one line per layer per refresh. The parser releases at
// most one of them per second per phase, so the report that reaches the control
// plane is a progress sample rather than a transcript.
func TestImagePullProgressReleasesOneLinePerSecondWithinAPhase(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	released := 0
	for i := 0; i < 200; i++ {
		// 200 lines spread over 999ms, i.e. entirely inside the first second.
		at := t0.Add(time.Duration(i) * 5 * time.Millisecond)
		if _, _, _, _, _, changed := p.Observe("abc12345: Downloading 1MB/100MB", at); changed {
			released++
		}
	}
	if released != 1 {
		t.Fatalf("released %d lines within one second, want exactly the first", released)
	}
}

func TestImagePullProgressReleasesAgainAfterTheIntervalElapses(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	if _, _, _, _, _, changed := p.Observe("abc12345: Downloading 1MB/100MB", t0); !changed {
		t.Fatal("the first line of a pull must be released")
	}
	for i := 1; i < 50; i++ {
		if _, _, _, _, _, changed := p.Observe("abc12345: Downloading 2MB/100MB", t0.Add(time.Duration(i)*10*time.Millisecond)); changed {
			t.Fatalf("line %d was released inside the same second", i)
		}
	}
	// A suppressed line must not push the window forward, otherwise a fast
	// stream would go permanently silent after its first line.
	if _, _, _, _, _, changed := p.Observe("abc12345: Downloading 40MB/100MB", t0.Add(time.Second)); !changed {
		t.Fatal("a line one second after the last released one must be released")
	}
	if _, _, _, _, _, changed := p.Observe("abc12345: Downloading 41MB/100MB", t0.Add(2*time.Second)); !changed {
		t.Fatal("the stream must keep releasing one line per second")
	}
}

func TestImagePullProgressReleasesEveryLineOfASlowStream(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	for i := 0; i < 5; i++ {
		at := t0.Add(time.Duration(i) * 3 * time.Second)
		if _, _, _, _, _, changed := p.Observe("abc12345: Downloading 1MB/100MB", at); !changed {
			t.Fatalf("line %d of a slower-than-1Hz stream was suppressed", i)
		}
	}
}

func TestImagePullProgressReleasesAPhaseChangeImmediately(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	p.Observe("abc12345: Downloading 1MB/100MB", t0)
	if _, _, _, _, _, changed := p.Observe("abc12345: Downloading 2MB/100MB", t0.Add(10*time.Millisecond)); changed {
		t.Fatal("a same-phase line inside the interval must be suppressed")
	}
	phase, _, _, _, _, changed := p.Observe("abc12345: Extracting 1MB/100MB", t0.Add(20*time.Millisecond))
	if !changed || phase != "image_extract" {
		t.Fatalf("phase change = phase %q changed %v, want image_extract released immediately", phase, changed)
	}
	// The phase change also opens a new interval rather than replaying the old
	// one: the next extract line inside the second stays suppressed.
	if _, _, _, _, _, changed := p.Observe("abc12345: Extracting 2MB/100MB", t0.Add(30*time.Millisecond)); changed {
		t.Fatal("a line inside the interval opened by the phase change must be suppressed")
	}
}

// The stall watchdog only judges a pull whose output carries byte counters.
// docker on a terminal prints them, in decimal units; docker on a pipe prints
// only per-layer states and is never judged, because it is silent for the
// whole of a large layer.
func TestFingerprintJudgesDockerTerminalOutputButNotPipeOutput(t *testing.T) {
	t0 := time.Unix(100, 0)
	terminal := NewProgress()
	for _, line := range []string{
		"3.20: Pulling from docker/library/alpine",
		"25f1d6b1951a: Pulling fs layer",
		"3a030ca3f633: Download complete",
		"25f1d6b1951a: Downloading [=====>    ]  1.049MB/3.624MB",
	} {
		terminal.Observe(line, t0)
	}
	fp, downloading := terminal.Fingerprint()
	if !downloading || fp.Done != 1099956 || fp.Phase != "image_pull" {
		t.Fatalf("terminal fingerprint = %+v downloading=%v", fp, downloading)
	}
	terminal.Observe("25f1d6b1951a: Downloading [=========> ]  2.1MB/3.624MB", t0.Add(time.Second))
	if next, _ := terminal.Fingerprint(); next == fp {
		t.Fatalf("new bytes must move the fingerprint: %+v", next)
	}
	terminal.Observe("25f1d6b1951a: Download complete", t0.Add(2*time.Second))
	if _, downloading := terminal.Fingerprint(); downloading {
		t.Fatal("after the last announced byte the pull is no longer judged")
	}

	pipe := NewProgress()
	for _, line := range []string{"3.20: Pulling from docker/library/alpine", "25f1d6b1951a: Pulling fs layer", "25f1d6b1951a: Waiting"} {
		pipe.Observe(line, t0)
	}
	if _, downloading := pipe.Fingerprint(); downloading {
		t.Fatal("output without byte counters is never judged")
	}
}

// A byte count on a line that names no layer can never be finished by a later
// line, so it must not arm the stall watch. These are real nerdctl 2.1.6 rows
// (colour codes included) for a 140-byte blob seen in flight and then done:
// fed to the parser raw, as before the nerdctl driver translated them, the
// count landed in no layer and froze the watch's view of the whole pull.
func TestFingerprintDoesNotJudgeCountsThatNameNoLayer(t *testing.T) {
	p := NewProgress()
	t0 := time.Unix(100, 0)
	p.Observe("layer-sha256:9250fc1e012caebd10d2bf1d40b1b5dd879b8c3b0f03483b77b16e2eeaecfb42:    downloading    |\x1b[32m\x1b[0m--------------------------------------|    0.0 B/140.0 B ", t0)
	p.Observe("layer-sha256:9250fc1e012caebd10d2bf1d40b1b5dd879b8c3b0f03483b77b16e2eeaecfb42:    done           |\x1b[32m++++++++++++++++++++++++++++++++++++++\x1b[0m| ", t0.Add(100*time.Millisecond))
	if _, downloading := p.Fingerprint(); downloading {
		t.Fatal("a count that names no layer armed the stall watch")
	}
	// It is still shown.
	if _, _, done, total, _, _ := p.Snapshot(t0.Add(time.Second)); done != 0 || total != 140 {
		t.Fatalf("snapshot bytes = %d/%d, want the count kept for display", done, total)
	}
	// A layer's own count does arm it, and the stray count does not hold it
	// open once the layer is done.
	p.Observe("sha256:a171f301df7b: Downloading 10 B/100 B", t0.Add(2*time.Second))
	if _, downloading := p.Fingerprint(); !downloading {
		t.Fatal("a layer's own count must arm the stall watch")
	}
	p.Observe("sha256:a171f301df7b: Download complete", t0.Add(3*time.Second))
	if _, downloading := p.Fingerprint(); downloading {
		t.Fatal("the stray count kept a finished pull judged")
	}
}
