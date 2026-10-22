package nerdctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The files under testdata/pull-nerdctl-2.1.6 are real `nerdctl pull` output,
// byte for byte (colour codes, tabwriter padding, trailing blanks included),
// captured 2026-10-20 from nerdctl 2.1.6 (built from its v2.1.6 module, the
// version the agent image ships) against containerd 2.x, with stdin not a
// terminal as under the agent. The image had a 1.5 GiB layer, a ~1 GB layer of
// 60,000 small files and three layers of 138–140 bytes, served by a local
// registry behind a proxy that delays every request by 0.4 s so the small
// blobs are seen in flight as they are from a remote registry.
//
//   - download.txt: the frames from the start until the last downloading
//     frame, keeping every frame of the first 2.6 s and every tenth after. The
//     frame at 1.94 s, which happened to catch the small blobs at offset ==
//     total just before they were committed, is left out: on a remote registry
//     the 100 ms redraw usually misses that instant, and it is then the
//     in-flight "0.0 B/140.0 B" row that is the last one shown.
//   - downloading-frame.txt: one frame at 9.0 s, mid-download.
//   - unpacking-frame.txt: one frame at 30.0 s; the download ended at 18.9 s
//     and the pull returned at 41.5 s, every frame in between identical but
//     for the elapsed row. The 941.8 MiB layer shows "waiting" in them: it
//     went back to waiting when its download ended at 11.9 s.
//   - end.txt: the last frame before the pull returned.
func readPullFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pull-nerdctl-2.1.6", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func convertPull(lines ...string) (out []string, table []bool) {
	f := newPullFrames(func(s string) { out = append(out, s) })
	for _, chunk := range lines {
		for _, l := range strings.Split(strings.TrimSuffix(chunk, "\n"), "\n") {
			table = append(table, f.line(l))
		}
	}
	return out, table
}

const (
	bigLayer   = "a171f301df7b8d27e72eb53099ab1a7b548ec5b3ac8baf546ea16d2fa64c46aa"
	smallLayer = "9250fc1e012caebd10d2bf1d40b1b5dd879b8c3b0f03483b77b16e2eeaecfb42"
)

func TestPullFramesTurnTheRealTableIntoLayerLines(t *testing.T) {
	download := readPullFixture(t, "download.txt")
	unpack := readPullFixture(t, "unpacking-frame.txt")
	out, _ := convertPull(download, unpack, unpack, unpack, readPullFixture(t, "end.txt"))
	all := strings.Join(out, "\n")

	if strings.Contains(all, "\x1b") || strings.Contains(all, "elapsed:") || strings.Contains(all, "resolv") {
		t.Fatalf("table furniture leaked into the lines:\n%s", all)
	}
	if !strings.Contains(all, `level=warning msg="skipping verifying HTTPS certs`) {
		t.Fatalf("nerdctl's own log line was not passed through:\n%s", all)
	}
	// The 1.5 GiB layer reports bytes against its full size; "1.5 GiB" is
	// 1.5 * 2^30.
	if !strings.Contains(all, "sha256:"+bigLayer+": Downloading ") || !strings.Contains(all, " B/1610612736 B") {
		t.Fatalf("the big layer's bytes are missing:\n%s", all)
	}
	// Every blob of the image ends finished: two big layers, three small
	// ones, the manifest and the config.
	if n := strings.Count(all, ": Download complete"); n != 7 {
		t.Fatalf("%d blobs finished, want 7:\n%s", n, all)
	}
	// The small blob seen in flight is finished by its "done" row.
	if !strings.Contains(all, "sha256:"+smallLayer+": Downloading 0 B/140 B") || !strings.Contains(all, "sha256:"+smallLayer+": Download complete") {
		t.Fatalf("small blob not tracked to its end:\n%s", all)
	}
	// Unpacking is announced once: the repeated frames carry the same
	// elapsed time, and the end frame is only 11 s later.
	if n := strings.Count(all, "Extracting downloaded layers"); n != 2 {
		t.Fatalf("Extracting reported %d times, want the start and the 10 s mark:\n%s", n, all)
	}
	if !strings.HasSuffix(all, "Extracting downloaded layers (11s)") {
		t.Fatalf("last line = %q", out[len(out)-1])
	}
	// The table is redrawn every 100 ms; only changes survive.
	if in := strings.Count(download, "\n"); len(out)*3 > in {
		t.Fatalf("%d lines out of %d in: repeated frames are not dropped", len(out), in)
	}
}

