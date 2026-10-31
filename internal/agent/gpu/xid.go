package gpu

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// XidEvent is one NVIDIA Xid line from the kernel log.
type XidEvent struct {
	// Card is the card's bus address as the driver prints it
	// ("0000:17:00"), without the function.
	Card    string
	Xid     int
	At      time.Time
	Message string
}

// XidWatcher follows the kernel log (/dev/kmsg) for NVIDIA Xid events. It
// reads the log from the start of the ring buffer, so an agent restarted
// after an event still finds it; a reboot starts a new log, which is the
// other way an event stops counting. Reading /dev/kmsg needs root (or
// CAP_SYSLOG); without it the watcher says so once and finds nothing.
type XidWatcher struct {
	// Path is the log to read; empty is /dev/kmsg.
	Path string
	// BootTime converts the log's microseconds since boot to wall time;
	// nil reads /proc/uptime.
	BootTime func() time.Time

	mu     sync.Mutex
	byCard map[string][]XidEvent
}

// xidKeep is how many events are kept per card: the latest of each Xid is
// what matters, and a storm of one kind must not push out another.
const xidKeep = 16

var xidRE = regexp.MustCompile(`NVRM: Xid \(PCI:([0-9a-fA-F]{4,8}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2})\): (\d+),\s*(.*)$`)

// Run reads the log until ctx ends.
func (w *XidWatcher) Run(ctx context.Context) {
	path := w.Path
	if path == "" {
		path = "/dev/kmsg"
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Warn("NVIDIA Xid events are not read: the kernel log cannot be opened", "path", path, "err", err)
		return
	}
	go func() {
		<-ctx.Done()
		_ = f.Close()
	}()
	boot := w.bootTime()
	reader := bufio.NewReaderSize(f, 8192)
	for ctx.Err() == nil {
		// /dev/kmsg hands out one record per read; a regular file (a test)
		// one line per ReadString.
		line, err := reader.ReadString('\n')
		if line != "" {
			if ev, ok := parseXidRecord(line, boot); ok {
				w.add(ev)
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, syscall.EPIPE):
			// Records were overwritten before they were read; go on.
		case errors.Is(err, io.EOF):
			if path != "/dev/kmsg" {
				return
			}
			time.Sleep(time.Second)
		default:
			if ctx.Err() == nil {
				slog.Warn("NVIDIA Xid events: reading the kernel log failed", "err", err)
			}
			return
		}
	}
}

func (w *XidWatcher) bootTime() time.Time {
	if w.BootTime != nil {
		return w.BootTime()
	}
	b, err := os.ReadFile("/proc/uptime")
	if err == nil {
		if field, _, ok := strings.Cut(strings.TrimSpace(string(b)), " "); ok {
			if up, err := strconv.ParseFloat(field, 64); err == nil {
				return time.Now().Add(-time.Duration(up * float64(time.Second)))
			}
		}
	}
	return time.Now()
}

// parseXidRecord reads a /dev/kmsg record ("6,1234,5678901,-;NVRM: Xid ...").
func parseXidRecord(record string, boot time.Time) (XidEvent, bool) {
	prefix, message, ok := strings.Cut(strings.TrimRight(record, "\n"), ";")
	if !ok {
		return XidEvent{}, false
	}
	m := xidRE.FindStringSubmatch(message)
	if m == nil {
		return XidEvent{}, false
	}
	xid, err := strconv.Atoi(m[2])
	if err != nil {
		return XidEvent{}, false
	}
	ev := XidEvent{Card: strings.ToLower(xidCardKey(m[1])), Xid: xid, Message: strings.TrimSpace(m[3]), At: boot}
	if fields := strings.Split(prefix, ","); len(fields) >= 3 {
		if usec, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
			ev.At = boot.Add(time.Duration(usec) * time.Microsecond)
		}
	}
	return ev, true
}

func (w *XidWatcher) add(ev XidEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.byCard == nil {
		w.byCard = map[string][]XidEvent{}
	}
	events := w.byCard[ev.Card]
	for i := range events {
		if events[i].Xid == ev.Xid {
			events = append(events[:i], events[i+1:]...)
			break
		}
	}
	events = append(events, ev)
	if len(events) > xidKeep {
		events = events[len(events)-xidKeep:]
	}
	w.byCard[ev.Card] = events
	slog.Warn("NVIDIA Xid", "card", ev.Card, "xid", ev.Xid, "message", ev.Message, "at", ev.At.UTC().Format(time.RFC3339))
}

// Events are the latest event of each Xid on the card at bdf.
func (w *XidWatcher) Events(bdf string) []XidEvent {
	if w == nil {
		return nil
	}
	key := strings.ToLower(xidCardKey(bdf))
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]XidEvent(nil), w.byCard[key]...)
}

// xidCardKey is a card's address the way the driver prints it in an Xid:
// domain, bus and device, the domain in four digits.
func xidCardKey(bdf string) string {
	bdf = strings.TrimSpace(bdf)
	domain, rest, ok := strings.Cut(bdf, ":")
	if !ok {
		return bdf
	}
	for len(domain) > 4 && domain[0] == '0' {
		domain = domain[1:]
	}
	if dot := strings.LastIndex(rest, "."); dot >= 0 {
		rest = rest[:dot]
	}
	return domain + ":" + rest
}
