package conn

import (
	"fmt"
	"strings"
	"time"
)

// smokeTrace is the stage clock for one smoke attempt. Durations are the
// time spent inside a finished stage. An unfinished stage is still named,
// so a deadline says where the attempt was waiting.
type smokeTrace struct {
	marks []smokeMark
	open  string
	since time.Time
}

type smokeMark struct {
	name string
	d    time.Duration
}

func newSmokeTrace() *smokeTrace { return &smokeTrace{} }

func (t *smokeTrace) finish(name string, d time.Duration) {
	if t == nil {
		return
	}
	t.marks = append(t.marks, smokeMark{name: name, d: d})
	if t.open == name {
		t.open = ""
	}
}

func (t *smokeTrace) String() string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	for _, m := range t.marks {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%s", m.name, m.d.Round(time.Millisecond))
	}
	if t.open != "" {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%s", t.open, time.Since(t.since).Round(time.Millisecond))
	}
	return b.String()
}

func smokeStageDone() {}

// smokeStage times one step of a smoke connection. The returned function
// records the duration; calling it more than once records the first one.
// A nil trace makes it a no-op, so the normal connection path pays nothing.
func (c *Conn) smokeStage(name string) func() {
	if c == nil {
		return smokeStageDone
	}
	c.beginConnectionPhase(name, true)
	if c.trace == nil {
		return func() { c.endConnectionPhase(name, nil) }
	}
	start := time.Now()
	c.trace.open = name
	c.trace.since = start
	done := false
	return func() {
		if done {
			return
		}
		done = true
		c.trace.finish(name, time.Since(start))
		c.endConnectionPhase(name, nil)
	}
}

// finishSmoke logs the stage clock and attaches it to a failure. A success
// stays a nil error; the log is the record that the attempt finished.
func (c *Conn) finishSmoke(err error) error {
	err = safeConnectionError(err)
	if c == nil || c.trace == nil {
		return err
	}
	line := c.trace.String()
	if c.log != nil {
		c.log.Info("smoke stages", "stages", line, "err", err)
	}
	if err == nil || line == "" {
		return err
	}
	return fmt.Errorf("smoke %s: %w", line, err)
}
