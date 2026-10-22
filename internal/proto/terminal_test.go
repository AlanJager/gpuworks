package proto

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestTermFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		typ     byte
		payload []byte
	}{
		{"data", TermData, []byte("ls -la\n")},
		{"empty payload", TermData, nil},
		{"resize", TermResize, EncodeResize(40, 120)},
		{"exit", TermExit, EncodeExit(137, "killed")},
		{"max payload", TermData, bytes.Repeat([]byte("x"), TermMaxPayload)},
		{"binary safe", TermData, []byte{0x00, 0x01, 0xff, 0x0a, 0x1b, 0x5b}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteTermFrame(&buf, tc.typ, tc.payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got, want := buf.Len(), TermHeaderSize+len(tc.payload); got != want {
				t.Fatalf("encoded length = %d, want %d", got, want)
			}
			f, err := NewTermFrameReader(&buf).Next()
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if f.Type != tc.typ {
				t.Errorf("type = %#x, want %#x", f.Type, tc.typ)
			}
			if !bytes.Equal(f.Payload, tc.payload) {
				t.Errorf("payload = %q, want %q", f.Payload, tc.payload)
			}
		})
	}
}

func TestTermFrameReaderSequence(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTermFrame(&buf, TermData, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := WriteTermFrame(&buf, TermResize, EncodeResize(24, 80)); err != nil {
		t.Fatal(err)
	}
	if err := WriteTermFrame(&buf, TermData, []byte("bb")); err != nil {
		t.Fatal(err)
	}

	fr := NewTermFrameReader(&buf)
	want := []struct {
		typ     byte
		payload string
	}{
		{TermData, "a"},
		{TermResize, string(EncodeResize(24, 80))},
		{TermData, "bb"},
	}
	for i, w := range want {
		f, err := fr.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.Type != w.typ || string(f.Payload) != w.payload {
			t.Errorf("frame %d = (%#x,%q), want (%#x,%q)", i, f.Type, f.Payload, w.typ, w.payload)
		}
	}
	if _, err := fr.Next(); err != io.EOF {
		t.Errorf("end of stream: err = %v, want io.EOF", err)
	}
}

// A payload buffer is reused across Next calls, so a longer frame following a
// shorter one must not leak the previous frame's tail.
func TestTermFrameReaderReusesBufferSafely(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTermFrame(&buf, TermData, []byte("xxxxxxxx")); err != nil {
		t.Fatal(err)
	}
	if err := WriteTermFrame(&buf, TermData, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	fr := NewTermFrameReader(&buf)
	if _, err := fr.Next(); err != nil {
		t.Fatal(err)
	}
	f, err := fr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Payload) != "ab" {
		t.Errorf("payload = %q, want %q", f.Payload, "ab")
	}
}

func TestTermFrameTruncated(t *testing.T) {
	full := func(t *testing.T) []byte {
		t.Helper()
		b, err := EncodeTermFrame(TermData, []byte("hello"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("partial header", func(t *testing.T) {
		_, err := NewTermFrameReader(bytes.NewReader(full(t)[:2])).Next()
		if err != io.ErrUnexpectedEOF {
			t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("header without payload", func(t *testing.T) {
		// A length field promising bytes that never arrive is truncation, not a
		// clean end of stream.
		_, err := NewTermFrameReader(bytes.NewReader(full(t)[:TermHeaderSize])).Next()
		if err != io.ErrUnexpectedEOF {
			t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("partial payload", func(t *testing.T) {
		_, err := NewTermFrameReader(bytes.NewReader(full(t)[:TermHeaderSize+2])).Next()
		if err != io.ErrUnexpectedEOF {
			t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("empty stream is clean EOF", func(t *testing.T) {
		_, err := NewTermFrameReader(bytes.NewReader(nil)).Next()
		if err != io.EOF {
			t.Errorf("err = %v, want io.EOF", err)
		}
	})
}

func TestEncodeTermFrameRejectsOversizePayload(t *testing.T) {
	if _, err := EncodeTermFrame(TermData, bytes.Repeat([]byte("x"), TermMaxPayload+1)); err != ErrTermPayloadTooLarge {
		t.Errorf("err = %v, want ErrTermPayloadTooLarge", err)
	}
}

// WriteTermData is the caller-facing path for arbitrary-length output; it must
// split rather than fail when a pty read exceeds the frame limit.
func TestWriteTermDataChunksLargeWrites(t *testing.T) {
	payload := bytes.Repeat([]byte("y"), TermMaxPayload*2+5)
	var buf bytes.Buffer
	if err := WriteTermData(&buf, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	fr := NewTermFrameReader(&buf)
	var got []byte
	frames := 0
	for {
		f, err := fr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if f.Type != TermData {
			t.Fatalf("frame type = %#x, want TermData", f.Type)
		}
		got = append(got, f.Payload...)
		frames++
	}
	if frames != 3 {
		t.Errorf("frames = %d, want 3", frames)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("reassembled %d bytes, want %d", len(got), len(payload))
	}
}

func TestWriteTermDataEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTermData(&buf, nil); err != nil {
		t.Fatal(err)
	}
	// An empty write still emits one frame; a zero-length data frame is how the
	// agent signals "no output" without closing the stream.
	if buf.Len() != TermHeaderSize {
		t.Errorf("encoded length = %d, want %d", buf.Len(), TermHeaderSize)
	}
}

func TestResizeCodec(t *testing.T) {
	rows, cols, err := DecodeResize(EncodeResize(50, 200))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rows != 50 || cols != 200 {
		t.Errorf("got %dx%d, want 50x200", rows, cols)
	}

	// Extra trailing bytes are tolerated so the frame can grow later.
	if _, _, err := DecodeResize([]byte{0, 24, 0, 80, 0xff}); err != nil {
		t.Errorf("trailing bytes: %v", err)
	}
	for _, short := range [][]byte{nil, {0}, {0, 24}, {0, 24, 0}} {
		if _, _, err := DecodeResize(short); err != ErrTermShortPayload {
			t.Errorf("DecodeResize(%v) err = %v, want ErrTermShortPayload", short, err)
		}
	}
}

func TestExitCodec(t *testing.T) {
	code, reason, err := DecodeExit(EncodeExit(137, "idle timeout"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if code != 137 || reason != "idle timeout" {
		t.Errorf("got (%d,%q), want (137,%q)", code, reason, "idle timeout")
	}

	code, reason, err = DecodeExit(EncodeExit(0, ""))
	if err != nil || code != 0 || reason != "" {
		t.Errorf("got (%d,%q,%v), want (0,\"\",nil)", code, reason, err)
	}

	// Non-zero exits are the interesting case for operators; -1 (no wait status
	// available) must survive the uint32 round trip.
	if code, _, err = DecodeExit(EncodeExit(-1, "")); err != nil || code != -1 {
		t.Errorf("got (%d,%v), want (-1,nil)", code, err)
	}

	if _, _, err := DecodeExit([]byte{0, 0}); err != ErrTermShortPayload {
		t.Errorf("err = %v, want ErrTermShortPayload", err)
	}
}

func TestTermExitText(t *testing.T) {
	if got := TermExitText(137, "idle timeout"); !strings.Contains(got, "137") || !strings.Contains(got, "idle timeout") {
		t.Errorf("TermExitText = %q, want code and reason", got)
	}
	if got := TermExitText(0, ""); strings.Contains(got, ":") {
		t.Errorf("TermExitText = %q, want no reason clause", got)
	}
}

// Unknown frame types must decode cleanly so a newer peer can add frame types
// without breaking this build; the dispatcher decides to ignore them.
func TestUnknownFrameTypeDecodes(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTermFrame(&buf, 0x7f, []byte("future")); err != nil {
		t.Fatal(err)
	}
	f, err := NewTermFrameReader(&buf).Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if f.Type != 0x7f || string(f.Payload) != "future" {
		t.Errorf("got (%#x,%q)", f.Type, f.Payload)
	}
}
