package native

import (
	"fmt"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Downloads and unpack workers report from many goroutines; the callbacks
// behind onLine are not concurrency-safe, so the reporter must serialize
// them. The unguarded counter below is what the race detector checks.
func TestByteReporterSerializesOnLine(t *testing.T) {
	lines := 0
	descs := make([]Descriptor, 16)
	for i := range descs {
		descs[i] = Descriptor{Digest: fmt.Sprintf("sha256:%02d", i), Size: 1 << 20}
	}
	reporter := newByteReporter(descs, func(string) { lines++ }, runtime.PullLineExtracting)
	var wg sync.WaitGroup
	for _, desc := range descs {
		wg.Add(1)
		go func(digest string) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				reporter.add(digest, 1<<10)
				reporter.say("line")
			}
		}(desc.Digest)
	}
	wg.Wait()
	reporter.finish()
	if lines == 0 {
		t.Fatal("no progress reported")
	}
}
