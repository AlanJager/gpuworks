package proto

import "testing"

func TestLaneFor(t *testing.T) {
	for _, kind := range []string{StreamLogtail, StreamTunnel, StreamModelRead, StreamModelWrite, StreamImageRead, StreamImageWrite, StreamLargeFileRead, StreamAgentUpgrade, StreamStorageUpload, StreamStorageDownload} {
		if got := LaneFor(kind); got != LaneData {
			t.Fatalf("%s: got %q", kind, got)
		}
	}
	for _, kind := range []string{StreamCtrl, StreamMetrics, StreamTerminal, "future"} {
		if got := LaneFor(kind); got != LaneCtrl {
			t.Fatalf("%s: got %q", kind, got)
		}
	}
}
