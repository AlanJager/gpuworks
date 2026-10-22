package tracecontext

import (
	"strings"
	"testing"
	"time"
)

const (
	validTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	validSpan  = "00f067aa0ba902b7"
)

func header(version, trace, span, flags string) string {
	return version + "-" + trace + "-" + span + "-" + flags
}

func TestParseTraceparentAcceptsTheSpecificationExample(t *testing.T) {
	p, ok := ParseTraceparent([]string{header("00", validTrace, validSpan, "01")}, []string{"rojo=00f067aa0ba902b7", "congo=t61rcWkgMzE"})
	if !ok {
		t.Fatal("valid traceparent refused")
	}
	if p.Trace.String() != validTrace || p.Span.String() != validSpan {
		t.Fatalf("ids = %s/%s", p.Trace, p.Span)
	}
	if p.Flags != FlagSampled {
		t.Fatalf("flags = %x, want sampled", p.Flags)
	}
	if p.State != "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE" {
		t.Fatalf("tracestate = %q", p.State)
	}
}

func TestParseTraceparentRefusesWhatTheSpecificationRefuses(t *testing.T) {
	zeroTrace := strings.Repeat("0", 32)
	zeroSpan := strings.Repeat("0", 16)
	cases := map[string][]string{
		"none":                   nil,
		"two headers":            {header("00", validTrace, validSpan, "01"), header("00", validTrace, validSpan, "01")},
		"empty":                  {""},
		"short":                  {header("00", validTrace, validSpan, "0")},
		"version ff":             {header("ff", validTrace, validSpan, "01")},
		"version 00 with suffix": {header("00", validTrace, validSpan, "01") + "-extra"},
		"later version glued":    {header("01", validTrace, validSpan, "01") + "x"},
		"uppercase trace":        {header("00", strings.ToUpper(validTrace), validSpan, "01")},
		"uppercase version":      {header("0A", validTrace, validSpan, "01")},
		"zero trace":             {header("00", zeroTrace, validSpan, "01")},
		"zero span":              {header("00", validTrace, zeroSpan, "01")},
		"non-hex flags":          {header("00", validTrace, validSpan, "0g")},
		"wrong separator":        {"00_" + validTrace + "-" + validSpan + "-01"},
		"non-hex trace":          {header("00", "4bf92f3577b34da6a3ce929d0e0e473z", validSpan, "01")},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if p, ok := ParseTraceparent(values, []string{"a=b"}); ok {
				t.Fatalf("accepted %q as %+v", values, p)
			}
		})
	}
}

func TestParseTraceparentReadsALaterVersionByItsFirstFields(t *testing.T) {
	p, ok := ParseTraceparent([]string{header("cc", validTrace, validSpan, "09") + "-what-the-future-holds"}, nil)
	if !ok {
		t.Fatal("future version refused")
	}
	// 0x09 = sampled | an unknown bit; only the known bits survive.
	if p.Flags != FlagSampled {
		t.Fatalf("flags = %x", p.Flags)
	}
	if p.State != "" {
		t.Fatalf("tracestate = %q, want none", p.State)
	}
}

func TestParseTraceparentDropsAnOverlongTracestate(t *testing.T) {
	p, ok := ParseTraceparent([]string{header("00", validTrace, validSpan, "00")}, []string{strings.Repeat("k=v,", 200)})
	if !ok || p.State != "" {
		t.Fatalf("ok=%v state len=%d, want the parent without a state", ok, len(p.State))
	}
}

func TestFormatRoundTrips(t *testing.T) {
	p, _ := ParseTraceparent([]string{header("00", validTrace, validSpan, "03")}, nil)
	if got := Format(p.Trace, p.Span, p.Flags); got != header("00", validTrace, validSpan, "03") {
		t.Fatalf("Format = %q", got)
	}
	if got := Format(p.Trace, p.Span, 0xff); !strings.HasSuffix(got, "-03") {
		t.Fatalf("unknown flag bits forwarded: %q", got)
	}
}