func TestPullFramesNeverRepeatAnUnchangedFrame(t *testing.T) {
	frame := readPullFixture(t, "downloading-frame.txt")
	first, _ := convertPull(frame)
	again, _ := convertPull(frame, frame, frame, frame)
	if len(first) == 0 || len(again) != len(first) {
		t.Fatalf("one frame gave %d lines, four identical frames gave %d", len(first), len(again))
	}
}

// These rows are generated, not captured: they follow nerdctl 2.1.6
// jobs.Display's "%s:\t%s\t%40r\t%8.8s/%s\t\n" with containerd's
// progress.Bytes ("%02.1f %s", IEC units), whose eight-character cut drops the
// end of the done half's unit once the number has four digits or more.
func TestPullFramesReadTruncatedUnits(t *testing.T) {
	const key = "layer-sha256:" + bigLayer
	bar := "|\x1b[32m+++++++++++++++\x1b[0m-----------------------| "
	for _, c := range []struct {
		row  string
		want string
	}{
		// The step is one decimal of the done half's unit.
		{"123.4 Mi/3.4 GiB", "Downloading 129394278 B/3650722201 B (in steps of 104857 B)"},
		{"1023.9 K/2.0 MiB", "Downloading 1048473 B/2097152 B (in steps of 102 B)"},
		{"   0.0 B/182.0 B", "Downloading 0 B/182 B"},
		{"  2.0 Ki/2.0 KiB", "Downloading 2047 B/2048 B (in steps of 102 B)"}, // rounded up to its total: still in flight
		{"  1.5 GiB/1.5 GiB", "Downloading 1610612735 B/1610612736 B (in steps of 107374182 B)"},
	} {
		out, table := convertPull(key + ":    downloading    " + bar + c.row + " ")
		if len(out) != 1 || out[0] != "sha256:"+bigLayer+": "+c.want || !table[0] {
			t.Errorf("%q -> %q (table %v), want %q", c.row, out, table, c.want)
		}
	}
}

func TestPullFramesReportUnpackingEveryTenSecondsAndStopWhenDownloadingResumes(t *testing.T) {
	const key = "layer-sha256:" + bigLayer
	done := key + ":    done           |\x1b[32m++++\x1b[0m| \n"
	busy := key + ":    downloading    |\x1b[32m++\x1b[0m--|  1.0 GiB/1.5 GiB \n"
	frame := func(row string, elapsed string) string {
		return row + "elapsed: " + elapsed + "s    total:  1.5 Gi (1 MiB/s)\n"
	}
	out, _ := convertPull(
		frame(done, "20.0"), frame(done, "25.0"), frame(done, "31.0"), frame(done, "41.2"),
		frame(busy, "42.0"), frame(done, "43.0"),
	)
	want := []string{
		"sha256:" + bigLayer + ": Download complete",
		"Extracting downloaded layers",
		"Extracting downloaded layers (11s)",
		"Extracting downloaded layers (21s)",
		"sha256:" + bigLayer + ": Downloading 1073741824 B/1610612736 B (in steps of 107374182 B)",
		"sha256:" + bigLayer + ": Download complete",
		"Extracting downloaded layers",
	}
	if strings.Join(out, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(out, "\n"), strings.Join(want, "\n"))
	}
}

func TestPullFramesDoNotCallAFrameWithoutLayersUnpacking(t *testing.T) {
	// Right after the manifest arrives the table lists it and the config as
	// done before any layer row exists.
	out, _ := convertPull(
		"r.example/x:1:    resolved       |\x1b[32m++++\x1b[0m| ",
		"manifest-sha256:"+bigLayer+": done           |\x1b[32m++++\x1b[0m| ",
		"elapsed: 0.6 s    total:  1.1 Ki (1.8 KiB/s)",
	)
	for _, l := range out {
		if strings.Contains(l, "Extracting") {
			t.Fatalf("a frame with no layer was reported as unpacking: %q", out)
		}
	}
}

