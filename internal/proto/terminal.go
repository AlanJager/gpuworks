package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Terminal framing. A StreamTerminal stream carries length-prefixed frames
// after the one-line StreamHeader. Both directions use the same 3-byte header
// (type + big-endian payload length), so one codec serves server and agent.
//
//	+--------+----------------+------------------+
//	| type   | length (2B BE) | payload          |
//	| 1 byte |                | <= 65535 bytes   |
//	+--------+----------------+------------------+
//
// yamux already guarantees a reliable, ordered byte stream, so frames carry no
// sequence numbers or checksums.
const (
	// TermData carries raw bytes: stdin server->agent, stdout agent->server.
	TermData byte = 0x00
	// TermResize is server->agent: EncodeResize payload (rows, cols).
	TermResize byte = 0x01
	// TermExit is agent->server: EncodeExit payload (code + optional reason).
	TermExit byte = 0x02
	// TermInfo is agent->server, sent once right after the pty starts: a JSON
	// TermSessionInfo. The agent's privileges vary by install path (systemd runs
	// it as root, the nohup fallback as whoever installed it), so the UI reports
	// what the shell actually is rather than assuming root.
	TermInfo byte = 0x03
)

// TermSessionInfo is the TermInfo payload.
type TermSessionInfo struct {
	User  string `json:"user"`
	UID   int    `json:"uid"`
	Shell string `json:"shell"`
	Cwd   string `json:"cwd"`
	Host  string `json:"host,omitempty"`
}

// TermHeaderSize is the fixed per-frame header length.
const TermHeaderSize = 3

// TermMaxPayload is the largest payload a single frame can carry, bounded by
// the 2-byte length field.
const TermMaxPayload = 65535

// Errors returned by the terminal codec.
var (
	ErrTermPayloadTooLarge = errors.New("terminal frame payload exceeds 65535 bytes")
	ErrTermShortPayload    = errors.New("terminal frame payload too short")
)

// TermFrame is one decoded terminal frame.
type TermFrame struct {
	Type    byte
	Payload []byte
}

// EncodeTermFrame returns a single encoded frame. Payloads longer than
// TermMaxPayload are rejected; use WriteTermData to chunk large writes.
func EncodeTermFrame(typ byte, payload []byte) ([]byte, error) {
	if len(payload) > TermMaxPayload {
		return nil, ErrTermPayloadTooLarge
	}
	buf := make([]byte, TermHeaderSize+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint16(buf[1:3], uint16(len(payload)))
	copy(buf[TermHeaderSize:], payload)
	return buf, nil
}

// WriteTermFrame writes one frame to w.
func WriteTermFrame(w io.Writer, typ byte, payload []byte) error {
	buf, err := EncodeTermFrame(typ, payload)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// WriteTermData writes p as one or more TermData frames, splitting at
// TermMaxPayload so callers never have to think about the length limit.
func WriteTermData(w io.Writer, p []byte) error {
	for {
		chunk := p
		if len(chunk) > TermMaxPayload {
			chunk = chunk[:TermMaxPayload]
		}
		if err := WriteTermFrame(w, TermData, chunk); err != nil {
			return err
		}
		p = p[len(chunk):]
		if len(p) == 0 {
			return nil
		}
	}
}

// TermFrameReader decodes frames from a stream.
type TermFrameReader struct {
	r   io.Reader
	hdr [TermHeaderSize]byte
	buf []byte
}

// NewTermFrameReader wraps r. The reader performs its own full reads, so r
// need not be buffered.
func NewTermFrameReader(r io.Reader) *TermFrameReader {
	return &TermFrameReader{r: r}
}

// Next reads the next frame. The returned Payload aliases an internal buffer
// and stays valid only until the following Next call (same contract as
// bufio.Scanner.Bytes); copy it if you need to retain it.
//
// A truncated frame yields io.ErrUnexpectedEOF. A clean end of stream between
// frames yields io.EOF.
func (fr *TermFrameReader) Next() (TermFrame, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		// ReadFull reports EOF only when nothing was read, which is the clean
		// inter-frame end of stream; a partial header is a truncated frame.
		return TermFrame{}, err
	}
	n := int(binary.BigEndian.Uint16(fr.hdr[1:3]))
	if cap(fr.buf) < n {
		fr.buf = make([]byte, n)
	}
	payload := fr.buf[:n]
	if n > 0 {
		if _, err := io.ReadFull(fr.r, payload); err != nil {
			if err == io.EOF {
				return TermFrame{}, io.ErrUnexpectedEOF
			}
			return TermFrame{}, err
		}
	}
	return TermFrame{Type: fr.hdr[0], Payload: payload}, nil
}

// EncodeResize builds a TermResize payload.
func EncodeResize(rows, cols uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], rows)
	binary.BigEndian.PutUint16(b[2:4], cols)
	return b
}

// DecodeResize parses a TermResize payload.
func DecodeResize(payload []byte) (rows, cols uint16, err error) {
	if len(payload) < 4 {
		return 0, 0, ErrTermShortPayload
	}
	return binary.BigEndian.Uint16(payload[0:2]), binary.BigEndian.Uint16(payload[2:4]), nil
}

// EncodeExit builds a TermExit payload: a 4-byte big-endian exit code followed
// by an optional human-readable reason (idle timeout, killed, and so on).
func EncodeExit(code int32, reason string) []byte {
	b := make([]byte, 4, 4+len(reason))
	binary.BigEndian.PutUint32(b[0:4], uint32(code))
	return append(b, reason...)
}

// DecodeExit parses a TermExit payload.
func DecodeExit(payload []byte) (code int32, reason string, err error) {
	if len(payload) < 4 {
		return 0, "", ErrTermShortPayload
	}
	return int32(binary.BigEndian.Uint32(payload[0:4])), string(payload[4:]), nil
}

// TermExitText renders an exit frame for the operator.
func TermExitText(code int32, reason string) string {
	if reason != "" {
		return fmt.Sprintf("process exited (code %d): %s", code, reason)
	}
	return fmt.Sprintf("process exited (code %d)", code)
}
