package stallwatch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	reportPrefix = "stall-"
	reportSuffix = ".txt"
	// pressureFile is the kernel's account of tasks waiting on I/O; reading
	// it touches no disk.
	pressureFile = "/proc/pressure/io"
)

// reportName names a report by the moment the stall was noticed, so the
// directory sorts by time.
func reportName(at time.Time) string {
	return reportPrefix + at.UTC().Format("20060102T150405.000Z") + reportSuffix
}

// Reports lists dir's report files, oldest first. A directory that does not
// exist has none.
func Reports(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, reportPrefix) && strings.HasSuffix(name, reportSuffix) {
			names = append(names, filepath.Join(dir, name))
		}
	}
	sort.Strings(names)
	return names
}

// ReportTime is the moment a report file's name records.
func ReportTime(path string) (time.Time, bool) {
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), reportPrefix), reportSuffix)
	at, err := time.Parse("20060102T150405.000Z", name)
	return at, err == nil
}

// record writes one event out: the file first, the log line after, so a
// logger that is itself what stalled cannot keep the report from the disk.
// It runs on the reporting goroutine only and may block for as long as the
// disk or the logger does.
func (w *Watchdog) record(ev event) {
	path := ""
	if w.opts.Dir != "" {
		path = filepath.Join(w.opts.Dir, ev.name)
	}
	switch ev.kind {
	case eventStall:
		var b bytes.Buffer
		b.WriteString("gpuworks stall report\n")
		for _, line := range w.opts.Header {
			b.WriteString(line + "\n")
		}
		fmt.Fprintf(&b, "noticed: %s\nthreshold: %s\n", ev.at.UTC().Format(time.RFC3339Nano), w.opts.Threshold)
		writeWaiting(&b, ev.waiting)
		if ev.suppressed > 0 {
			fmt.Fprintf(&b, "stalls without a report since the previous one: %d\n", ev.suppressed)
		}
		if dropped := w.dropped.Swap(0); dropped > 0 {
			fmt.Fprintf(&b, "records lost while reporting was behind: %d\n", dropped)
		}
		if pressure, err := os.ReadFile(pressureFile); err == nil {
			fmt.Fprintf(&b, "io pressure:\n%s", indent(string(pressure)))
		}
		writeStacks(&b, ev)
		w.write(path, b.Bytes(), false)
		w.sweep()
	case eventStill:
		var b bytes.Buffer
		fmt.Fprintf(&b, "\nstill waiting at %s\n", ev.at.UTC().Format(time.RFC3339Nano))
		writeWaiting(&b, ev.waiting)
		writeStacks(&b, ev)
		w.write(path, b.Bytes(), true)
		w.opts.Log.Warn("process stall continues", "site", ev.waiting[0].Site, "waited", ev.duration.Round(time.Millisecond).String(), "report", path)
	case eventEnd:
		line := fmt.Sprintf("\nended: %s, after %s\n", ev.at.UTC().Format(time.RFC3339Nano), ev.duration.Round(time.Millisecond))
		w.write(path, []byte(line), true)
		w.opts.Log.Warn("process stall recorded", "site", strings.Join(ev.sites, ", "), "duration", ev.duration.Round(time.Millisecond).String(), "report", path)
	case eventPause:
		var b bytes.Buffer
		b.WriteString("gpuworks stall report\n")
		for _, line := range w.opts.Header {
			b.WriteString(line + "\n")
		}
		fmt.Fprintf(&b, "noticed: %s\nthreshold: %s\n", ev.at.UTC().Format(time.RFC3339Nano), w.opts.Threshold)
		fmt.Fprintf(&b, "the whole process did not run for %s: nothing in it was scheduled, so no stacks were taken\n", ev.duration.Round(time.Millisecond))
		if ev.suppressed > 0 {
			fmt.Fprintf(&b, "stalls without a report since the previous one: %d\n", ev.suppressed)
		}
		if pressure, err := os.ReadFile(pressureFile); err == nil {
			fmt.Fprintf(&b, "io pressure:\n%s", indent(string(pressure)))
		}
		w.write(path, b.Bytes(), false)
		w.sweep()
		w.opts.Log.Warn("process pause recorded", "duration", ev.duration.Round(time.Millisecond).String(), "report", path)
	}
}

func writeWaiting(b *bytes.Buffer, waiting []Waiting) {
	b.WriteString("waiting:\n")
	for _, one := range waiting {
		fmt.Fprintf(b, "  %s  %s\n", one.For.Round(time.Millisecond), one.Site)
	}
}

func writeStacks(b *bytes.Buffer, ev event) {
	if ev.cut {
		fmt.Fprintf(b, "goroutines (cut at %d bytes):\n\n", maxStackBytes)
	} else {
		b.WriteString("goroutines:\n\n")
	}
	b.Write(ev.stacks)
	if len(ev.stacks) > 0 && ev.stacks[len(ev.stacks)-1] != '\n' {
		b.WriteByte('\n')
	}
}

func indent(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// write puts data in the report file. A report that cannot be written is
// dropped: recording a stall must never become one.
func (w *Watchdog) write(path string, data []byte, add bool) {
	if path == "" {
		return
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if add {
		flags = os.O_WRONLY | os.O_APPEND
	} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(data)
	_ = f.Close()
}

// sweep keeps the newest reports.
func (w *Watchdog) sweep() {
	if w.opts.Dir == "" {
		return
	}
	reports := Reports(w.opts.Dir)
	for len(reports) > w.opts.Keep {
		_ = os.Remove(reports[0])
		reports = reports[1:]
	}
}
