package diagnostics

import (
	"bufio"
	"bytes"
	"io"
	"time"
)

// filterLogWindow copies the lines of a slog text log (each record starts
// with time=<RFC 3339>) whose time lies in [from, to]. A line with no time
// of its own (a panic's stack, a multi-line value) goes with the record
// before it. It stops after max bytes and reports whether it did.
func filterLogWindow(r io.Reader, w io.Writer, from, to time.Time, max int64) (written int64, truncated bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	keep := false
	for sc.Scan() {
		line := sc.Bytes()
		if at, ok := lineTime(line); ok {
			keep = !at.Before(from) && !at.After(to)
		}
		if !keep {
			continue
		}
		if written+int64(len(line))+1 > max {
			return written, true, nil
		}
		n, err := w.Write(append(line, '\n'))
		written += int64(n)
		if err != nil {
			return written, false, err
		}
	}
	return written, false, sc.Err()
}

var timePrefix = []byte("time=")

func lineTime(line []byte) (time.Time, bool) {
	rest, ok := bytes.CutPrefix(line, timePrefix)
	if !ok {
		return time.Time{}, false
	}
	end := bytes.IndexByte(rest, ' ')
	if end < 0 {
		end = len(rest)
	}
	at, err := time.Parse(time.RFC3339Nano, string(rest[:end]))
	return at, err == nil
}