func TestNewTraceIDCarriesItsMillisecond(t *testing.T) {
	now := time.UnixMilli(1760500000123)
	a, b := NewTraceID(now), NewTraceID(now)
	if a == b {
		t.Fatal("two ids minted alike")
	}
	if got := a.Time(); !got.Equal(now) {
		t.Fatalf("Time = %v, want %v", got, now)
	}
	if a.String()[:12] != "0199e5fa257b" {
		t.Fatalf("prefix = %s", a.String()[:12])
	}
	later := NewTraceID(now.Add(time.Millisecond))
	if later.String()[:12] <= a.String()[:12] {
		t.Fatal("a later id does not sort after an earlier one")
	}
}

func TestParseTraceIDAcceptsWhatAPersonPastes(t *testing.T) {
	id, ok := ParseTraceID("  " + strings.ToUpper(validTrace) + "\n")
	if !ok || id.String() != validTrace {
		t.Fatalf("got %s %v", id, ok)
	}
	for _, bad := range []string{"", validTrace[:31], validTrace + "0", strings.Repeat("0", 32), "zz" + validTrace[2:]} {
		if _, ok := ParseTraceID(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestZeroIDsPrintEmpty(t *testing.T) {
	if (TraceID{}).String() != "" || (SpanID{}).String() != "" {
		t.Fatal("zero ids print digits")
	}
	if NewSpanID().IsZero() {
		t.Fatal("minted a zero span id")
	}
}

func TestSampledByRatioUsesTheRandomTail(t *testing.T) {
	var low, high TraceID
	high[9] = 0xff // tail 0xff000000000000: above any ratio below ~0.996
	low[15] = 1
	if !SampledByRatio(low, 0.01) || SampledByRatio(high, 0.5) {
		t.Fatal("threshold compares the wrong bytes")
	}
	// The time prefix must not influence the decision.
	high[0], high[5] = 0xff, 0xff
	low[0] = 0xff
	if !SampledByRatio(low, 0.01) || SampledByRatio(high, 0.5) {
		t.Fatal("prefix bytes changed the decision")
	}
	if SampledByRatio(low, 0) || !SampledByRatio(high, 1) {
		t.Fatal("bounds wrong")
	}
	kept := 0
	now := time.Now()
	for i := 0; i < 20000; i++ {
		if SampledByRatio(NewTraceID(now), 0.25) {
			kept++
		}
	}
	if kept < 4500 || kept > 5500 {
		t.Fatalf("kept %d of 20000 at 0.25", kept)
	}
}

func TestBeginContinuesOrStartsATrace(t *testing.T) {
	parent, _ := ParseTraceparent([]string{header("00", validTrace, validSpan, "00")}, []string{"a=b"})
	child := Begin(parent, true, time.Now(), func(TraceID) bool { return true })
	if child.Trace != parent.Trace || child.Parent != parent.Span || child.ID.IsZero() || child.ID == parent.Span {
		t.Fatalf("child = %+v", child)
	}
	if child.Sampled() {
		t.Fatal("the root sampler overrode the parent's decision")
	}
	if child.State != "a=b" {
		t.Fatalf("state = %q", child.State)
	}
	if got := child.Traceparent(); got != header("00", validTrace, child.ID.String(), "00") {
		t.Fatalf("traceparent = %q", got)
	}

	now := time.UnixMilli(1760500000000)
	root := Begin(Parent{}, false, now, func(TraceID) bool { return true })
	if !root.Parent.IsZero() || !root.Trace.Time().Equal(now) || !root.Sampled() || root.Flags&FlagRandom == 0 {
		t.Fatalf("root = %+v", root)
	}
	if unsampled := Begin(Parent{}, false, now, nil); unsampled.Sampled() || !strings.HasSuffix(unsampled.Traceparent(), "-02") {
		t.Fatalf("root without a sampler = %q", unsampled.Traceparent())
	}
}

func BenchmarkBeginAndFormat(b *testing.B) {
	values := []string{header("00", validTrace, validSpan, "01")}
	for i := 0; i < b.N; i++ {
		p, ok := ParseTraceparent(values, nil)
		_ = Begin(p, ok, time.Now(), nil).Traceparent()
	}
}