func TestPullFramesHandleAnExistsOnlyFrameAndUnknownRows(t *testing.T) {
	out, table := convertPull(
		"layer-sha256:"+bigLayer+":    exists         |\x1b[32m++++\x1b[0m| ",
		"layer-sha256:"+smallLayer+":    waiting        |\x1b[32m\x1b[0m----| ",
		"elapsed: 0.2 s    total:   0.0 B (0.0 B/s)",
		"layer-sha256:"+smallLayer+":    exists         |\x1b[32m++++\x1b[0m| ",
		"elapsed: 0.3 s    total:   0.0 B (0.0 B/s)",
		"",
		"time=\"2026-10-20T06:14:55-05:00\" level=error msg=\"active check failed\" error=\"context canceled\"",
	)
	want := []string{
		"sha256:" + bigLayer + ": Already exists",
		"sha256:" + smallLayer + ": Already exists",
		"Extracting downloaded layers",
		`time="2026-10-20T06:14:55-05:00" level=error msg="active check failed" error="context canceled"`,
	}
	if strings.Join(out, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(out, "\n"), strings.Join(want, "\n"))
	}
	if !table[0] || !table[1] || !table[2] || !table[5] || table[6] {
		t.Fatalf("table flags = %v", table)
	}
}

// A failed pull's error carries nerdctl's own message, not the progress table
// it printed before failing.
func TestPullErrorKeepsTheMessageAndDropsTheTable(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "frames")
	if err := os.WriteFile(fixture, []byte(readPullFixture(t, "download.txt")), 0o600); err != nil {
		t.Fatal(err)
	}
	installFakeNerdctl(t, "cat '"+fixture+"' >&2\n"+
		`echo 'time="2026-10-20T06:30:00-05:00" level=fatal msg="failed to copy: httpReadSeeker: failed open: unexpected status code 503"' >&2`+"\nexit 1")
	var lines []string
	err := New().PullImage(context.Background(), "r.example/x:1", func(s string) { lines = append(lines, s) })
	if err == nil {
		t.Fatal("a failed pull returned no error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unexpected status code 503") || strings.Contains(msg, "downloading") || strings.Contains(msg, "elapsed:") {
		t.Fatalf("error = %q", msg)
	}
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "level=fatal") {
		t.Fatalf("the fatal line did not reach the caller: %q", lines)
	}
}

// In the capture, the 941.8 MiB layer went from downloading back to waiting
// when its download ended and stayed there for the 30 s of unpacking. The
// first two rows are the capture's, at 11.8 s and 11.9 s; a blob never seen
// downloading that is waiting is still queued.
func TestPullFramesCountAWaitingRowAfterADownloadAsFinished(t *testing.T) {
	const other = "2ef5388bbdeb62d3ec42e5ece9a1547a15d206a794d7b5e41c245af7249e4e2d"
	queued := "layer-sha256:" + bigLayer + ":    waiting        |\x1b[32m\x1b[0m--------------------------------------| "
	out, _ := convertPull(
		"layer-sha256:"+other+":    downloading    |\x1b[32m++++++++++++++++++++++++++++++++++++++\x1b[0m| 941.8 Mi/941.8 MiB ",
		"elapsed: 11.8s",
		"layer-sha256:"+other+":    waiting        |\x1b[32m\x1b[0m--------------------------------------| ",
		queued,
		"elapsed: 11.9s",
		"layer-sha256:"+other+":    waiting        |\x1b[32m\x1b[0m--------------------------------------| ",
		"elapsed: 12.0s",
	)
	want := []string{
		"sha256:" + other + ": Downloading 987548875 B/987548876 B (in steps of 104857 B)",
		"sha256:" + other + ": Download complete",
		"Extracting downloaded layers",
	}
	if strings.Join(out, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(out, "\n"), strings.Join(want, "\n"))
	}
}
