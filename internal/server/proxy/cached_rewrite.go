package proxy

import (
	"bytes"
	"io"
	"strconv"
)

// cachedRewriter puts a PD request's prompt-cache figure into the decode
// instance's reply (ADR 0047). The decoder counts the KV it pulled from the
// prefill instance as cached, so its cached_tokens is the whole prompt; the
// prompt the cache really saved is what the prefill instance reported. Every
// "<key>": <digits> in the reply - the usage object's cache figure, once per
// usage the engine writes - gets the prefill instance's number instead.
//
// The rewrite is in place and keeps the length: the new digits are padded
// with spaces, which JSON allows after a number, so a declared
// Content-Length and every byte count still hold. A figure that would need
// more digits than the decoder wrote is left alone (the decoder's figure is
// the prompt length, which the prefill instance's never exceeds). The key
// cannot occur inside generated text, where its quotes would be escaped.
//
// It sits under the gateway's response observer, so the request record, the
// gateway's counters and the exported span read the same figure the client
// gets. Bytes pass through as they arrive; only a read that ends inside a
// candidate figure holds that tail (at most cachedHoldMax bytes) until the
// next read completes it.
type cachedRewriter struct {
	src   io.ReadCloser
	key   []byte // the quoted key, e.g. "cached_tokens"
	value []byte
	// anchor is the index in key of the byte searched for first: a rare one
	// in replies, so a candidate is seldom checked in vain (cachedAnchor).
	anchor int
	// carry is the held tail of the last read, not yet final; out is final
	// bytes a short read could not take; scratch is read into when the
	// caller's buffer is too short to hold carry and still make progress.
	carry, out, scratch []byte
	// err is the source's error, returned once everything is out.
	err error
	// rewritten counts figures replaced; tests read it.
	rewritten int
}

// cachedHoldMax bounds a held tail: the key, a colon, some spaces and a
// count fit well within it, and a longer run is not a figure to rewrite.
const cachedHoldMax = 64

func newCachedRewriter(src io.ReadCloser, key string, cached uint64) *cachedRewriter {
	quoted := []byte(strconv.Quote(key))
	return &cachedRewriter{src: src, key: quoted, value: strconv.AppendUint(nil, cached, 10), anchor: cachedAnchor(quoted)}
}

// cachedAnchor picks the byte of the key to look for first. A quote is in
// every JSON string, so searching for the key's leading quote checks a
// candidate every few bytes; "k" is about 1% of vLLM's stream bytes (once
// per event, in "chunk"), which makes the search run at memchr speed.
func cachedAnchor(key []byte) int {
	if i := bytes.LastIndexByte(key, 'k'); i > 0 {
		return i
	}
	return 0
}

// Read rewrites in the caller's buffer: the carried tail goes first, the
// source fills the rest, and the final part is returned.
func (c *cachedRewriter) Read(p []byte) (int, error) {
	if len(c.out) > 0 {
		n := copy(p, c.out)
		c.out = c.out[n:]
		return n, nil
	}
	for {
		if c.err != nil {
			return 0, c.err
		}
		buf := p
		short := len(p) <= len(c.carry)+cachedHoldMax
		if short {
			if c.scratch == nil {
				c.scratch = make([]byte, 4<<10)
			}
			buf = c.scratch
		}
		held := copy(buf, c.carry)
		c.carry = c.carry[:0]
		n, err := c.src.Read(buf[held:])
		data := buf[:held+n]
		keep := c.rewrite(data, err != nil)
		c.carry = append(c.carry, data[keep:]...)
		c.err = err
		if keep == 0 {
			continue
		}
		if short {
			m := copy(p, data[:keep])
			c.out = append(c.out[:0], data[m:keep]...)
			return m, nil
		}
		return keep, nil
	}
}

// rewrite replaces the figures in data and returns how many bytes are
// final; data[keep:] may be the start of a figure the next read completes.
// At the end of the stream everything is final.
func (c *cachedRewriter) rewrite(data []byte, end bool) int {
	from := 0
	for search := c.anchor; search < len(data); {
		i := bytes.IndexByte(data[search:], c.key[c.anchor])
		if i < 0 {
			break
		}
		at := search + i - c.anchor
		if at+len(c.key) > len(data) || !bytes.Equal(data[at:at+len(c.key)], c.key) {
			// Not the key, or the key cut at the end (the tail check below).
			search += i + 1
			continue
		}
		next, complete := c.rewriteAt(data, at)
		if !complete && !end && len(data)-at <= cachedHoldMax {
			return at
		}
		from = next
		search = from + c.anchor
	}
	if end {
		return len(data)
	}
	// A tail that could still become the key.
	for k := min(len(c.key)-1, len(data)); k > 0; k-- {
		if bytes.Equal(data[len(data)-k:], c.key[:k]) {
			return len(data) - k
		}
	}
	return len(data)
}

// rewriteAt rewrites the figure whose key starts at at. complete is false
// when data ends before the figure does; next is where scanning resumes.
func (c *cachedRewriter) rewriteAt(data []byte, at int) (next int, complete bool) {
	j := at + len(c.key)
	j = skipJSONSpace(data, j)
	if j == len(data) {
		return len(data), false
	}
	if data[j] != ':' {
		return at + len(c.key), true
	}
	j = skipJSONSpace(data, j+1)
	start := j
	for j < len(data) && data[j] >= '0' && data[j] <= '9' {
		j++
	}
	if j == len(data) {
		return len(data), false
	}
	// A count is whole digits ending the value; anything else (a fraction,
	// an exponent) is not one to rewrite.
	if j == start || len(c.value) > j-start || !endsJSONValue(data[j]) {
		return j, true
	}
	n := copy(data[start:j], c.value)
	for k := start + n; k < j; k++ {
		data[k] = ' '
	}
	c.rewritten++
	return j, true
}

func endsJSONValue(b byte) bool {
	return b == ',' || b == '}' || b == ']' || b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func skipJSONSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

func (c *cachedRewriter) Close() error { return c.src.Close() }
