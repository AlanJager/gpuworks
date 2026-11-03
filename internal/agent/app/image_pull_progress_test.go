package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type imagePullProgressDriver struct {
	*imagePullTestDriver
	interval time.Duration
}

func (d *imagePullProgressDriver) PullImage(ctx context.Context, _ string, onLine func(string)) error {
	for _, line := range d.pullLines {
		if d.interval > 0 {
			select {
			case <-time.After(d.interval):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		onLine(line)
	}
	return d.pullErr
}

// go test ./internal/agent/app -run '^TestRPCImagePullReportsByteProgress$' -count=1
func TestRPCImagePullReportsByteProgress(t *testing.T) {
	cases := []struct {
		name                          string
		lines                         []string
		interval                      time.Duration
		pullErr                       error
		wantDone, wantTotal, wantRate int64
	}{
		{
			name:      "docker tooltip",
			lines:     []string{"60c68e5ca95a: Downloading [===> ] 18.04MB/233.5MB"},
			wantDone:  18916311,
			wantTotal: 244842496,
		},
		{
			name: "layers and rate",
			lines: []string{
				"abc12345: Downloading 10 B/100 B",
				"def12345: Downloading 20 B/200 B",
				"abc12345: Downloading 30 B/100 B",
			},
			interval:  time.Second,
			wantDone:  50,
			wantTotal: 300,
			wantRate:  20,
		},
		{
			name:  "runtime without byte counters",
			lines: []string{"abc12345: Pulling fs layer", "abc12345: Waiting"},
		},
		{
			name:  "failure after partial download",
			lines: []string{"abc12345: Downloading 10 B/100 B"},
			// Let the initial report flush before emitting progress and failing.
			interval:  2 * statusProgressInterval,
			pullErr:   errors.New("registry connection lost"),
			wantDone:  10,
			wantTotal: 100,
		},
	}
	for _, candidates := range []bool{false, true} {
		mode := "legacy"
		if candidates {
			mode = "candidates"
		}
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					d := &imagePullProgressDriver{
						imagePullTestDriver: &imagePullTestDriver{tagImage: true, pullLines: tc.lines, pullErr: tc.pullErr},
						interval:            tc.interval,
					}
					n := NewNodeRuntime(slog.Default(), d, t.TempDir())
					p := proto.ImagePullParams{EffectiveRef: "mirror.example/image:v1", CanonicalRef: "registry.example/image:v1", TransferID: "download-1"}
					if candidates {
						p.Candidates = []proto.ImagePullCandidate{{Ref: p.EffectiveRef}}
					}
					raw, err := json.Marshal(p)
					if err != nil {
						t.Fatal(err)
					}
					statuses := pullRun(t, n, string(raw))
					lastLine := tc.lines[len(tc.lines)-1]
					sawProgress := false
					for _, st := range statuses {
						if st.ImageRef != p.CanonicalRef || st.TransferID != p.TransferID {
							t.Fatalf("status lost its image or transfer identity: %+v", st)
						}
						if st.State != "pulling" || st.Message != lastLine {
							continue
						}
						sawProgress = true
						if st.TransferredBytes != tc.wantDone || st.SizeBytes != tc.wantTotal || st.RateBPS != tc.wantRate {
							t.Errorf("progress = %d/%d at %d B/s, want %d/%d at %d B/s; message=%q", st.TransferredBytes, st.SizeBytes, st.RateBPS, tc.wantDone, tc.wantTotal, tc.wantRate, st.Message)
						}
						if candidates && st.Source != p.EffectiveRef {
							t.Errorf("source = %q, want %q", st.Source, p.EffectiveRef)
						}
					}
					if !sawProgress {
						t.Fatalf("download output never reached the status channel: %+v", statuses)
					}
					terminal := statuses[len(statuses)-1]
					if tc.pullErr != nil {
						if terminal.State != "failed" || !strings.Contains(terminal.Message, tc.pullErr.Error()) {
							t.Fatalf("pull failure was lost: %+v", terminal)
						}
					} else if terminal.State != "present" {
						t.Fatalf("successful pull did not finish: %+v", terminal)
					}
				})
			})
		}
	}
}
