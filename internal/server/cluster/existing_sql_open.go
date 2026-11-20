package cluster

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

const existingReadOpenRequest = 20

// existingReadSQLConnection adapts only the private SDK connector's OPEN.
// It retains the original socket's deadlines and Close ownership. There is no
// fallback to a creating OPEN when the native extension is unavailable.
type existingReadSQLConnection struct {
	net.Conn
	writeMu   sync.Mutex
	readMu    sync.Mutex
	errMu     sync.Mutex
	err       error
	header    []byte
	body      []byte
	left      int
	handshake bool
	opened    bool
	opening   bool
	awaitOpen atomic.Bool
	closed    atomic.Bool
	response  []byte
}

func wrapExistingReadSQLConnection(conn net.Conn) net.Conn {
	return &existingReadSQLConnection{Conn: conn}
}

func (c *existingReadSQLConnection) failure(err error) error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if err != nil && c.err == nil {
		c.err = err
	}
	return c.err
}

func (c *existingReadSQLConnection) send(p []byte) error {
	n, err := c.Conn.Write(p)
	if n != len(p) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	return c.failure(err)
}

func (c *existingReadSQLConnection) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	if err := c.failure(nil); err != nil {
		return 0, err
	}
	accepted := 0
	for len(p) > 0 {
		if c.left == 0 {
			n := min(8-len(c.header), len(p))
			c.header = append(c.header, p[:n]...)
			p, accepted = p[n:], accepted+n
			if len(c.header) < 8 {
				continue
			}
			if !c.handshake {
				if binary.LittleEndian.Uint64(c.header) != 1 {
					return accepted, c.failure(errors.New("existing SQL requires protocol v1"))
				}
				if err := c.send(c.header); err != nil {
					return accepted, err
				}
				c.handshake, c.header = true, nil
				continue
			}
			words := binary.LittleEndian.Uint32(c.header)
			kind, schema := c.header[4], c.header[5]
			if words == 0 || words > (16<<20)/8 || binary.LittleEndian.Uint16(c.header[6:]) != 0 {
				return accepted, c.failure(errors.New("invalid existing SQL request header"))
			}
			c.left = int(words) * 8
			if kind == 3 {
				if c.opened || schema != 0 || c.left > 536 {
					return accepted, c.failure(errors.New("invalid or repeated existing SQL OPEN"))
				}
				c.opening = true
				continue
			}
			if !existingReadRequestAllowed(kind, schema, c.opened) {
				return accepted, c.failure(fmt.Errorf("unsupported existing SQL request %d schema %d", kind, schema))
			}
			if err := c.send(c.header); err != nil {
				return accepted, err
			}
			c.header = nil
		}
		n := min(c.left, len(p))
		if c.opening {
			c.body = append(c.body, p[:n]...)
		} else if err := c.send(p[:n]); err != nil {
			return accepted + n, err
		}
		p, accepted, c.left = p[n:], accepted+n, c.left-n
		if c.opening && c.left == 0 {
			if err := validateExistingReadOpen(c.body); err != nil {
				return accepted, c.failure(err)
			}
			c.header[4] = existingReadOpenRequest
			// The SDK serializes each request and its response. Publish before
			// sending so its following Read validates the successful DB reply.
			c.awaitOpen.Store(true)
			if err := c.send(append(c.header, c.body...)); err != nil {
				return accepted, err
			}
			c.opened, c.opening, c.header, c.body = true, false, nil, nil
		}
	}
	return accepted, nil
}

func existingReadRequestAllowed(kind, schema byte, opened bool) bool {
	if !opened {
		return schema == 0 && (kind == 0 || kind == 1 || kind == 2 || kind == 16 || kind == 18)
	}
	return kind >= 4 && kind <= 10 && (schema == 0 || schema == 1 && (kind == 5 || kind == 6 || kind == 8 || kind == 9))
}

func existingReadOpenString(body []byte) (string, []byte, error) {
	end := bytes.IndexByte(body, 0)
	if end < 0 {
		return "", nil, errors.New("unterminated existing SQL OPEN string")
	}
	aligned := (end + 8) &^ 7
	if aligned > len(body) || !bytes.Equal(body[end:aligned], make([]byte, aligned-end)) {
		return "", nil, errors.New("invalid existing SQL OPEN string padding")
	}
	return string(body[:end]), body[aligned:], nil
}

func validateExistingReadName(name string) error {
	if len(name) == 0 || len(name) > 511 || !utf8.ValidString(name) || name == "." || name == ".." || strings.ContainsAny(name, "/\\:?") {
		return errors.New("invalid existing SQL database filename")
	}
	for _, b := range []byte(name) {
		if b < 32 || b == 127 {
			return errors.New("invalid existing SQL database filename")
		}
	}
	return nil
}

func validateExistingReadOpen(body []byte) error {
	name, rest, err := existingReadOpenString(body)
	if err != nil {
		return err
	}
	if err := validateExistingReadName(name); err != nil {
		return err
	}
	if len(rest) < 8 || binary.LittleEndian.Uint64(rest) != 0 {
		return errors.New("existing SQL OPEN requires zero flags")
	}
	vfs, tail, err := existingReadOpenString(rest[8:])
	if err != nil || vfs != "volatile" || len(tail) != 0 {
		return errors.New("existing SQL OPEN requires exactly volatile VFS")
	}
	return nil
}

func (c *existingReadSQLConnection) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	if err := c.failure(nil); err != nil {
		return 0, err
	}
	if len(c.response) == 0 && c.awaitOpen.Load() {
		var header [8]byte
		if _, err := io.ReadFull(c.Conn, header[:]); err != nil {
			return 0, c.failure(err)
		}
		words := binary.LittleEndian.Uint32(header[:])
		if header[5] != 0 || binary.LittleEndian.Uint16(header[6:]) != 0 || words == 0 || words > (64<<10)/8 {
			return 0, c.failure(errors.New("invalid existing SQL OPEN response header"))
		}
		c.response = append(c.response, header[:]...)
		switch header[4] {
		case 0: // Preserve native rejection for the SDK's failure decoder.
			if words < 2 {
				return 0, c.failure(errors.New("invalid existing SQL failure response size"))
			}
			body := make([]byte, int(words)*8)
			if _, err := io.ReadFull(c.Conn, body); err != nil {
				return 0, c.failure(err)
			}
			if _, tail, err := existingReadOpenString(body[8:]); err != nil || len(tail) != 0 {
				return 0, c.failure(errors.New("invalid existing SQL failure response text"))
			}
			c.response = append(c.response, body...)
		case 4:
			var id [8]byte
			if words != 1 {
				return 0, c.failure(errors.New("invalid existing SQL database response size"))
			}
			if _, err := io.ReadFull(c.Conn, id[:]); err != nil {
				return 0, c.failure(err)
			}
			if binary.LittleEndian.Uint64(id[:]) != 0 {
				return 0, c.failure(errors.New("existing SQL database response requires id zero"))
			}
			c.response = append(c.response, id[:]...)
		default:
			return 0, c.failure(errors.New("unsupported existing SQL OPEN response"))
		}
		c.awaitOpen.Store(false)
	}
	if len(c.response) > 0 {
		n := copy(p, c.response)
		c.response = c.response[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

func (c *existingReadSQLConnection) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}
